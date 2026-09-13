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
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
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
	color := fs.String("color", "auto", "色块：auto（终端时开）/ always / never")
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
	colorize := isTerminal()
	switch *color {
	case "always":
		colorize = true
	case "never":
		colorize = false
	}
	renderStatusColored(os.Stdout, evs, colorize)
	return nil
}

// isTerminal 的判定点（isatty.IsTerminal 的引入面唯一——渲染测试直接
// 传 color 参数，不依赖真实终端）。
func isTerminal() bool {
	return isatty.IsTerminal(os.Stdout.Fd())
}

// renderStatus 渲染状态快照（与 I/O 解耦——golden 测试直接驱动本函数）。
func renderStatus(w io.Writer, evs []*store.AuditEvent) {
	renderStatusColored(w, evs, false)
}

// renderStatusColored 是带色块的渲染（color=false 时不输出 ANSI——输入
// 重定向/管道里的字节垃圾没有可读性平衡）。
//
// 色块口径（Part 8.6 的 ⚠️/✅ 同向映射：状态恒等式在色里承载）：
//
//	green（青）—— Running / Idle（正常态）
//	yellow（黄）—— Blocked（含 Blocked(Discussing) 的等人场景）
//	red（红）—— Crashed（ 框架代报/Watchdog 终止的路径）
func renderStatusColored(w io.Writer, evs []*store.AuditEvent, color bool) {
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
			get(ev.AgentID).Since = ev.Timestamp
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
			// 请求者也入行（Part 14.6：requester 可以是人类 Actor——
			// 监督树以人类为根；行的 state 由 agent_state/actor_registered
			// 事件补齐，缺省"未运行"）。
			get(ev.AgentID)
			if d != nil {
				child.parent = ev.AgentID
			}
			// depth 守卫（[阶段 12 修正] 真机发现 #8）：agent 侧的 spawn
			// 审计（intentSpawn 的回填）不带 depth 字段——用它覆盖会把
			// spawner 侧的正确 depth 重置为 0。只在字段在场时更新。
			if dv, ok := d["depth"]; ok {
				child.depth = numberOf(dv)
			}
		case "actor_registered":
			// 统一 Actor 面（Part 14.2 #2）：人类/装配 Agent 的注册行
			//（人类是监督树的根——depth=0 的拓扑事实）。
			d := payloadMap(ev.Payload)
			row := get(ev.AgentID)
			row.depth = numberOf(d["depth"])
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
			printTree(w, agents[id], agents, order, 0, color)
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
// agentStatus 是重建出的一行。
type agentStatus struct {
	ID     types.AgentID
	parent types.AgentID
	depth  int
	State  string
	Reason string
	// Since 是最后一次状态事件的时刻（Running 的"运行多久"与 Blocked
	// 的"卡了多久"共用；零值 = 无时间口径——旧审计没有 Timestamp 时退化）。
	Since time.Time
}

// stateLine 渲染状态 + 阻塞原因 + 时长（Part 8.6 的
// "Blocked(Discussing) 3h08m" 形态；时长 = 最后一次状态事件到"现在"。
// [偏离文档: 时长的基准是审计快照的重建时刻（statuses 允许滞后一个
// 事件——三进制： lasted "现在"进程内存态不落库）。]
func (a *agentStatus) stateLine() string {
	if a.State == "" {
		return "（无状态审计——尚未运行或为旧版本写入）"
	}
	out := a.State
	if a.Reason != "" {
		out += "(" + a.Reason + ")"
	}
	if !a.Since.IsZero() {
		out += " " + humanDuration(sinceOf(a))
	}
	return out
}

// sinceOf 延迟包装（unit-testable 的开销隔离——now 的注入点）。
func sinceOf(a *agentStatus) time.Duration { return time.Since(a.Since) }

// humanDuration 渲染"如何告诉人"的时长（大单位优先）。
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func printTree(w io.Writer, a *agentStatus, agents map[types.AgentID]*agentStatus, order []types.AgentID, depth int, color bool) {
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "   └─ "
	}
	// 图标按 ActorID 前缀（呈现层专用——Part 14.2 纪律 1 的"Kind 只管
	// 渲染"在 CLI 面的形态；权限判断不在此处）。
	prefix := "🤖"
	if strings.HasPrefix(string(a.ID), "human:") {
		prefix = "👤"
	} else if depth > 0 {
		prefix = "🔧"
	}
	fmt.Fprintf(w, "%s%s %s (depth=%d) %s\n", indent, prefix, a.ID, a.depth, colorizeState(color, a.stateLine()))
	for _, id := range order {
		if agents[id].parent == a.ID {
			printTree(w, agents[id], agents, order, depth+1, color)
		}
	}
}

// ANSI 色码（色块的机制面；Go 的 map 遍历顺序与此无关——渲染输出是
// stdout 的字节流）。Terminal 判定在 cmdStatus 层（renderStatus 保持
// 无色的默认——管道/文件里 ANSI 字节没有可读性收益）。
const (
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiReset  = "\x1b[0m"
)

// colorizeState 按状态选色（色块口径见 renderStatusColored 的注释；
// 无法归类的状态不加色——比猜错一个色更诚实）。
func colorizeState(color bool, stateLine string) string {
	if !color {
		return stateLine
	}
	var code string
	switch {
	case strings.Contains(stateLine, "blocked"):
		code = ansiYellow
	case strings.Contains(stateLine, "crashed"):
		code = ansiRed
	case strings.Contains(stateLine, "running"), strings.Contains(stateLine, "idle"):
		code = ansiGreen
	default:
		return stateLine
	}
	return code + stateLine + ansiReset
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
