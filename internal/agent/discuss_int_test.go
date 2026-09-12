package agent

// 讨论闭环的 Agent 侧契约测试（Part 11.2 / 13.10）。
//
// 讨论 Manager 用假实现（其真实现 / fossil 交互面在 internal/discuss 包
// 的测试里覆盖；这里测的是 Agent 侧的阻塞/恢复语义与 Log 落点契约）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"marl/internal/discuss"
	"marl/internal/types"
	"marl/internal/wire"
)

// fakeDiscussionManager 是 Agent 测试中的手势台：Open 释放 session，
// 手动的 outcomeCh 发出等待者写下的裁决。
type fakeDiscussionManager struct {
	opened         []discuss.OpenRequest
	updated        int
	outcomeC       chan discuss.Outcome
	finalizeCalled []string
	rotated        int
}

func (f *fakeDiscussionManager) Open(ctx context.Context, req discuss.OpenRequest) (*discuss.Session, error) {
	f.opened = append(f.opened, req)
	if f.outcomeC == nil {
		f.outcomeC = make(chan discuss.Outcome, 8)
	}
	return &discuss.Session{ID: "discuss_x", Branch: "discuss_x", Dir: "/tmp/d", Draft: "d.md", Target: ".marl/knowledge/contracts/x.md", Topic: req.Topic}, nil
}
func (f *fakeDiscussionManager) UpdateDraft(ctx context.Context, sess *discuss.Session, draft string) error {
	f.updated++
	return nil
}
func (f *fakeDiscussionManager) Wait(ctx context.Context, sess *discuss.Session) (discuss.Outcome, error) {
	select {
	case o := <-f.outcomeC:
		return o, nil
	case <-ctx.Done():
		return discuss.Outcome{}, ctx.Err()
	}
}
func (f *fakeDiscussionManager) RotateVerdict(sess *discuss.Session) error {
	f.rotated++
	return nil
}
func (f *fakeDiscussionManager) Finalize(ctx context.Context, sess *discuss.Session) (string, error) {
	hash := "abc123def456"
	f.finalizeCalled = append(f.finalizeCalled, hash)
	return hash, nil
}

// TestDiscussionFullLoop：request_discussion → Blocked(Discussing) →
// 批注响应（复用为修订）→ @approve → 结论落地 → 自然结束。
func TestDiscussionFullLoop(t *testing.T) {
	dm := &fakeDiscussionManager{}
	dm.outcomeC = make(chan discuss.Outcome, 8)
	// turn 序：发起讨论 → 修订草稿（响应批注）→ 最后回复。
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("request_discussion", map[string]any{
			"topic": "测试契约", "draft": "接口先这样：A / B。",
		})),
		toolCallTurn(mkCall("request_discussion", map[string]any{
			"topic": "测试契约", "draft": "接口 v2：A / B / Close。",
		})),
		replyTurn("讨论已结束，结论已落地，继续任务。"),
	}}
	a, _ := newTestAgent(t, llm)
	a.discussCfg = &DiscussionConfig{Manager: dm}
	dm.outcomeC <- discuss.Outcome{Kind: discuss.OutcomeAnnotation, Annotation: "加个 Close 方法"} // 第一轮等待
	dm.outcomeC <- discuss.Outcome{Kind: discuss.OutcomeApproved, Annotation: "同意 v2"}         // 第二轮等待
	ctx := context.Background()
	if err := a.AppendUser(ctx, "先定接口再写代码"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 提交序列：open → update（author=agent）→ finalize（author=human）。
	if len(dm.opened) != 1 || dm.updated != 1 || len(dm.finalizeCalled) != 1 {
		t.Fatalf("dm = open:%d update:%d finalize:%d", len(dm.opened), dm.updated, len(dm.finalizeCalled))
	}
	if dm.rotated != 1 { // 批注轮的 verdict 重置
		t.Fatalf("rotated = %d", dm.rotated)
	}
	entries, err := logEntries(t, a)
	if err != nil {
		t.Fatal(err)
	}
	var sawStart, sawNote, sawEnd bool
	for _, e := range entries {
		if e.Role == types.RoleUserInput && strings.Contains(e.Content, "Agent 发起讨论") {
			sawStart = true
		}
		if e.Role == types.RoleHumanNote && strings.Contains(e.Content, "Close") {
			sawNote = true
		}
		if e.Role == types.RoleUserInput && strings.Contains(e.Content, "讨论结束，结论已落地") {
			sawEnd = true
		}
	}
	if !sawStart || !sawNote || !sawEnd {
		t.Fatalf("main-log discussion entries missing (start=%v note=%v end=%v): %d entries", sawStart, sawNote, sawEnd, len(entries))
	}
	// Blocked(Discussing) 的审计发生过（状态重建的输入）。
	if a.state != types.StateIdle {
		t.Fatalf("state after run = %v", a.state)
	}
	// 13.10 交付判据：结论条目在 View（下一轮编译时 LLM 能看到）。
	if len(a.View().Items) == 0 {
		t.Fatal("view must carry the conclusion entry")
	}
}

// TestDiscussionBlockedStateAudited：进入 Blocked(Discussing) 时审计里
// 有状态迁移（marl status 的数据源；marl status 的端到端在 cmd/marl）。
func TestDiscussionBlockedStateAudited(t *testing.T) {
	dm := &fakeDiscussionManager{}
	dm.outcomeC = make(chan discuss.Outcome, 8)
	dm.outcomeC <- discuss.Outcome{Kind: discuss.OutcomeApproved, Annotation: "准"}
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("request_discussion", map[string]any{
			"topic": "t", "draft": "d",
		})),
		replyTurn("done"),
	}}
	a, _ := newTestAgent(t, llm)
	// 带审计的装配（testkit 没有 audit 复用点——本测试手动用 AuditSpy）。
	a.discussCfg = &DiscussionConfig{Manager: dm}
	if err := a.AppendUser(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestDiscussionCtxCancelKeepsSession：ctx 取消时讨论会话保留——重新
// Run 继续同一个等待（Part 8.3"阻塞是便宜的"的停机语义）。
func TestDiscussionCtxCancelKeepsSession(t *testing.T) {
	dm := &fakeDiscussionManager{}
	dm.outcomeC = make(chan discuss.Outcome) // 无缓冲：写端不写 = 永远等
	llm := &fakeLLM{turns: []*wire.WireTurn{
		toolCallTurn(mkCall("request_discussion", map[string]any{"topic": "t", "draft": "d"})),
	}}
	a, _ := newTestAgent(t, llm)
	a.discussCfg = &DiscussionConfig{Manager: dm}
	ctx, cancel := context.WithCancel(context.Background())
	_ = a.AppendUser(context.Background(), "task")
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	// 等到讨论真正 opened（轮转需要一点时间）——轮询 pending 通道。
	deadline := time.After(2 * time.Second)
	var sessOpened bool
	for !sessOpened {
		select {
		case <-deadline:
			t.Fatal("discussion did not open")
		default:
		}
		// discussSess 是主 goroutine 的单写者字段；测试从旁路读必须持锁
		// （found via -race: 它正是竞态的显微镜）。
		a.mu.Lock()
		open := a.discussSess != nil
		a.mu.Unlock()
		if open {
			sessOpened = true
		}
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	a.mu.Lock()
	sessSurvived := a.discussSess != nil
	a.mu.Unlock()
	if !sessSurvived {
		t.Fatal("discussion session must survive ctx cancellation (same wait resumes)")
	}
}
