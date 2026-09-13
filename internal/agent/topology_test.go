package agent

// 阶段 9 的拓扑与等待策略测试（13.11 测试规格）：
//
//	三层 fork（根 → 子 → 孙）；孙到顶被拒；spawn_batch + await 策略；
//	Watchdog 终止 starving 子 → 框架代报 → 父收到 failed。
//
// 环境复用 fork_test.go 的 forkFactory——它已经装配了 report/机械检查/
// 信箱链，阶段 9 只把 Mailbox 与深度口径扩进去。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/spawner"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/watchdog"
	"github.com/RobiNexy/Marl/internal/wire"
)

// batchCall 构造 spawn_batch 调用（items + await）。
// grandFileName 从孙任务文本里解析孙的输出文件名（测试协议：任务
// 文本形如 "…写 src/auth/a_grand_out.txt …"——解析只是测试的取巧，
// 领域上没人解析任务文本；孙的真实产物路径来自它自己的工具参数）。
func grandFileOf(task string) string { return grandFileName(task) }

func grandFileName(task string) string {
	for _, tok := range strings.Fields(task) {
		// CJK 任务文本里文件名常紧跟国的说明短语（"…txt（孙任务…）"）——
		// 先截断再取后缀（真机踩过的解析坑）。
		if i := strings.Index(tok, "（"); i >= 0 {
			tok = tok[:i]
		}
		if strings.HasSuffix(tok, ".txt") {
			return filepath.Base(tok)
		}
	}
	return "grand_out.txt"
}

func batchCall(items []map[string]any, await string, n int) types.ToolCall {
	args := map[string]any{"items": items}
	if await != "" {
		args["await"] = await
	}
	if n > 0 {
		args["n"] = n
	}
	b, _ := json.Marshal(args)
	return types.ToolCall{ID: "call-batch", Name: "spawn_batch", Arguments: b}
}

// itemOf 是批量单项的小构造（writable 显式给出——[阶段 12 修正] schema
// 必填后，省略 = BAD_ARGS，"继承"语义本就不存在）。
func itemOf(task string) map[string]any {
	return map[string]any{"profile_id": "coder", "task": task, "writable_paths": []string{"src/auth/**"}}
}

// TestThreeLevelFork：根 spawn_batch 2 子（await all）；每个子再 fork 1
// 个孙各自写文件；全部 report 回流：孙 → 子 →（sub_task_result）→ 根。
// 13.11 的"根 fork 3 个子"判据的 2+2 收敛形态（孙的 id 由 Spawner 分配，
// 文件名带上孙 id 由脚本按 plan 命名）。
func TestThreeLevelFork(t *testing.T) {
	ctx := context.Background()
	rootAgent, _, ff, _, wsRoot := setupForkMulti(t)

	// 子脚本：fork 1 个孙（写孙专属文件）→ report success。
	// 孙脚本：file_write（在子传下来的 writable 范围内）→ report success。
	// depth==2（子，Part 14.6 记账后）：fork 一个孙（任务文本携带孙专属
	// 的文件名）→ 等孙 report → 自己 report 到根。
	// depth>=3（孙）：写孙专属文件（plan.Task 里的文件名）→ report。
	ff.scriptFn = func(plan *spawner.ChildPlan) []*wire.WireTurn {
		name := grandFileName(plan.Task)
		if plan.Depth >= 3 {
			return []*wire.WireTurn{
				toolCallTurn(mkCallID("file_write", "fw-"+string(plan.ID), map[string]any{
					"path": "src/auth/" + name, "content": "grand " + string(plan.ID) + " here\n",
				})),
				toolCallTurn(reportCall("success", "grand report 已提交（写了 "+name+"）。")),
			}
		}
		return []*wire.WireTurn{
			toolCallTurn(spawnCall("coder",
				fmt.Sprintf("在 %s 写 %s（孙任务，内容为占位文本）", "src/auth", name),
				[]string{"src/auth/**"}, nil)),
			toolCallTurn(reportCall("success", "孙的结果已聚合到我的 report。")),
		}
	}
	// 根：batch 2 个子（writable 收窄到 src/auth/**），await all。
	rootAgent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(batchCall([]map[string]any{
			{"profile_id": "coder", "task": "子任务 A：fork 孙写 src/auth/a_grand_out.txt 后聚合", "writable_paths": []string{"src/auth/**"}},
			{"profile_id": "coder", "task": "子任务 B：fork 孙写 src/auth/b_grand_out.txt 后聚合", "writable_paths": []string{"src/auth/**"}},
		}, "all", 0)),
	}, final: replyTurn("2 个子带孙的结果都收齐了。")}

	if err := rootAgent.AppendUser(ctx, "分治：2 子 × 1 孙"); err != nil {
		t.Fatal(err)
	}
	if err := rootAgent.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 断言 1：孙写的两个文件都存在（失败时把根的 Log dump 出来——
	// 排查链路的直接证据面）。
	for _, name := range []string{"a_grand_out.txt", "b_grand_out.txt"} {
		if _, err := os.Stat(filepath.Join(wsRoot, "src", "auth", name)); err != nil {
			for _, e := range entriesMust(t, rootAgent) {
				t.Logf("root[%d] %s tool=%v meta=%v: %.200s", e.Seq, e.Role, e.Meta["name"], e.Meta, e.Content)
			}
			for _, cid := range []string{"sub_000001", "sub_000002", "sub_000003", "sub_000004"} {
				ce, err := rootAgent.log.Range(ctx, types.AgentID(cid), 1, 100)
				if err != nil {
					continue
				}
				for _, e := range ce {
					t.Logf("%s[%d] %s meta=%v: %.200s", cid, e.Seq, e.Role, e.Meta, e.Content)
				}
			}
			t.Fatalf("grand file %s: %v", name, err)
		}
	}
	// 根的 Log：两条 sub_task_result（A/B）。
	entries := entriesMust(t, rootAgent)
	subs := 0
	for _, e := range entries {
		if e.Role == types.RoleSubTaskResult {
			subs++
		}
	}
	if subs != 2 {
		t.Fatalf("root sub_task_result count = %d", subs)
	}
	// 子的 Log：各含 1 条孙的 report（孙的 id = sub_000003 / sub_000004——
	// "父 fork 顺序即拓扑"的计数格式，这里用内容匹配而不是 id 硬编码）。
	for _, e := range entries {
		if e.Role != types.RoleSubTaskResult {
			continue
		}
		childID, ok := e.Meta["child_id"].(string)
		if !ok {
			continue
		}
		sub, err := rootAgent.log.Range(ctx, types.AgentID(childID), 1, 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ce := range sub {
			if ce.Role == types.RoleSubTaskResult && strings.Contains(ce.Content, "grand") {
				found = true
			}
		}
		if !found {
			t.Fatalf("child %s has no grand report: %v", childID, sub)
		}
	}
}

// TestDepthAtTopRejectedToLLM：到顶的孙再 fork → MAX_DEPTH_REACHED 如实
// 回填（13.11"到顶拒绝 fork 的错误消息正确回传给 LLM"）。
func TestDepthAtTopRejectedToLLM(t *testing.T) {
	ctx := context.Background()
	rootAgent, _, ff, _, _ := setupForkMulti(t)

	// 孙的脚本：尝试再 fork（深度 3 的孙再 fork = 4 > MaxDepth 3 的闸）
	// → report success 直退。
	// 子脚本：fork 这个孙，等孙 report，然后自己 report。
	ff.scriptFn = func(plan *spawner.ChildPlan) []*wire.WireTurn {
		if plan.Depth == 2 {
			// 子：fork 孙，孙再 fork 失败后不影响子收 report。
			return []*wire.WireTurn{
				toolCallTurn(spawnCall("coder", "深度 2 的孙任务（孙再 fork 被拒）", []string{"src/auth/**"}, nil)),
				toolCallTurn(reportCall("success", "孙子被深度闸拒后按降级路径完成了自己的部分。")),
			}
		}
		// 孙：尝试 fork（带 writable——[阶段 12 修正] writable_paths 已是
		// schema 必填，缺失会先被 BAD_ARGS 拦住轮不到深度闸）→ 被拒 →
		// report success（”到顶直接执行任务“）。
		return []*wire.WireTurn{
			toolCallTurn(spawnCall("coder", "深度 4：应被 MAX_DEPTH 拒", []string{"src/auth/**"}, nil)),
			toolCallTurn(reportCall("success", "到顶拒绝；改用自己的 file_write 完成了任务。")),
		}
	}
	rootAgent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(batchCall([]map[string]any{
			{"profile_id": "coder", "task": "子任务：fork 一个孙，孙到顶后自行完成", "writable_paths": []string{"src/auth/**"}},
		}, "all", 0)),
	}, final: replyTurn("done")}

	if err := rootAgent.AppendUser(ctx, "fork"); err != nil {
		t.Fatal(err)
	}
	if err := rootAgent.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 孙的 id 从子的 sub_task_result meta 里拿（id 是运行时分配——
	// 硬编码它就是和实现的 numbering 耦合，测试去耦）。
	// 子的 id 由根的 ChildrenStatus 拿（同上：不硬编码分配的编号）。
	var childID string
	for id := range rootAgent.ChildrenStatus() {
		childID = string(id)
	}
	ce, err := rootAgent.log.Range(ctx, types.AgentID(childID), 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ce) == 0 {
		t.Fatalf("child log needs at least 1 line: %v", ce)
	}
	var grandID string
	for _, e := range ce {
		if e.Role == types.RoleSubTaskResult {
			grandID, _ = e.Meta["child_id"].(string)
		}
	}
	if grandID == "" {
		t.Fatalf("grand id not found in child log: %v", ce)
	}
	sub, err := rootAgent.log.Range(ctx, types.AgentID(grandID), 1, 100)
	if err != nil {
		t.Fatalf("grandchild log: %v", err)
	}
	denied := false
	for _, e := range sub {
		t.Logf("grand[%d] %s meta=%v: %.200s", e.Seq, e.Role, e.Meta, e.Content)
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, "MAX_DEPTH_REACHED") {
			denied = true
		}
	}
	if !denied {
		t.Fatal("MAX_DEPTH_REACHED denial missing in grandchild log")
	}
}

// TestSpawnBatchAwaitN：n 策略的判据单元（waitReports 计数器直测——
// 信号面：任一到达 → 叫醒；n=2 收 2 份才醒）。
func TestWaitStrategySignals(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// any：arrival 信号在场 → 立即恢复。
	a.reportArrival <- struct{}{}
	if err := a.waitReports(ctx, WaitAny, 0); err != nil {
		t.Fatalf("any: %v", err)
	}
	// n=1：判据读"已到达的 report 数"（childReports 计数）+ 信号；先
	// 预置一份 report（直测判据本身，不夹 pump 的间接面）。
	a.reportArrival <- struct{}{}
	a.mu.Lock()
	a.childReports = append(a.childReports, &proto.ChildReport{ChildID: "c1"})
	a.mu.Unlock()
	if err := a.waitReports(ctx, WaitN, 1); err != nil {
		t.Fatalf("n=1: %v", err)
	}
	// n=2：只有 1 份 → 超时（判据"到 2 份才醒"是策略本身）。
	ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel2()
	err := a.waitReports(ctx2, WaitN, 2)
	if err == nil || err != context.DeadlineExceeded {
		t.Fatalf("n=2 with one arrival must block: %v", err)
	}
}

// TestSpawnBatchPartialReject：批量中的坏项被拒 + 好项照常启动；
// 拒绝清单作为 tool_result 的一部分回传（Part 9.4 的批量形态）。
func TestSpawnBatchPartialReject(t *testing.T) {
	ctx := context.Background()
	parent, spw, ff, _, _ := setupFork(t)
	// 父命名空间收窄到 src/**（"etc/**" 项必然越界）。
	parent.env.Namespace.Mounts = []types.Mount{{Pattern: "src/**", Mode: types.PathWrite}}
	ff.scriptFn = func(*spawner.ChildPlan) []*wire.WireTurn {
		return []*wire.WireTurn{toolCallTurn(reportCall("success", "好项照常完成。"))}
	}
	parent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(batchCall([]map[string]any{
			{"profile_id": "coder", "task": "好项：读写 src/auth", "writable_paths": []string{"src/auth/**"}},
			{"profile_id": "coder", "task": "坏项：越权 writable", "writable_paths": []string{"etc/**"}},
		}, "all", 0)),
	}, final: replyTurn("部分拒绝已处理。")}

	if err := parent.AppendUser(ctx, "batch"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("partial reject must not abort: %v", err)
	}
	entries := entriesMust(t, parent)
	batchResult := false
	for _, e := range entries {
		if e.Role == types.RoleToolResult && strings.Contains(e.Content, "rejected_items") &&
			strings.Contains(e.Content, "NAMESPACE_EXCEEDED") {
			batchResult = true
		}
	}
	if !batchResult {
		t.Fatalf("batch partial-reject feedback missing: %v", entries)
	}
	if got := len(parent.ChildrenStatus()); got != 1 {
		t.Fatalf("approved count = %d", got)
	}
	_ = spw
}

// TestWatchdogKillsStalledChild：子 LLM 卡 3s；Watchdog 400ms 超时终止
// → 框架代报（reason 带 watchdog）→ 父收到 failed 并收尾（Part 8.5：
// 父不感知时间，只看"子回来了没有"）。
func TestWatchdogKillsStalledChild(t *testing.T) {
	ctx := context.Background()
	parent, spw, ff, _, _ := setupFork(t)
	// 悬停子：BuildChild 替换 slowLLM（ExecuteTurn 阻塞，取消打断）。
	ff.scriptFn = func(*spawner.ChildPlan) []*wire.WireTurn {
		return nil
	}
	ff.slow = true

	parent.llm = &scriptedParent{turns: []*wire.WireTurn{
		toolCallTurn(spawnCall("coder", "慢任务", []string{"src/auth/**"}, nil)),
	}, final: replyTurn(" Watchdog 终止了不省心的子。")}

	// Watchdog：无进展检测关闭；超时 400ms；扫描 50ms。
	wd, err := watchdog.New(watchdog.Config{
		Table:           watchdog.AdaptTable(spw),
		Log:             parent.log,
		Interval:        50 * time.Millisecond,
		MaxChildSeconds: 1, // 子 StartedAt 起超过 1s 就杀（含 BuildChild 时间）
	})
	if err != nil {
		t.Fatal(err)
	}
	wdCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd.Start(wdCtx)
	t.Cleanup(wd.Stop)

	if err := parent.AppendUser(ctx, "fork 慢子"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries := entriesMust(t, parent)
	var failed *types.LogEntry
	for _, e := range entries {
		if e.Role == types.RoleSubTaskResult && strings.Contains(e.Content, "watchdog 终止") {
			failed = e
		}
	}
	if failed == nil {
		for _, e := range entries {
			t.Logf("parent[%d] %s: %.200s", e.Seq, e.Role, e.Content)
		}
		sp, err := parent.log.Range(ctx, "sub_000001", 1, 100)
		if err == nil {
			for _, e := range sp {
				t.Logf("child[%d] %s: %.200s", e.Seq, e.Role, e.Content)
			}
		}
		t.Fatalf("watchdog surrogate failed report missing: %v", entries)
	}
	if failed.Meta["status"] != string(proto.ReportFailed) {
		t.Fatalf("failed status: %+v", failed.Meta)
	}
}

// slowLLM 是 Watchdog 测试的悬停子（ExecuteTurn 阻塞在 select：ctx 取消
// 打断——Run 的退出路径覆盖取消，框架代报 failed 随后自动发生）。
type slowLLM struct{}

func (*slowLLM) ExecuteTurn(ctx context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	select {
	case <-time.After(3 * time.Second):
		return replyTurn("终于醒了。"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
