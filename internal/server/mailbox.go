package server

// FileMailbox：无守护进程时的契约实现（文件投递兜底）。
//
// 场景：Agent 以 detached 形态在跑（watcher 消费收件箱），宿主进程
// （CLI/脚本）没有进程表可达——SendMessage 走文件投递，运行中的任务
// 下一轮消费。引擎态操作（StartTask/StopTask/Agents/Events/Conversation/
// Costs）需要守护进程 → 一律 ErrNoDaemon（调用方先拉起：
// marl start --detach 或 marl serve）。
//
// 文件可达面（Inbox/ReadInbox/ReplyGate/Discussions/ReplyDiscussion/
// 配置/知识库/Doctor）与 App **逐字节同语义**——共享 LocalFiles 核。

import (
	"context"
	"fmt"

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// FileMailbox 是契约的文件投递实现（无守护进程的宿主）。
type FileMailbox struct {
	lf  *LocalFiles
	hum actor.ActorID
}

// NewFileMailbox 构造（root = 项目目录）。
func NewFileMailbox(root string) *FileMailbox {
	return &FileMailbox{
		lf:  &LocalFiles{Root: root, Control: ControlRootOf(root)},
		hum: actor.HumanID(osUID()),
	}
}

// SendMessage 走文件投递（收件箱 direct 文件；watcher 消费并路由）。
func (m *FileMailbox) SendMessage(to, text string) error { return m.lf.SendMessage(to, text) }

// Inbox 列出收件箱（文件面）。
func (m *FileMailbox) Inbox() ([]contract.InboxItem, error) { return m.lf.Inbox() }

// ReadInbox 读收件文件（文件面）。
func (m *FileMailbox) ReadInbox(name string) ([]byte, error) { return m.lf.ReadInbox(name) }

// ReplyGate 写审批文件（文件面；watcher 消费并路由回 Agent）。
func (m *FileMailbox) ReplyGate(id string, d contract.GateDecision) error {
	return m.lf.ReplyGate(id, d)
}

// Discussions 列出讨论（文件面）。
func (m *FileMailbox) Discussions() ([]contract.DiscussionView, error) {
	return m.lf.Discussions()
}

// ReplyDiscussion 写 verdict（文件面）。
func (m *FileMailbox) ReplyDiscussion(id, annotation string, approve bool) error {
	return m.lf.ReplyDiscussion(id, annotation, approve)
}

// ConfigRaw / WriteConfig / ProfileRaw / WriteProfile / KnowledgeLint /
// KnowledgePromote / KnowledgePull / Doctor（文件面，委托 LocalFiles）。
func (m *FileMailbox) ConfigRaw() ([]byte, error) { return m.lf.ConfigRaw() }
func (m *FileMailbox) WriteConfig(raw []byte) error {
	return m.lf.WriteConfig(raw)
}
func (m *FileMailbox) ProfileRaw(id string) ([]byte, error) { return m.lf.ProfileRaw(id) }
func (m *FileMailbox) WriteProfile(id string, raw []byte) error {
	return m.lf.WriteProfile(id, raw)
}
func (m *FileMailbox) KnowledgeLint() (string, error) { return m.lf.KnowledgeLint() }
func (m *FileMailbox) KnowledgePromote(ctx context.Context, relPath, globalRepo string) (string, error) {
	return m.lf.KnowledgePromote(ctx, relPath, globalRepo)
}
func (m *FileMailbox) KnowledgePull(ctx context.Context, globalRepo string) ([]string, error) {
	return m.lf.KnowledgePull(ctx, globalRepo)
}
func (m *FileMailbox) Doctor() []CheckResult { return RunChecks(m.lf.Root) }

// ---- 引擎态操作：无守护进程 → ErrNoDaemon（调用方先拉起） ----

// StartTask 需要进程表（守护进程持有）——无守护 → ErrNoDaemon。
func (m *FileMailbox) StartTask(task string) (types.AgentID, error) {
	return "", contract.ErrNoDaemon
}

// StopTask 需要进程表——无守护 → ErrNoDaemon。
func (m *FileMailbox) StopTask(force bool) error { return contract.ErrNoDaemon }

// CurrentRun 需要进程表——无守护 → 零值（Active=false 是诚实的"未知"）。
func (m *FileMailbox) CurrentRun() contract.RunStatus {
	return contract.RunStatus{Active: false}
}

// Agents 需要进程表——无守护 → 空树。
func (m *FileMailbox) Agents() []contract.AgentView { return []contract.AgentView{} }

// Events 需要审计访问（守护进程的 store）——无守护 → ErrNoDaemon。
func (m *FileMailbox) Events(ctx context.Context, since int64, limit int) ([]*contract.Event, error) {
	return nil, contract.ErrNoDaemon
}

// Conversation 需要守护进程的 store——无守护 → ErrNoDaemon。
func (m *FileMailbox) Conversation(ctx context.Context, agentID string) ([]*types.LogEntry, error) {
	return nil, contract.ErrNoDaemon
}

// ConversationMarkdown 同上。
func (m *FileMailbox) ConversationMarkdown(ctx context.Context, agentID string) (string, error) {
	return "", contract.ErrNoDaemon
}

// Costs 同上。
func (m *FileMailbox) Costs(ctx context.Context, taskID string) (*store.TaskCostSummary, error) {
	return nil, contract.ErrNoDaemon
}

// Profiles 走文件面（Profile 目录就够）。
func (m *FileMailbox) Profiles() ([]types.ProfileSummary, error) { return m.lf.Profiles() }

// Close 无资源（每次操作即用即弃）。
func (m *FileMailbox) Close() error { return nil }

// HumanID 返回人类 ActorID（文件投递的 From 推导）。
func (m *FileMailbox) HumanID() actor.ActorID { return m.hum }

// 编译期断言：FileMailbox 也满足宿主契约。
var _ contract.Interaction = (*FileMailbox)(nil)

// fmt 引用防呆（错误的包装面留在这里）。
var _ = fmt.Errorf
