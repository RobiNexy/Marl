package escalate

// 契约测试（Part 11.3 / 11.5；轮询参数缩小——语义之上的数值不测）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/types"
)

func TestRoute(t *testing.T) {
	req := &proto.EscalationRequest{From: "c1", Question: "q"}
	// 父在且有权限 → parent。
	tg, err := Route(proto.EscalationRule{ParentID: "p1"}, req)
	if err != nil || tg != TargetParent {
		t.Fatalf("parent route: %v %v", tg, err)
	}
	// 父 Blocked → human（fallback 不要求显式 true——有父目标形态即
	// rule 校验合法）。
	tg, err = Route(proto.EscalationRule{ParentID: "p1", ParentBlocked: true}, req)
	if err != nil || tg != TargetHuman {
		t.Fatalf("blocked-parent route: %v %v", tg, err)
	}
	// 无父且无 fallback → 错误（无出口必须显式——丢弃求助是最坏失败）。
	if _, err := Route(proto.EscalationRule{}, req); err == nil {
		t.Fatal("no-outlet must error")
	}
}

func fastMailbox(t *testing.T) (*Mailbox, string) {
	t.Helper()
	root := t.TempDir()
	m, err := NewMailbox(MailboxConfig{ControlRoot: root, PollInterval: 10 * time.Millisecond, Quiescence: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

// TestSubmitAndReply：提问写入 pending → 人类改 + 移到 done → WaitReply
// 读出正文（nonce 匹配）。
func TestSubmitReplyRoundtrip(t *testing.T) {
	m, root := fastMailbox(t)
	req := &proto.EscalationRequest{From: "c1", Question: "Redis 还是内存？", Reason: "session 存储选型"}
	f, err := m.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	// pending 里存在。
	if _, err := os.Stat(f.Path); err != nil {
		t.Fatalf("pending file: %v", err)
	}
	// 人类：写入回复然后移动。
	body, _ := os.ReadFile(f.Path)
	edited := strings.Replace(string(body), "（回复写在这里，然后把整个文件移到 requests/done/）", "用 Redis。", 1)
	donePath := filepath.Join(root, "requests", "done", string(f.ID)+".md")
	if err := os.WriteFile(donePath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(f.Path)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reply, err := m.WaitReply(ctx, f)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if reply != "用 Redis。" {
		t.Fatalf("reply = %q", reply)
	}
}

// TestWrongNonceIgnored：nonce 不匹配的文件不解除等待（原则 4 的裁决凭据面）。
func TestWrongNonceIgnored(t *testing.T) {
	m, root := fastMailbox(t)
	req := &proto.EscalationRequest{From: "c1", Question: "q"}
	f, _ := m.Submit(req)
	// 伪造文件（nonce 错）放入 done/。
	fake := "---\nescalation_id: x\nnonce: deadbeef\n---\n\n回复伪造\n"
	if err := os.WriteFile(filepath.Join(root, "requests", "done", string(f.ID)+".md"), []byte(fake), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := m.WaitReply(ctx, f); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forge nonce must not resolve reply: %v", err)
	}
}

// TestManagerRoutesToParent：parent 路由投递 envelope（ParentSender 被
// 打到）+ pump 侧 ACK 回程用人岗实现（agent 包的集成覆盖；这里的 manager
// 只测投递的 trace 键与 deliver）。
func TestManagerRoutesToParent(t *testing.T) {
	var sent *proto.EscalationRequest
	mgr, err := NewManager(Config{
		Rule: func() (proto.EscalationRule, error) {
			return proto.EscalationRule{ParentID: "parent-1"}, nil
		},
		ParentSender: func(ctx context.Context, rule proto.EscalationRule, req *proto.EscalationRequest) error {
			sent = req
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &proto.EscalationRequest{From: "c1", Question: "q", TraceID: "t1"}
	id, err := mgr.SendEscalation(context.Background(), req)
	if err != nil || sent == nil {
		t.Fatalf("send: %v sent=%v", err, sent)
	}
	if string(id) != "" && id != types.EscalationID("t1") {
		t.Fatalf("id mismatch: %q", id)
	}
	// pump 的 ACK 回程（DeliverReply）→ WaitBack 返回。
	go mgr.DeliverReply(&proto.EscalationReply{From: "parent-1", Content: "ack", TraceID: "t1"})
	rep, err := mgr.WaitBack(context.Background(), "t1")
	if err != nil || rep.Content != "ack" || rep.IsHuman {
		t.Fatalf("ack: %v %+v", err, rep)
	}
}

// TestManagerHumanPath：human 路径的完整 manager 集成（Submit → 人类
// 修改/移动 → WaitBack 拿到 IsHuman=true 的回复）。
func TestManagerHumanPath(t *testing.T) {
	mb, root := fastMailbox(t)
	m2, err := NewManager(Config{
		Rule: func() (proto.EscalationRule, error) {
			// 无父也无 fallback → 无出口必须显式建（fail-closed）。
			return proto.EscalationRule{FallbackHuman: true}, nil
		},
		Mailbox: mb,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &proto.EscalationRequest{From: "solo", Question: "q", TraceID: "t2"}
	id, err := m2.SendEscalation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(id), "escalation_") {
		t.Fatalf("id shape: %q", id)
	}
	// 人类处理：改正文 + 移到 done/。凭据（nonce）从 Submit 生成的文件
	// frontmatter 里读——回复文件必须带同一 nonce。
	pending := filepath.Join(root, "requests", "pending", string(id)+".md")
	raw, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	nonce := nonceOf(string(raw))
	if nonce == "" {
		t.Fatal("nonce could not be parsed from the pending file")
	}
	edited := strings.Replace(string(raw), "（回复写在这里，然后把整个文件移到 requests/done/）", "内存即可。", 1)
	if err := os.WriteFile(filepath.Join(root, "requests", "done", string(id)+".md"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(pending)
	rep, err := m2.WaitBack(context.Background(), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IsHuman || rep.Content != "内存即可。" {
		t.Fatalf("human reply: %+v", rep)
	}
}

// nonceOf 解析 frontmatter 的 nonce（测试的凭据读取——与"人类看到
// frontmatter 就在那里"是同一信息面）。
func nonceOf(content string) string {
	for _, ln := range strings.Split(content, "\n") {
		if strings.HasPrefix(ln, "nonce:") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "nonce:"))
		}
	}
	return ""
}
