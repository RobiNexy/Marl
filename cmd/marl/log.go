// 子命令 marl log / attach（13.12 阶段 10 的打磨面："conversation 文件
// 导出 / marl log / marl attach"）。
//
// 导出形态（Part 1.4 的 conversation 文件格式）：
//
//	## 10:30:05  Human
//	（消息正文）
//
// 内部角色 → 呈现名的映射在 roleDisplayName（user_input→Human、
// assistant_reply→Assistant、tool_result→Tool、…）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("marl log", flag.ContinueOnError)
	dbPath := fs.String("db", ".marl/store.db", "存储数据库路径")
	agent := fs.String("agent", "", "限定 Agent（缺省全部）")
	out := fs.String("out", "", "写入 conversation 文件（缺省 stdout）")
	limit := fs.Int("limit", 200, "每 Agent 最多导出的条目数（真相之源在 Log；旧条目被略过）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.OpenSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	md, err := renderLogToMarkdown(context.Background(), store.AuditSQLite{SQLiteStore: st}, store.MessageLog(st), types.AgentID(*agent), nil, *limit)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *out, err)
		}
		fmt.Printf("已导出 conversation → %s\n", *out)
		return nil
	}
	_, _ = io.WriteString(os.Stdout, md)
	return nil
}

// cmdAttach 是 `marl attach` 子命令：对某个 Agent（或缺省全部）的会话做
// 增量 tail —— 打开 store、装配 os/signal 取消，然后把纯逻辑委托给 tailLoop。
//
// 契约：
//   - 前置：dbPath 指向可读的 store.db（不存在 → OpenSQLite 报错返回）。
//   - 行为：每 interval 拉取自上次游标以来的新 Log 条目并渲染到 stdout，
//     直到收到 SIGINT/SIGTERM（Ctrl-C）后优雅退出，返回 nil。
//   - 失败：store 打开失败 → 返回错误；轮询中的单次查询错误不终止循环
//     （见 tailLoop 的失败模式说明）。
//
// 并发：本函数拥有 signal.NotifyContext 派生的 ctx，退出前 stop() 释放
// 信号注册；store 在 defer 中关闭。tailLoop 内不再起额外 goroutine。
func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("marl attach", flag.ContinueOnError)
	dbPath := fs.String("db", ".marl/store.db", "存储数据库路径")
	agent := fs.String("agent", "", "限定 Agent（缺省跟随全部 Agent）")
	interval := fs.Duration("interval", 2*time.Second, "轮询步长")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.OpenSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	// Ctrl-C / SIGTERM 折算为 ctx 取消。用 NotifyContext 而非手写
	// os/signal，退出路径唯一且可指认（stop() 在 defer 释放注册）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("（marl attach：每 %v 轮询新条目；Ctrl-C 退出）\n", *interval)
	return tailLoop(ctx, tailDeps{
		Log:      store.MessageLog(st),
		Audit:    store.AuditSQLite{SQLiteStore: st},
		AgentID:  types.AgentID(*agent),
		Interval: *interval,
		Out:      os.Stdout,
	})
}

// tailDeps 是 tailLoop 的显式依赖集（消费侧收窄的窄接口 + 值参数）。
//
// 把依赖收进一个结构体而非散落成长参数列表，让测试能注入内存实现的
// Log/Audit、可控的时钟粒度和一个 bytes.Buffer 作为 Out —— tailLoop 因此
// 可在无信号、无真实数据库、无真实时间的条件下被完整验证。
type tailDeps struct {
	Log      store.MessageLog // 真相之源；按 (AgentID, Seq) 增量读取
	Audit    store.AuditStore // 仅在 AgentID 为空时用于发现"有哪些 Agent"
	AgentID  types.AgentID    // 空 = 跟随全部 Agent
	Interval time.Duration    // 轮询步长；<=0 时回落到 500ms 防忙等
	Out      io.Writer        // 渲染目标（stdout 或测试缓冲）
}

// tailLoop 是 attach 的纯循环内核：不触碰 os/signal、不自建时钟之外的状态，
// 因此可被测试直接驱动（用 context.WithCancel 模拟 Ctrl-C）。
//
// 数据流建模：会话 tail 是一个游标驱动的增量管道——
//
//	cursor[agent] --Range(cursor+1, LastSeq)--> 新条目 --渲染--> 推进 cursor
//
// 每个 Agent 维护独立游标（Seq 是每 Agent 独立序列）。首轮把每个已存在
// Agent 的游标初始化为其当前 LastSeq —— 即"只跟随此刻之后的新内容"，
// 不重放历史（历史用 `marl log` 导出）。这是 tail 语义的核心不变量。
//
// 契约：
//   - 前置：deps.Log 非 nil；deps.Out 非 nil。AgentID 为空时 deps.Audit 非 nil。
//   - 后置：ctx 取消时返回 nil（正常退出）。
//   - 不变量：对每个 Agent，只输出 Seq 严格大于其游标的条目，且按 Seq 升序、
//     不重不漏（依赖 MessageLog "无空洞无重号" 的 Seq 契约）。
//
// 失败模式（刻意的韧性设计）：
//   - 单次轮询中某个查询失败（如瞬时锁竞争）不终止 tail —— 记一行诊断到
//     Out 后继续下一轮。理由：attach 是长驻观察工具，一次瞬时错误就退出
//     会破坏"持续跟随"的语义；真正的致命错误（store 关闭）会持续复现，
//     由使用者 Ctrl-C 结束。
//   - ctx 取消优先于轮询：select 同时监听 ctx.Done 与 ticker，取消立即返回。
//
// 并发：本函数单 goroutine 运行，不派生子 goroutine；无共享可变状态逃逸。
// ticker 在函数退出前 Stop（唯一且可指认的释放点）。
func tailLoop(ctx context.Context, deps tailDeps) error {
	interval := deps.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond // [权衡: 防御性下限，避免 0 值导致忙等 CPU 打满]
	}
	// cursor 记录每个 Agent 已输出到的最大 Seq；nil 值表示"尚未初始化"。
	cursor := map[types.AgentID]int64{}
	// header 记录已经打过 "### Agent X" 标题的 Agent，避免每轮重复打标题。
	header := map[types.AgentID]bool{}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 立即跑一轮（初始化游标 + 输出首屏之后的新内容），随后按 ticker 节拍。
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
		first = false
		if ctx.Err() != nil {
			return nil
		}
		if err := tailTick(ctx, deps, cursor, header); err != nil {
			// 单轮失败不致命：诊断后继续（见失败模式说明）。
			fmt.Fprintf(deps.Out, "（attach 轮询错误：%v；继续跟随）\n", err)
		}
	}
}

// tailTick 执行一轮增量拉取：解析待跟随的 Agent 集合，对每个 Agent 输出
// 游标之后的新条目并推进游标。cursor/header 为就地更新的调用方拥有状态。
//
// 失败：解析 Agent 集合失败（仅 AgentID 为空时可能）→ 返回错误由 tailLoop
// 记录；单个 Agent 的 LastSeq/Range 失败被跳过（下一轮重试），不影响其它 Agent。
func tailTick(ctx context.Context, deps tailDeps, cursor map[types.AgentID]int64, header map[types.AgentID]bool) error {
	agents, err := tailAgents(ctx, deps)
	if err != nil {
		return err
	}
	for _, id := range agents {
		last, err := deps.Log.LastSeq(ctx, id)
		if err != nil {
			continue // 该 Agent 本轮不可读，下一轮重试
		}
		prev, seen := cursor[id]
		if !seen {
			// 首次见到该 Agent：把游标钉在当前末尾，只跟随之后的新内容。
			cursor[id] = last
			continue
		}
		if last <= prev {
			continue // 无新条目
		}
		entries, err := deps.Log.Range(ctx, id, prev+1, last)
		if err != nil {
			continue // 本轮读失败，游标不推进，下一轮重试
		}
		for _, e := range entries {
			if !header[id] {
				fmt.Fprintf(deps.Out, "### Agent %s\n", id)
				header[id] = true
			}
			fmt.Fprintf(deps.Out, "## %s  %s\n%s\n", e.CreatedAt.Format("15:04:05"), roleDisplayName(e.Role), clipBody(e.Content))
		}
		cursor[id] = last
	}
	return nil
}

// tailAgents 解析本轮要跟随的 Agent 列表。
//
//   - 指定了 AgentID：恒返回单元素切片（不查审计，零成本）。
//   - AgentID 为空：从 audit_events 发现已出现的 Agent（排除 "watchdog"，
//     与 renderLogToMarkdown 的口径一致）。新 spawn 的 Agent 会在其产生
//     首个审计事件后被自动纳入跟随 —— 这正是 tail "全部" 的期望行为。
func tailAgents(ctx context.Context, deps tailDeps) ([]types.AgentID, error) {
	if deps.AgentID != "" {
		return []types.AgentID{deps.AgentID}, nil
	}
	evs, err := deps.Audit.Query(ctx, store.AuditFilter{Limit: 1000})
	if err != nil {
		return nil, err
	}
	ids := make([]types.AgentID, 0, len(evs))
	for _, ev := range evs {
		if ev.AgentID != "watchdog" {
			ids = append(ids, ev.AgentID)
		}
	}
	return uniqueAgents(ids), nil
}

// renderLogToMarkdown 把 Log 条目转成 conversation markdown（Part 1.4
// 的块形态）；agentIDs 可以限定（限定的 agent 只导它自己的链条）。
func renderLogToMarkdown(ctx context.Context, as store.AuditStore, lg store.MessageLog, agentID types.AgentID, agentIDs []types.AgentID, limit int) (string, error) {
	if agentID != "" {
		agentIDs = []types.AgentID{agentID}
	}
	if agentIDs == nil {
		q := store.AuditFilter{Limit: 1000}
		evs, err := as.Query(ctx, q)
		if err != nil {
			return "", err
		}
		for _, ev := range evs {
			if ev.AgentID != "watchdog" {
				agentIDs = append(agentIDs, ev.AgentID)
			}
		}
	}
	var sb strings.Builder
	seenAgents := false
	for _, id := range uniqueAgents(agentIDs) {
		if seenAgents {
			sb.WriteString("\n")
		}
		seenAgents = true
		fmt.Fprintf(&sb, "### Agent %s\n", id)
		lastSeq, err := lg.LastSeq(ctx, id)
		if err != nil {
			continue
		}
		from := int64(1)
		if lastSeq > int64(limit) {
			from = lastSeq - int64(limit) + 1
		}
		entries, err := lg.Range(ctx, id, from, lastSeq)
		if err != nil {
			continue
		}
		for _, e := range entries {
			fmt.Fprintf(&sb, "## %s  %s\n%s\n", e.CreatedAt.Format("15:04:05"), roleDisplayName(e.Role), clipBody(e.Content))
		}
	}
	return sb.String(), nil
}

func uniqueAgents(in []types.AgentID) []types.AgentID {
	seen := map[types.AgentID]bool{}
	out := []types.AgentID{}
	for _, id := range in {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// roleDisplayName 是 Part 3.4 映射表的 conversation 呈现名（可读性
// 优先；与 View 的角色分类解耦）。
func roleDisplayName(r types.InternalRole) string {
	switch r {
	case types.RoleUserInput:
		return "Human"
	case types.RoleAssistantReply:
		return "Assistant"
	case types.RoleToolResult:
		return "Tool"
	case types.RoleThinking:
		return "Assistant(thinking)"
	case types.RoleSubTaskResult:
		return "SubTask"
	case types.RoleEscalation:
		return "Escalation"
	case types.RoleHumanNote:
		return "Note"
	default:
		return strings.ToTitle(string(r))
	}
}

// clipBody 的显示面（conversation 文件里的长内容截断；完整内容读 Log）。
func clipBody(content string) string {
	if len(content) > 4096 {
		return content[:4096] + " …（截断；完整内容在 Log）"
	}
	return content
}
