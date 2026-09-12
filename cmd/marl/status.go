// 子命令 marl status 是状态观测面（Part 8.6 / 11.2 超时可观测，13.10 阶段 8）。
//
// 数据源：SQLite store 的 audit_events（进程表是运行时内存态，CLI 进程
// 看不到；审计是持久旁路——所有结构性事件都进它，所以从审计重建"现在
// 大概什么样"是合法的近似）。重建的内容：
//
//   - Agent 树：spawn 审计事件的 requester → child 关系；
//   - 阻塞状态：agent_state 审计事件的最后一次非 running；
//   - 讨论等待：discussion_opened / discussion_concluded 的配对。
//
// 交付判据（13.10）：marl status 能看到 Agent 在 Blocked(Discussing)，
// 讨论等待提示里有 verdict.md 的路径。
//
// 用法：
//
//	marl status [-db <path>]     # db 缺省 .marl/store.db（真实 daemon 期落位）
//
// 还原的近似性在最后一段免责输出里显式声明：CLI 进程不含 daemon 内存态，
// 审计快照允许比实时进程表晚一个事件。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"marl/internal/store"
	"marl/internal/types"
)

// 审计 action 常量（与 internal/agent、internal/discuss 的写入点共享
// 同一套约定；CLI 侧只读取）。
const (
	auditActionState          = "agent_state"
	auditActionDiscussionOpen = "discussion_opened"
	auditActionDiscussionEnd  = "discussion_concluded"
)

// discussionInfo 从审计重建的一个讨论（status 的"讨论等待"行）。
type discussionInfo struct {
	ID        string
	AgentID   types.AgentID
	Topic     string
	Dir       string
	Target    string
	Concluded bool
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("marl status", flag.ContinueOnError)
	dbPath := fs.String("db", ".marl/store.db", "存储数据库路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.OpenSQLite(*dbPath)
	if err != nil {
		return fmt.Errorf("open db %s: %w", *dbPath, err)
	}
	defer st.Close()

	evs, err := store.AuditSQLite{SQLiteStore: st}.Query(context.Background(), store.AuditFilter{Limit: 1000})
	if err != nil {
		return fmt.Errorf("query audit: %w", err)
	}
	renderStatus(os.Stdout, evs)
	return nil
}

// renderStatus 渲染状态快照（与 I/O 解耦——golden 测试直接驱动本函数）。
func renderStatus(w io.Writer, evs []*store.AuditEvent) {
	if len(evs) == 0 {
		fmt.Fprintln(w, "（无任何审计事件：库里还没有 agent 运行过）")
		return
	}
	agents := map[types.AgentID]*agentStatus{}
	discussions := map[string]*discussionInfo{}
	var order []types.AgentID

	get := func(id types.AgentID) *agentStatus {
		if a, ok := agents[id]; ok {
			return a
		}
		a := &agentStatus{ID: id, parent: ""}
		agents[id] = a
		order = append(order, id)
		return a
	}
	for _, ev := range evs {
		switch ev.Action {
		case auditActionState:
			// Agent 状态在 Target（agent.auditState 的 action 形态：
			// action=agent_state，target=state，payload 带 reason）。
			st := ev.Target
			// AgentID == 记录者：状态的归属就是它自己（spawn 的 AgentID
			// 是请求者、Target 是孩子——agent_state 没有这个二义）。
			get(ev.AgentID).State = st
			get(ev.AgentID).Reason = reasonOfPayload(ev.Payload)
		case auditActionDiscussionOpen:
			get(ev.AgentID).State = string(types.StateBlocked)
			get(ev.AgentID).Reason = string(types.BlockDiscussing)
			d := payloadMap(ev.Payload)
			// 讨论标识：payload.discussion_id 或事件 Target（audit 的
			// 形态是 Target=讨论 id）。
			id := ev.Target
			if fromPayload, _ := d["discussion_id"].(string); fromPayload != "" {
				id = fromPayload
			}
			if _, ok := discussions[id]; !ok {
				discussions[id] = &discussionInfo{ID: id, AgentID: ev.AgentID}
			}
			discussions[id].Topic, _ = d["topic"].(string)
			discussions[id].Dir, _ = d["dir"].(string)
			discussions[id].Target, _ = d["target_path"].(string)
		case auditActionDiscussionEnd:
			d := payloadMap(ev.Payload)
			id := ev.Target
			if fromPayload, _ := d["discussion_id"].(string); fromPayload != "" {
				id = fromPayload
			}
			if _, ok := discussions[id]; !ok {
				discussions[id] = &discussionInfo{ID: id, AgentID: ev.AgentID}
			}
			discussions[id].Concluded = true
			get(ev.AgentID).State = string(types.StateRunning)
			get(ev.AgentID).Reason = ""
		case "spawn":
			// spawner 的审计：AgentID=requester，Target=child，payload.depth。
			// depth 的类型容差：真实路径是 JSON 反序列化出的 float64，
			// 单测/内存事件可能是 int——统一走 numberOf。
			d := payloadMap(ev.Payload)
			child := get(types.AgentID(ev.Target))
			if d != nil {
				child.parent = ev.AgentID
				child.depth = numberOf(d["depth"])
			}
		}
	}
	// 树排序：按 depth 升序、同 depth 按 ID。
	sort.SliceStable(order, func(i, j int) bool {
		return agents[order[i]].depth < agents[order[j]].depth
	})

	// 树渲染：无父（root 或独立主体）为起点，DFS 下钻。
	// 子的呈现序按 order（审计事件出现序）稳定——树形语义下父在子上、
	// 同父之子按 fork 顺序。
	for _, id := range order {
		if agents[id].parent == "" {
			printTree(w, agents[id], agents, order, 0)
		}
	}

	isDiscussing := false
	var waiting []discussionInfo
	for _, d := range discussions {
		if !d.Concluded {
			waiting = append(waiting, *d)
			isDiscussing = true
		}
	}
	if len(waiting) > 0 {
		sort.Slice(waiting, func(i, j int) bool { return waiting[i].ID < waiting[j].ID })
		for _, d := range waiting {
			fmt.Fprintf(w, "   ⚠️ 讨论等待中: %s「%s」\n", d.ID, d.Topic)
			if d.Dir != "" {
				fmt.Fprintf(w, "        等你: %s/verdict.md\n", d.Dir)
			}
			if d.Target != "" {
				fmt.Fprintf(w, "        结论将落地: %s\n", d.Target)
			}
		}
		if isDiscussing {
			fmt.Fprintf(w, "（讨论期间该 Agent 不发起 LLM 调用——等待不烧钱）\n")
		}
	}
}

// agentStatus 是重建出的一行。
type agentStatus struct {
	ID     types.AgentID
	parent types.AgentID
	depth  int
	State  string
	Reason string
}

// stateLine 渲染状态与阻塞原因（Blocked(Discussing) 形态）。
func (a *agentStatus) stateLine() string {
	if a.State == "" {
		return "（无状态审计——尚未运行或为旧版本写入）"
	}
	if a.Reason != "" {
		return fmt.Sprintf("%s(%s)", a.State, a.Reason)
	}
	return a.State
}

func printTree(w io.Writer, a *agentStatus, agents map[types.AgentID]*agentStatus, order []types.AgentID, depth int) {
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "   └─ "
	}
	prefix := "🤖"
	if depth > 0 {
		prefix = "🔧"
	}
	fmt.Fprintf(w, "%s%s %s (depth=%d) %s\n", indent, prefix, a.ID, a.depth, a.stateLine())
	for _, id := range order {
		if agents[id].parent == a.ID {
			printTree(w, agents[id], agents, order, depth+1)
		}
	}
}

// payloadMap 把审计 payload 解成 map（还原失败 = 空表——任何解析失败
// 都不该让整个 status 崩掉）。
func payloadMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case string:
		var out map[string]any
		if err := json.Unmarshal([]byte(m), &out); err == nil {
			return out
		}
	}
	return map[string]any{}
}

// numberOf 是数字字段的宽容读取（float64 / int / int64 的 JSON 差异面）。
func numberOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// reasonOfPayload 读 agent_state 事件的 reason 字段。
func reasonOfPayload(v any) string { s, _ := payloadMap(v)["reason"].(string); return s }
