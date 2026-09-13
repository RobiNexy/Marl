// Package gatewaytest 提供契约 Interaction 的脚本化替身（gateway 与 tui
// 测试共享；与 actor/actortest 同模式：测试辅助是独立包而非 _test 文件，
// 因为有两个消费方）。
//
// 替身的每一处默认行为都取"生产语义的最小诚实例"：Events 用游标增量、
// StartTask 改 Run 态、ReplyGate 校验非空——替身的语义漂移是测试价值的
// 第一杀手，因此替身自身也保持最小逻辑。
package gatewaytest

import (
	"context"
	"fmt"
	"sync"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// Stub 是 contract.Interaction 的可编程替身（函数字段 = 行为注点；
// 内置数据 = 默认行为）。零值不可用，用 New 构造。
type Stub struct {
	mu sync.Mutex

	// 内置数据（默认行为的素材）。
	EventsData  []*contract.Event
	AgentsData  []contract.AgentView
	RunData     contract.RunStatus
	InboxData   []contract.InboxItem
	EscalData   []contract.EscalationView
	DiscussData []contract.DiscussionView
	CostsData   *store.TaskCostSummary

	// 行为注点（nil → 默认行为）。
	OnStartTask  func(task string) error
	OnSendMessage func(to, text string) error
	OnReplyGate  func(id string, d contract.GateDecision) error

	// 调用记录（断言"命令被转发"用）。
	SentMessages []string
	RepliedGates []contract.GateDecision
	StartedTasks []string
}

// New 构造。
func New() *Stub { return &Stub{} }

// AddEvents 追加审计事件（自动分配递增 Seq——游标语义的素材）。
func (s *Stub) AddEvents(actions ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range actions {
		s.EventsData = append(s.EventsData, &contract.Event{
			Seq:     int64(len(s.EventsData)) + 1,
			AgentID: "sub_000001",
			Action:  a,
		})
	}
}

// --- contract.Interaction（脚本行为 + 调用记录） ---

func (s *Stub) StartTask(task string) (types.AgentID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.OnStartTask != nil {
		if err := s.OnStartTask(task); err != nil {
			return "", err
		}
	}
	s.StartedTasks = append(s.StartedTasks, task)
	s.RunData.Active = true
	s.RunData.Task = task
	return "sub_000001", nil
}

func (s *Stub) StopTask(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RunData.Active = false
	return nil
}

func (s *Stub) CurrentRun() contract.RunStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.RunData
}

func (s *Stub) Agents() []contract.AgentView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.AgentView{}, s.AgentsData...)
}

func (s *Stub) Events(_ context.Context, since int64, limit int) ([]*contract.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*contract.Event
	for _, ev := range s.EventsData {
		if ev.Seq > since && len(out) < limit {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *Stub) Conversation(_ context.Context, agentID string) ([]*types.LogEntry, error) {
	return []*types.LogEntry{{AgentID: types.AgentID(agentID), Content: "stub 内容", Role: types.RoleAssistantReply}}, nil
}

func (s *Stub) ConversationMarkdown(_ context.Context, agentID string) (string, error) {
	return "### Agent " + agentID + "\n", nil
}

func (s *Stub) Costs(_ context.Context, taskID string) (*store.TaskCostSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CostsData == nil {
		return nil, fmt.Errorf("marl: not found: task %q has no entries", taskID)
	}
	return s.CostsData, nil
}

func (s *Stub) SendMessage(to, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.OnSendMessage != nil {
		return s.OnSendMessage(to, text)
	}
	s.SentMessages = append(s.SentMessages, to+"|"+text)
	return nil
}

func (s *Stub) Inbox() ([]contract.InboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.InboxItem{}, s.InboxData...), nil
}

func (s *Stub) ReadInbox(name string) ([]byte, error) {
	return []byte("---\nid: x\n---\n正文"), nil
}

func (s *Stub) ReplyGate(id string, d contract.GateDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.OnReplyGate != nil {
		return s.OnReplyGate(id, d)
	}
	s.RepliedGates = append(s.RepliedGates, d)
	return nil
}

func (s *Stub) Discussions() ([]contract.DiscussionView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.DiscussionView{}, s.DiscussData...), nil
}

func (s *Stub) ReplyDiscussion(id, annotation string, approve bool) error { return nil }

func (s *Stub) Escalations() ([]contract.EscalationView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.EscalationView{}, s.EscalData...), nil
}

func (s *Stub) ReplyEscalation(id, reply string) error { return nil }

func (s *Stub) ConfigRaw() ([]byte, error)                  { return []byte("project:\n"), nil }
func (s *Stub) WriteConfig(raw []byte) error                { return nil }
func (s *Stub) Profiles() ([]types.ProfileSummary, error)   { return nil, nil }
func (s *Stub) ProfileRaw(id string) ([]byte, error)        { return []byte("profile:\n"), nil }
func (s *Stub) WriteProfile(id string, raw []byte) error    { return nil }
func (s *Stub) KnowledgeLint() (string, error)              { return "OK", nil }
func (s *Stub) KnowledgePromote(_ context.Context, relPath, globalRepo string) (string, error) {
	return "deadbeef", nil
}
func (s *Stub) KnowledgePull(_ context.Context, globalRepo string) ([]string, error) {
	return nil, nil
}
func (s *Stub) Doctor() []contract.CheckResult { return []contract.CheckResult{{Name: "fossil", OK: true}} }
func (s *Stub) Close() error                   { return nil }

// 编译期断言：替身满足契约。
var _ contract.Interaction = (*Stub)(nil)
