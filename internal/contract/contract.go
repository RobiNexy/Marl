// Package contract 是宿主（CLI / GUI / WebUI / TUI / IDE 插件）与 Marl
// 交互的唯一契约：一套接口 + 数据形状，零引擎依赖（只 import types 与
// store 的数据类型——不 import SQLite 之外的任何实现）。
//
// 依赖方向（依赖倒置的落点）：
//
//	contract（接口+DTO）  ←  实现层（server.App 进程内 / server.HTTPClient
//	                      ←  宿主（cmd/marl、未来的 GUI/TUI/IDE 插件）
//	          跨进程文件投递 / REST+SSE）
//
// 三个实现共享同一契约、不同传输：
//   - InProcess（server.App）：进程内直达——serve / attached start 的宿主。
//   - HTTPClient：连 serve 的 REST/SSE——daemon 在跑时的外部宿主。
//   - FileMailbox：收件箱文件投递——无守护进程时的兜底（Agent 以
//     detached 形态在跑、watcher 消费文件的场景）。
//
// 语义不变量：三个实现产生**同一效果**（SendMessage 的三种路径最终都
// 是一条 MsgDirect 信封进入运行中 Agent 的消费点；ReplyGate 的两条路径
// 都是把 @ 命令写进同一个收件箱文件——GUI 只是"人类的笔"，原则 4
// 的通道语义不因宿主而改道）。
package contract

import (
	"context"
	"errors"
	"time"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// ErrNoDaemon 是"该操作需要守护进程（进程表/事件环），但当前没有"的
// 哨兵。修复提示：marl start --detach（会拉起 serve）或 marl serve。
var ErrNoDaemon = errors.New("contract: no running daemon (start one with: marl start --detach or marl serve)")

// AgentView 是监督树的一行（GUI 的树节点）。
type AgentView struct {
	ID          string    `json:"id"`
	Parent      string    `json:"parent,omitempty"`
	Kind        string    `json:"kind"`
	Depth       int       `json:"depth"`
	State       string    `json:"state"`
	PendingKind string    `json:"pending_kind,omitempty"`
	PendingAt   time.Time `json:"pending_at,omitempty"`
	StartedAt   time.Time `json:"started_at"`
}

// RunStatus 是当前任务的运行态快照（GUI 顶栏）。
type RunStatus struct {
	Agent     string    `json:"agent,omitempty"`
	Task      string    `json:"task,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Active    bool      `json:"active"`
	State     string    `json:"state,omitempty"`
}

// InboxItem 是收件箱的一行（GUI 的收件箱列表）。
type InboxItem struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	Preview string    `json:"preview,omitempty"`
	ModTime time.Time `json:"mod_time"`
}

// GateDecision 是人类对审批请求的答复（GUI 的批准/拒绝按钮）。
//
// 语义：写 @ 命令行进收件箱文件——三个实现（进程内直写/HTTP/文件追加）
// 等价。
type GateDecision struct {
	Action string `json:"action"`           // allow | deny
	Mode   string `json:"mode,omitempty"`   // once | count | tokens | always
	Count  int    `json:"count,omitempty"`  // mode=count
	Tokens int64  `json:"tokens,omitempty"` // mode=tokens
	Reason string `json:"reason,omitempty"` // 批注
}

// DiscussionView 是一次讨论的概要（GUI 的讨论列表）。
type DiscussionView struct {
	ID      string    `json:"id"`
	Topic   string    `json:"topic"`
	Dir     string    `json:"dir"`
	ModTime time.Time `json:"mod_time"`
}

// CheckResult 是一项自检的产出（GUI 的诊断页）。
type CheckResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Note string `json:"note"`
}

// EscalationView 是一条待人类回复的求助概要（GUI 的求助列表）。
//
// 语义：对应控制面 requests/pending/ 下的一份 escalation_<ulid>.md。它与
// InboxItem 分立，因为求助走的是独立的 requests/pending→done 通道（不是
// inbox/），且回复语义不同（写 "## 回复" 正文后移动到 done/，而非追加
// @ 命令行）。
//
// 零值：零值 EscalationView 是一条"空求助"，无实际用途——本类型总是由
// Escalations() 从文件解析后返回，调用方只读不构造。
type EscalationView struct {
	ID       string    `json:"id"`                 // escalation_<ulid>（= 文件名去掉 .md）
	From     string    `json:"from,omitempty"`     // 发起求助的 Agent ID
	Question string    `json:"question,omitempty"` // 一句可被直接回答的问题
	Preview  string    `json:"preview,omitempty"`  // Reason/上下文首行预览
	ModTime  time.Time `json:"mod_time"`           // pending 文件的修改时间
}

// Event 是事件流的单元（audit_events 的透传 + 序号游标）。
type Event = store.AuditEvent

// Interaction 是宿主的全部操作面。
//
// 实现方：server.App（进程内）、server.HTTPClient（跨进程 REST）、
// server.FileMailbox（文件投递兜底）。三个实现**逐方法语义一致**——
// 一致性由契约一致性测试守护（同一场景驱动两个实现断言同结果）。
type Interaction interface {
	// 任务生命周期。
	StartTask(task string) (types.AgentID, error)
	StopTask(force bool) error
	CurrentRun() RunStatus
	// 观测。
	Agents() []AgentView
	Events(ctx context.Context, since int64, limit int) ([]*Event, error)
	Conversation(ctx context.Context, agentID string) ([]*types.LogEntry, error)
	ConversationMarkdown(ctx context.Context, agentID string) (string, error)
	Costs(ctx context.Context, taskID string) (*store.TaskCostSummary, error)
	// 交互。
	SendMessage(to, text string) error
	Inbox() ([]InboxItem, error)
	ReadInbox(name string) ([]byte, error)
	ReplyGate(id string, d GateDecision) error
	Discussions() ([]DiscussionView, error)
	ReplyDiscussion(id, annotation string, approve bool) error
	// Escalations 列出待人类回复的求助（控制面 requests/pending/）。
	//
	// 与 Inbox 分立：求助走独立的 requests/pending→done 通道，不在 inbox/。
	// 无 requests/pending 目录（尚无求助）时返回空切片 + nil，不报错。
	Escalations() ([]EscalationView, error)
	// ReplyEscalation 回复一条求助：把 reply 写入 "## 回复" 正文并把文件
	// 从 requests/pending/ 移动到 requests/done/（escalate.Mailbox 的读侧
	// 会经 nonce 校验后消费）。id = 文件名去掉 .md（escalation_<ulid>）。
	//
	// 失败：id 非法/含路径穿越 → 错误；pending 文件不存在（已回复？）→ 错误；
	// reply 为空 → 错误（空回复无意义，且读侧会把它当"编辑中间态"忽略）。
	ReplyEscalation(id, reply string) error
	// 管理。
	ConfigRaw() ([]byte, error)
	WriteConfig(raw []byte) error
	Profiles() ([]types.ProfileSummary, error)
	ProfileRaw(id string) ([]byte, error)
	WriteProfile(id string, raw []byte) error
	KnowledgeLint() (string, error)
	KnowledgePromote(ctx context.Context, relPath, globalRepo string) (string, error)
	KnowledgePull(ctx context.Context, globalRepo string) ([]string, error)
	Doctor() []CheckResult
	Close() error
}
