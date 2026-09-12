package agent

// 意图工具的处理（Part 4.1 / 9.2 / 9.4 / 9.6，13.7 阶段 5）。
//
// 意图与技能的边界（Part 4.1）：技能只读写外部世界；任何改变 Agent 自身或
// 系统结构的能力都是"意图"，走各自的裁决关口。意图调用**不做**技能白名单
// 校验——它的权限模型是关口裁决（CanSpawn / 深度 / 命名空间子集）。

import (
	"context"
	"encoding/json"
	"fmt"

	"marl/internal/proto"
	"marl/internal/skill"
	"marl/internal/types"
)

// 意图处理结果回填用的错误码（开放集合的扩展，见 skill 错误码契约）。
const (
	// ErrIntentNotHandled：意图已识别但本阶段未实现（诚实拒绝，不装死）。
	ErrIntentNotHandled = "INTENT_NOT_HANDLED"
	// ErrNoSpawner：spawn 意图没有关口可走（装配缺失 = 框架 bug 的可见形态）。
	ErrNoSpawner = "SPAWNER_UNAVAILABLE"
	// ErrNoParent：report 意图没有父可报（根 Agent 调用 report 的形态）。
	ErrNoParent = "NO_PARENT"
)

// spawnArgs 是 spawn_subagent 的参数形态（schema 的镜像；解析失败的字段
// 按缺失处理，必填项缺失在裁决前拦截）。
type spawnArgs struct {
	ProfileID  string   `json:"profile_id"`
	Task       string   `json:"task"`
	Writable   []string `json:"writable_paths"`
	Readable   []string `json:"readable_paths"`
	Prompt     string   `json:"prompt_override"`
	InjectSeqs []int64  `json:"inject_message_seqs"`
}

// spawnItemArgs 是 spawn_batch 里单个子的参数形态（单 spawn 的镜像子集）。
type spawnItemArgs struct {
	ProfileID  string   `json:"profile_id"`
	Task       string   `json:"task"`
	Writable   []string `json:"writable_paths"`
	Readable   []string `json:"readable_paths"`
	Prompt     string   `json:"prompt_override"`
	InjectSeqs []int64  `json:"inject_message_seqs"`
}

// batchArgs 是 spawn_batch 的参数形态（阶段 9"spawn_batch 工具"）。
type batchArgs struct {
	Items []spawnItemArgs `json:"items"`
	Await string          `json:"await"`
	N     int             `json:"n"`
}

// buildSpawnRequest 把（单项）参数折算成 SpawnRequest（单一换算点：
// 单 spawn 与批量 spawn 共享——两处的裁决语义由 Spawner 独有，这里只
// 做参数到请求的映射）。返回 nil 表示该参数不合法（已给出原因）。
func buildSpawnRequest(requester types.AgentID, item spawnItemArgs) (*proto.SpawnRequest, string) {
	if item.Task == "" {
		return nil, "task 描述不能为空：子 Agent 需要知道做什么"
	}
	return &proto.SpawnRequest{
		RequesterID:     requester,
		ProfileID:       types.ProfileID(item.ProfileID),
		PromptOverride:  item.Prompt,
		TaskDescription: item.Task,
		WritablePaths:   item.Writable,
		ReadablePaths:   item.Readable,
		InjectMessages:  item.InjectSeqs,
		TraceID:         types.TraceID(fmt.Sprintf("spawn-%s", requester)),
	}, ""
}

// batchRejects 是批量裁决的逐项失败记录（回填给模型的纠错依据——
// Part 9.4 的逐项形态：哪一项因什么被拒，父可修一份重派）。
type batchRejects struct {
	Index  int    `json:"index"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// reportArgs 是 report_to_parent 的参数形态（Part 9.6 的 schema）。
type reportArgs struct {
	Report string `json:"report"`
	Status string `json:"status"`
}

// executeIntent 处理一次意图调用，返回要回填给模型的结果。
//
// 失败：意图是协议的一部分（IsIntentTool 已保证名字合法），但本阶段只实现
// spawn_subagent 与 report_to_parent——其余意图返回 INTENT_NOT_HANDLED
// （Part 9.4 的"如实回填"：模型知道此路不通，而不是收到似是而非的成功）。
func (a *Agent) executeIntent(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	switch call.Name {
	case proto.ToolSpawnSubagent:
		return a.intentSpawn(ctx, call)
	case proto.ToolSpawnBatch:
		return a.intentSpawnBatch(ctx, call)
	case proto.ToolReportToParent:
		return a.intentReport(ctx, call)
	case proto.ToolRequestDiscussion:
		return a.intentDiscussion(ctx, call)
	default:
		return skill.NewFailure(ErrIntentNotHandled,
			"意图 %s 在当前阶段不可用（框架未实现该裁决关口）", call.Name), nil
	}
}

// intentSpawn 处理 spawn_subagent：构造 SpawnRequest → Spawner 裁决。
//
// 拒绝是**正常业务结果**（Part 9.2：裁决结果不是 error）——错误码与可读原因
// 如实回填，模型据此修正（改小 writable 范围 / 放弃 fork 直接干）。
// 批准 → 记录子、进入 WaitChildren（eventLoop 在轮边界返回 errWaitChildren）。
func (a *Agent) intentSpawn(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	if a.spawner == nil {
		return skill.NewFailure(ErrNoSpawner, "spawn_subagent 没有可用的裁决关口（框架装配缺失）"), nil
	}
	var args spawnArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			a.roundFormatErrors++
			return skill.NewFailure("BAD_ARGS", "arguments not valid JSON: %v", err), nil
		}
	}
	if args.Task == "" {
		return skill.NewFailure("BAD_ARGS", "task 描述不能为空：子 Agent 需要知道做什么"), nil
	}
	req, invalid := buildSpawnRequest(a.id, spawnItemArgs{
		ProfileID: args.ProfileID, Task: args.Task, Writable: args.Writable,
		Readable: args.Readable, Prompt: args.Prompt, InjectSeqs: args.InjectSeqs,
	})
	if invalid != "" {
		return skill.NewFailure("BAD_ARGS", "%s", invalid), nil
	}
	// 意图关口缺位即失败回填（见 intentSpawn 的注释）。
	dec, err := a.spawner.Adjudicate(ctx, req)
	if err != nil {
		return nil, err // 框架级故障上抛（与"请求不合规"严格区分，Part 9.2）
	}
	if dec.Status == proto.SpawnRejected {
		// 拒绝如实回传（Part 9.4）：错误码 + 指出正确范围的 Reason。
		return skill.NewFailure(string(dec.Code), "%s", dec.Reason), nil
	}
	// 批准：登记到运行时 Children（Part 8.1：框架维护，工具读）。
	a.mu.Lock()
	a.pendingChildren[dec.ChildAgentID] = true
	a.childrenStatus[dec.ChildAgentID] = types.ChildRunning
	a.mu.Unlock()
	a.auditf(ctx, "spawn", string(dec.ChildAgentID), map[string]any{
		"requester": string(a.id), "task": args.Task, "writable": args.Writable,
	})
	return skill.NewSuccess(map[string]any{
		"child_agent_id": string(dec.ChildAgentID),
		"message":        "子 Agent 已启动；它完成后会 report，你将收到它的结果。在此期间你不会收到任何中间输出。",
	}), nil
}

// intentSpawnBatch 处理 spawn_batch（Part 9.7 的批量形态，13.11 阶段 9）：
//
//	逐项过 Spawner 裁决（命名空间子集 / 扇出闸 / 注入合法性都与单项同一
//	关口——批量不批发豁免）；
//	部分拒绝合法：已批准的照常启动，拒绝项逐项回填（batchRejects）——
//	"全部或无"会强迫模型为了一个坏参数放弃 N-1 个好子任务；
//	失败归因：单批的裁决以一个 tool_result 汇总回填（批量是父的规划
//	动作，不是子间的协调协议）。
//
// 恢复策略（await）写在 agent 的 wait 状态里（wait.go）：all / any / n。
// 全部被拒 → errWaitChildren 不会发生（pending 空 → eventLoop 继续循环）。
func (a *Agent) intentSpawnBatch(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	if a.spawner == nil {
		return skill.NewFailure(ErrNoSpawner, "spawn_batch 没有可用的裁决关口（框架装配缺失）"), nil
	}
	var args batchArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			a.roundFormatErrors++
			return skill.NewFailure("BAD_ARGS", "arguments not valid JSON: %v", err), nil
		}
	}
	if len(args.Items) == 0 {
		return skill.NewFailure("BAD_ARGS", "items 不能为空：至少要派一个子 Agent"), nil
	}
	// await 的启动期校验（fail fast 在裁决之前——批量被部分批准后再发现
	// 策略参数非法，已跑起来的子资源无法收回）。
	kind := WaitAll
	n := 0
	switch args.Await {
	case "", "all":
		kind = WaitAll
	case "any":
		kind = WaitAny
	case "n":
		kind = WaitN
		if args.N < 1 || args.N >= len(args.Items) {
			return skill.NewFailure("BAD_ARGS", "await=n 需要 1..%d 的 n", len(args.Items)), nil
		}
		n = args.N
	default:
		return skill.NewFailure("BAD_ARGS", "await 只能取 all / any / n"), nil
	}
	approved := 0
	rejects := make([]batchRejects, 0)
	for i, item := range args.Items {
		req, invalid := buildSpawnRequest(a.id, item)
		if invalid != "" {
			rejects = append(rejects, batchRejects{Index: i, Code: "BAD_ARGS", Reason: invalid})
			continue
		}
		dec, err := a.spawner.Adjudicate(ctx, req)
		if err != nil {
			// 框架级故障：批量的部分已完成项已跑（批准即启动），错误
			// 上抛由 eventLoop 终结————父下一轮重派前先收 report。
			return nil, fmt.Errorf("spawn batch item %d: %w", i, err)
		}
		if dec.Status == proto.SpawnRejected {
			rejects = append(rejects, batchRejects{Index: i, Code: string(dec.Code), Reason: dec.Reason})
			continue
		}
		a.mu.Lock()
		a.pendingChildren[dec.ChildAgentID] = true
		a.childrenStatus[dec.ChildAgentID] = types.ChildRunning
		a.mu.Unlock()
		approved++
	}
	a.setWait(kind, n)
	a.auditf(ctx, "spawn_batch", fmt.Sprintf("items=%d", len(args.Items)), map[string]any{
		"agent_id": string(a.id), "approved": approved, "rejected": len(rejects),
		"await": args.Await, "requester": string(a.id),
	})
	if approved == 0 {
		// 全军覆没：拒绝清单逐项回传（模型自己决定放弃或修正）。
		rejJSON, _ := json.Marshal(rejects)
		return skill.NewFailure("SPAWN_ALL_REJECTED", "全部 %d 项都被拒绝：%s", len(rejects), rejJSON), nil
	}
	data := map[string]any{
		"approved":        approved,
		"child_agent_ids": a.childrenSnapshotIDs(),
		"message":         "批量已启动。子 Agent 各自完成后 report；按 await 判据恢复。",
	}
	if len(rejects) > 0 {
		rejJSON, _ := json.Marshal(rejects)
		data["rejected_items"] = json.RawMessage(rejJSON)
	}
	return skill.NewSuccess(data), nil
}

// childrenSnapshotIDs 是当前 pending 子的 ID 快照（批量结果回显）。
func (a *Agent) childrenSnapshotIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.pendingChildren))
	for id := range a.pendingChildren {
		out = append(out, string(id))
	}
	return out
}

// intentReport 处理 report_to_parent（Part 9.6）：机械检查 → 投递。
//
// 机械检查不通过 → 状态降级（partial / failed），report 文本原样保留——
// 父看到的结论是框架修正过的，但子的自述不删（审计要完整）。
// 投递成功后本 Agent 的任务终结（eventLoop 在轮边界检查 a.reported）。
func (a *Agent) intentReport(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	if a.reporter == nil {
		return skill.NewFailure(ErrNoParent, "你是根 Agent（没有父级）；完成任务直接给出总结即可"), nil
	}
	var args reportArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			a.roundFormatErrors++
			return skill.NewFailure("BAD_ARGS", "arguments not valid JSON: %v", err), nil
		}
	}
	status := proto.ReportStatus(args.Status)
	if !status.Valid() {
		return skill.NewFailure("BAD_ARGS", "status %q 非法（必须是 success / partial / failed）", args.Status), nil
	}
	if args.Report == "" {
		return skill.NewFailure("BAD_ARGS", "report 不能为空：父需要知道你做了什么"), nil
	}
	report := &proto.ChildReport{
		ChildID:      a.id,
		Status:       status,
		Report:       args.Report,
		FilesChanged: a.filesWritten(),
		TokenUsed:    a.totalTokenEst(),
	}
	// 机械检查（原则 4：不采信自述）。
	checked, err := a.reportChecker.Check(report, a.writablePaths())
	if err != nil {
		return nil, fmt.Errorf("report check: %w", err) // 基础设施故障
	}
	a.auditf(ctx, "report_check", string(a.id), map[string]any{
		"claimed":  string(report.Status),
		"reported": string(checked.Status),
		"blockers": checked.Blockers,
	})
	if err := a.reporter.ReportToParent(ctx, checked); err != nil {
		// 投递失败：任务不能当作完成。回填失败让模型重试 report
		// （父信箱暂时满等场景）；基础设施故障上抛由 eventLoop 终结。
		return skill.NewFailure("REPORT_UNDELIVERED", "report 投递失败：%v（请重试）", err), nil
	}
	a.reported = true
	return skill.NewSuccess(map[string]any{
		"message": "report 已送达父 Agent；你的任务完成。",
	}), nil
}

// filesWritten 返回本 Agent 成功写入的文件列表（file_write ok=true 的
// 相对路径；机械检查"声称改了文件但无改动"的数据源）。
func (a *Agent) filesWritten() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.writtenFiles))
	for f := range a.writtenFiles {
		out = append(out, f)
	}
	return out
}

// totalTokenEst 返回本 Agent 已消耗的 est token（Watchdog/报表口径）。
func (a *Agent) totalTokenEst() int64 {
	last, err := a.log.LastSeq(context.Background(), a.id)
	if err != nil {
		return 0
	}
	total, err := a.log.TotalTokens(context.Background(), a.id)
	if err != nil {
		return 0
	}
	_ = last
	return total
}

// writablePaths 提取命名空间里 write 模式的挂载模式串（机械检查的扫描范围）。
func (a *Agent) writablePaths() []string {
	out := make([]string, 0, len(a.env.Namespace.Mounts))
	for _, m := range a.env.Namespace.Mounts {
		if m.Mode == types.PathWrite {
			out = append(out, m.Pattern)
		}
	}
	return out
}
