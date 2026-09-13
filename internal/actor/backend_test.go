package actor

// 统一 Actor 模型的契约测试（Part 14.2/14.3/14.4/14.7）。
//
// 文件后端（生产的人类收件箱）：Deliver 编码 → 人类编辑 → Receive 解析
// 回信封（静默窗参数缩小到毫秒级——契约测"静默窗口 + nonce 凭据"的
// 语义，不测具体数值）。channel 后端：Drain/Close 的观察面。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"marl/internal/proto"
)

// TestHumanIDFormat：human: 前缀的构造/判定/拆解（路由与呈现的唯一判据）。
func TestHumanIDFormat(t *testing.T) {
	id := HumanID("u1000")
	if id != "human:u1000" || !IsHumanID(id) || !ValidID(id) {
		t.Fatalf("human id: %q", id)
	}
	if uid, ok := HumanUIDOf(id); !ok || uid != "u1000" {
		t.Fatalf("uid: %q %v", uid, ok)
	}
	if IsHumanID("sub_000001") || IsHumanID("") {
		t.Fatal("agent id / 空 id 不是人类形态")
	}
	if ValidID("human:") {
		t.Fatal("空 uid 非法")
	}
}

// TestCapsDiscipline：不对称是数据（纪律 3）——控制面在人类的 CapSet、
// 不在 Agent 的；认知面人类为零值（Part 14.13"没有 Executor"）。
func TestCapsDiscipline(t *testing.T) {
	h := HumanCaps()
	if !h.CanSpawn || !h.CanMessage || !h.OwnsControlPlane {
		t.Fatalf("human caps must be full: %+v", h)
	}
	if h.UsesSkills || h.HasBinding || h.HasView {
		t.Fatalf("human must have zero cognitive caps: %+v", h)
	}
	a := AgentCaps(false)
	if a.OwnsControlPlane {
		t.Fatal("AI 不得拥有控制面（纪律 3）")
	}
	if !a.UsesSkills || !a.HasBinding || !a.HasView {
		t.Fatalf("agent cognitive caps: %+v", a)
	}
	if a.CanSpawn {
		t.Fatal("AgentCaps(false) 不得持有 spawn 权威")
	}
}

// TestHumanActor：构造契约（id 形态 / caps.Kind 校验 / Mailbox 透传）。
func TestHumanActor(t *testing.T) {
	b := NewChannelBackend(4)
	h, err := NewHuman(HumanID("u9"), b, HumanCaps())
	if err != nil {
		t.Fatal(err)
	}
	if h.ID() != "human:u9" || h.Capabilities().Kind != HumanKind {
		t.Fatalf("actor: %+v", h)
	}
	if _, err := NewHuman("sub_1", b, HumanCaps()); err == nil {
		t.Fatal("非 human: 形态必须拒绝")
	}
	if _, err := NewHuman(HumanID("u9"), nil, HumanCaps()); err == nil {
		t.Fatal("nil backend 必须拒绝")
	}
	if _, err := NewHuman(HumanID("u9"), b, AgentCaps(true)); err == nil {
		t.Fatal("Kind=Agent 的 caps 必须拒绝（不一致的 CapSet 是装配 bug）")
	}
}

// TestChannelBackend：投递/接收/Drain/Close（Spawner 既有语义的搬移面）。
func TestChannelBackend(t *testing.T) {
	b := NewChannelBackend(1)
	if err := b.Deliver(Envelope{From: "a", To: "b"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := <-b.Receive(); got.To != "b" {
		t.Fatalf("recv: %+v", got)
	}
	// Drain 的观察面（缓冲 2：Deliver 不会背压）。
	b2 := NewChannelBackend(2)
	b2.Deliver(Envelope{From: "a", To: "c"})
	b2.Deliver(Envelope{From: "a", To: "d"})
	if n := len(b2.Drain()); n != 2 {
		t.Fatalf("drain: %d", n)
	}
	// Close 后 Receive 的通道关闭（pump 的 range 退出路径）；幂等。
	b.Close()
	if _, ok := <-b.Receive(); ok {
		t.Fatal("channel should be closed")
	}
	b.Close()
}

// TestFileBackendGateRoundtrip：Gate 审批的完整文件往返（Part 14.7 的
// "Gate 投递特例消除"验收）——Deliver 写审批文件 → 人类写 @grant next 2
// → Receive 解析出 MsgGateReply（nonce 对账）→ 文件搬入 done/。
func TestFileBackendGateRoundtrip(t *testing.T) {
	root := t.TempDir()
	fb, err := NewFileBackend(FileConfig{Root: root, Human: HumanID("u7"),
		PollInterval: 10 * time.Millisecond, Quiescence: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Stop()
	if err := fb.Deliver(Envelope{From: "sub_1", To: HumanID("u7"),
		Type: proto.MsgGateRequest,
		Payload: &proto.GateRequest{RequestID: "01hq", Nonce: "abcd1234",
			Kind: "llm_call", AgentID: "sub_1", RuleID: "review-all", Reason: "超限",
			Attributes: map[string]any{"task_call_count": 21, "task_tokens": 9000.0}},
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	// 文件在收件箱（frontmatter nonce 在场）。
	path := inboxPath(root, "gate_01hq.md")
	body := mustRead(t, path)
	if !strings.Contains(body, "nonce: abcd1234") || !strings.Contains(body, "@grant once") {
		t.Fatalf("template: %s", body)
	}
	// 人类编辑：把缺省行换成 @grant next 2（frontmatter 不动）。
	mustWrite(t, path, strings.Replace(body, "@grant once", "@grant next 2", 1))
	// Receive：静默窗后解析出回执信封。
	select {
	case env := <-fb.Receive():
		if env.Type != proto.MsgGateReply {
			t.Fatalf("envelope type: %v", env.Type)
		}
		if env.From != HumanID("u7") || env.To != "sub_1" {
			t.Fatalf("from/to: %+v", env)
		}
		reply, ok := env.Payload.(*proto.GateReply)
		if !ok || reply.RequestID != "01hq" || reply.Nonce != "abcd1234" ||
			reply.Action != "allow" || reply.GrantMode != "count" || reply.Count != 2 {
			t.Fatalf("reply: %+v", env.Payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gate reply not parsed")
	}
	// 消费后归档（done/；inbox 里消失——重复解析被结构排除）。
	if _, err := os.Stat(path); err == nil {
		t.Fatal("consumed approval file should move to done/")
	}
	mustRead(t, inboxPath(root, "done", "gate_01hq.md"))
}

// TestFileBackendNonceMismatch：凭据不匹配（陈旧回放）不进读端——文件
// 留在收件箱（继续留观，不是错误）。
func TestFileBackendNonceMismatch(t *testing.T) {
	root := t.TempDir()
	fb, err := NewFileBackend(FileConfig{Root: root, Human: HumanID("u7"),
		PollInterval: 10 * time.Millisecond, Quiescence: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Stop()
	if err := fb.Deliver(Envelope{From: "sub_1", To: HumanID("u7"),
		Type:    proto.MsgGateRequest,
		Payload: &proto.GateRequest{RequestID: "r1", Nonce: "good", Kind: "llm_call", AgentID: "sub_1"}}); err != nil {
		t.Fatal(err)
	}
	// 人类手滑改了 frontmatter 的 nonce（或旧轮残留）→ 校验拒绝。
	path := inboxPath(root, "gate_r1.md")
	body := mustRead(t, path)
	mustWrite(t, path, strings.Replace(body, "nonce: good", "nonce: stale", 1))
	select {
	case env := <-fb.Receive():
		t.Fatalf("stale reply must be rejected: %+v", env)
	case <-time.After(400 * time.Millisecond):
	}
	// 文件还在（不是错误，是留观）。
	mustRead(t, path)
}

// TestFileBackendDirectSay：marl say 的文件形态 → MsgDirect 信封
// （From = 收件箱主人——通道属性，文件是控制面，写者即同 UID 人类）。
func TestFileBackendDirectSay(t *testing.T) {
	root := t.TempDir()
	fb, err := NewFileBackend(FileConfig{Root: root, Human: HumanID("u7"),
		PollInterval: 10 * time.Millisecond, Quiescence: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Stop()
	if err := fb.Deliver(Envelope{From: HumanID("u7"), To: "sub_9",
		Type: proto.MsgDirect, Payload: &proto.DirectMessage{Text: "补充一句要求"}}); err != nil {
		t.Fatalf("say deliver: %v", err)
	}
	select {
	case env := <-fb.Receive():
		if env.Type != proto.MsgDirect || env.To != "sub_9" || env.From != HumanID("u7") {
			t.Fatalf("envelope: %+v", env)
		}
		msg := env.Payload.(*proto.DirectMessage)
		if msg.Text != "补充一句要求" {
			t.Fatalf("text: %q", msg.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("direct message not parsed")
	}
}

// TestFileBackendReportIsDeliverOnly：report（纯投递）永远不进读端——
// 它是人类的眼睛要看到的东西。
func TestFileBackendReportIsDeliverOnly(t *testing.T) {
	root := t.TempDir()
	fb, err := NewFileBackend(FileConfig{Root: root, Human: HumanID("u7"),
		PollInterval: 10 * time.Millisecond, Quiescence: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Stop()
	if err := fb.Deliver(Envelope{From: "sub_1", To: HumanID("u7"),
		Type:    proto.MsgChildReport,
		Payload: &proto.ChildReport{ChildID: "sub_1", Report: "done"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case env := <-fb.Receive():
		t.Fatalf("deliver-only file must not be received: %+v", env)
	case <-time.After(400 * time.Millisecond):
	}
	// 文件保留在收件箱（不归档不消失）。
	mustRead(t, inboxPath(root, mustGlobReport(root, t)))
}

// TestUnsupportedDelivery：回执类消息（MsgGateReply）不是投递形态——
// 显式报错（协议 bug 可见）。
func TestUnsupportedDelivery(t *testing.T) {
	fb, err := NewFileBackend(FileConfig{Root: t.TempDir(), Human: HumanID("u1"),
		PollInterval: time.Millisecond, Quiescence: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Stop()
	if err := fb.Deliver(Envelope{From: "h", To: "a", Type: proto.MsgGateReply,
		Payload: &proto.GateReply{}}); err == nil {
		t.Fatal("gate reply is not a deliverable-to-human shape")
	}
}

// TestPumpReceive：泵的 ctx 退出（goroutine 有 owner 有退出条件——2.7）。
func TestPumpReceive(t *testing.T) {
	fb, err := NewFileBackend(FileConfig{Root: t.TempDir(), Human: HumanID("u1"),
		PollInterval: time.Millisecond, Quiescence: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		PumpReceive(ctx, fb, func(Envelope) error { return nil })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pump did not exit on ctx cancel")
	}
}

// ---- 小工具 ----

// inboxPath 拼收件箱路径（段数可变：done/ 下的归档多一段）。
func inboxPath(root string, segs ...string) string {
	return filepath.Join(append([]string{root, "inbox"}, segs...)...)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustGlobReport 找 report_ 开头的唯一文件名（纯投递断言的路径面）。
func mustGlobReport(root string, t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(inboxPath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "report_") {
			return e.Name()
		}
	}
	t.Fatal("report file missing")
	return ""
}
