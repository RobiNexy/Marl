package server

// escalation 入契约的规格测试（contract.Escalations / ReplyEscalation）。
//
// 测试策略分层：
//  1. 契约一致性：同一场景表驱动驱动三个实现（App 进程内 / HTTPClient /
//     FileMailbox），断言同结果——与 driveContractScenario 同纪律。
//  2. 读侧兼容（最关键）：escalate.Mailbox.Submit（真写侧）创建 → 契约回复
//     → escalate.Mailbox.WaitReply（真读侧）消费。nonce/frontmatter 保留
//     契约不成立时，读侧会静默忽略回复，此测试必挂。
//  3. 失败模式与解析单元的表驱动矩阵。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/escalate"
	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/wire"
)

// replyTurn 是让 App 装配成功的最小替身 turn（本组测试不跑任务，只借装配）。
func replyTurn() []*wire.WireTurn {
	return []*wire.WireTurn{{Outcomes: []wire.Outcome{{Reply: "ok"}}}}
}

// newTestAppWithHTTP 建一个独立 root 的 App + HTTP 测试服务（不跑任务），
// 返回实现、项目根、HTTP 地址。
func newTestAppWithHTTP(t *testing.T) (contract.Interaction, string, string) {
	t.Helper()
	app, ts := setupServerOpts(t, replyTurn(), nil)
	return app, app.Root, strings.TrimPrefix(ts.URL, "http://")
}

// seedEscalation 用 escalate.Mailbox.Submit（生产写侧）种一份待回复求助，
// 返回 (id, EscalationFile)。用真写侧而非手工拼文件——写侧格式变化时测试
// 会第一时间暴露契约漂移。
func seedEscalation(t *testing.T, controlRoot string) (string, *escalate.EscalationFile) {
	t.Helper()
	mb, err := escalate.NewMailbox(escalate.MailboxConfig{
		ControlRoot:  controlRoot,
		PollInterval: 20 * time.Millisecond,
		Quiescence:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewMailbox: %v", err)
	}
	f, err := mb.Submit(&proto.EscalationRequest{
		From:     "sub_000001",
		Question: "用哪种渲染方案？",
		Reason:   "纯文本与 ANSI 两方案都能实现，成本差异大，请定夺。",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return string(f.ID), f
}

// driveEscalationScenario 是三个实现共用的场景断言。
func driveEscalationScenario(t *testing.T, impl contract.Interaction, controlRoot string) {
	t.Helper()
	ctx := context.Background()

	// 无求助时：空切片 + nil（正常业务态，非错误）。
	evs, err := impl.Escalations()
	if err != nil {
		t.Fatalf("Escalations (empty): %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("Escalations (empty): want 0 items, got %d", len(evs))
	}

	id, f := seedEscalation(t, controlRoot)

	// 列表：ID/From/Question/Preview 都要解析出来。
	evs, err = impl.Escalations()
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("Escalations: want 1, got %d: %+v", len(evs), evs)
	}
	got := evs[0]
	if got.ID != id {
		t.Fatalf("Escalations ID: want %q, got %q", id, got.ID)
	}
	if got.From != "sub_000001" {
		t.Fatalf("Escalations From: got %q", got.From)
	}
	if got.Question != "用哪种渲染方案？" {
		t.Fatalf("Escalations Question: got %q", got.Question)
	}
	if got.Preview == "" {
		t.Fatal("Escalations Preview: want non-empty reason preview")
	}

	// 回复：写 "## 回复" + 移 done/。
	const reply = "用位运算方案，注意回溯剪枝。"
	if err := impl.ReplyEscalation(id, reply); err != nil {
		t.Fatalf("ReplyEscalation: %v", err)
	}

	// pending 消失、done 落地、正文含回复、frontmatter 保留。
	if _, err := os.Stat(filepath.Join(controlRoot, "requests", "pending", id+".md")); !os.IsNotExist(err) {
		t.Fatalf("pending file should be gone, stat err=%v", err)
	}
	doneData, err := os.ReadFile(filepath.Join(controlRoot, "requests", "done", id+".md"))
	if err != nil {
		t.Fatalf("done file: %v", err)
	}
	if !strings.Contains(string(doneData), reply) {
		t.Fatalf("done file missing reply:\n%s", doneData)
	}
	if !strings.Contains(string(doneData), "escalation_id: "+id) {
		t.Fatalf("done file lost frontmatter:\n%s", doneData)
	}

	// 列表清空。
	evs, err = impl.Escalations()
	if err != nil || len(evs) != 0 {
		t.Fatalf("Escalations after reply: %v, %d", err, len(evs))
	}

	// 读侧兼容（终点验证）：escalate.Mailbox.WaitReply 消费 done 文件，
	// 经 nonce 校验后返回回复正文。这是"契约回复 = 文件通道回复"的证明。
	// [权衡] WaitReply 以 PollInterval 轮询 + Quiescence 静默窗，测试用缩小的
	// 配置等待（deadline 轮询），不做真实 10s 等待。
	mb, merr := escalate.NewMailbox(escalate.MailboxConfig{
		ControlRoot:  controlRoot,
		PollInterval: 20 * time.Millisecond,
		Quiescence:   100 * time.Millisecond,
	})
	if merr != nil {
		t.Fatalf("NewMailbox (read side): %v", merr)
	}
	var gotReply string
	deadline := time.Now().Add(5 * time.Second)
	for {
		var werr error
		gotReply, werr = mb.WaitReply(ctx, f)
		if werr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("WaitReply (read-side compat): %v", werr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.TrimSpace(gotReply) != reply {
		t.Fatalf("read-side reply: want %q, got %q", reply, gotReply)
	}
}

// TestEscalationContractConformance 表驱动：同一场景驱动三个契约实现。
func TestEscalationContractConformance(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T) (impl contract.Interaction, controlRoot string)
	}{
		{
			name: "in-process App",
			make: func(t *testing.T) (contract.Interaction, string) {
				app, _ := setupServerOpts(t, replyTurn(), nil)
				return app, ControlRootOf(app.Root)
			},
		},
		{
			name: "HTTPClient",
			make: func(t *testing.T) (contract.Interaction, string) {
				_, root, addr := newTestAppWithHTTP(t)
				return NewHTTPClient(addr), ControlRootOf(root)
			},
		},
		{
			name: "FileMailbox",
			make: func(t *testing.T) (contract.Interaction, string) {
				root := t.TempDir()
				initSkeleton(t, root)
				return NewFileMailbox(root), ControlRootOf(root)
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			impl, control := tc.make(t)
			driveEscalationScenario(t, impl, control)
		})
	}
}

// TestReplyEscalation_Failures 表驱动失败模式（错误路径显式枚举）。
func TestReplyEscalation_Failures(t *testing.T) {
	root := t.TempDir()
	initSkeleton(t, root)
	lf := &LocalFiles{Root: root, Control: ControlRootOf(root)}

	cases := []struct {
		name   string
		id     string
		reply  string
		wantPt string // 错误信息须包含的片段
	}{
		{name: "empty reply", id: "escalation_x", reply: "   ", wantPt: "reply text is required"},
		{name: "path traversal", id: "../evil", reply: "ok", wantPt: "invalid escalation id"},
		{name: "missing pending", id: "escalation_nope", reply: "ok", wantPt: "not in pending"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := lf.ReplyEscalation(tc.id, tc.reply)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantPt)
			}
			if !strings.Contains(err.Error(), tc.wantPt) {
				t.Fatalf("error %q missing %q", err, tc.wantPt)
			}
		})
	}

	// 重复回复：第一次成功后，同 id 二次回复必须失败（pending 已删）。
	id, _ := seedEscalation(t, lf.Control)
	if err := lf.ReplyEscalation(id, "第一次"); err != nil {
		t.Fatalf("first reply: %v", err)
	}
	if err := lf.ReplyEscalation(id, "第二次"); err == nil {
		t.Fatal("second reply on same id must fail (pending gone)")
	}
}

// TestParseEscalationText 表驱动解析矩阵（呈现面的鲁棒性：缺字段不报错）。
func TestParseEscalationText(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantFrom string
		wantQ    string
		wantPrev string
	}{
		{
			name: "full form",
			content: "---\nescalation_id: e1\nnonce: n\n---\n\n## 求助来源\n\nfrom: sub_1\nquestion: Q1\n\n卡住了\ncontext: C\n\n## 回复\n\n（回复写在这里）\n",
			wantFrom: "sub_1", wantQ: "Q1", wantPrev: "卡住了",
		},
		{
			name:     "no reply section",
			content:  "---\nnonce: n\n---\n\nfrom: sub_2\nquestion: Q2\n\n正文\n",
			wantFrom: "sub_2", wantQ: "Q2", wantPrev: "正文",
		},
		{
			name:     "crlf",
			content:  "---\r\nnonce: n\r\n---\r\n\r\nfrom: sub_3\r\nquestion: Q3\r\n",
			wantFrom: "sub_3", wantQ: "Q3",
		},
		{
			name:     "long preview clipped",
			content:  "from: s\nquestion: q\n" + strings.Repeat("长", 100) + "\n",
			wantFrom: "s", wantQ: "q",
			wantPrev: strings.Repeat("长", 80) + "…",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			from, q, prev := parseEscalationText(tc.content)
			if from != tc.wantFrom {
				t.Fatalf("from: want %q, got %q", tc.wantFrom, from)
			}
			if q != tc.wantQ {
				t.Fatalf("question: want %q, got %q", tc.wantQ, q)
			}
			if prev != tc.wantPrev {
				t.Fatalf("preview: want %q, got %q", tc.wantPrev, prev)
			}
		})
	}
}

// TestInjectEscalationReply 的边界：有标记（原地注入）/ 无标记（末尾补段）。
func TestInjectEscalationReply(t *testing.T) {
	with := "---\nnonce: n\n---\n## 回复\n（占位）\n"
	got := injectEscalationReply(with, "R1")
	if !strings.Contains(got, "R1") || strings.Contains(got, "（占位）") {
		t.Fatalf("marker path: reply should replace placeholder:\n%s", got)
	}
	if !strings.Contains(got, "nonce: n") {
		t.Fatal("frontmatter must be preserved verbatim")
	}

	without := "---\nnonce: n\n---\n正文\n"
	got = injectEscalationReply(without, "R2")
	if !strings.Contains(got, "R2") || !strings.Contains(got, "## 回复") {
		t.Fatalf("no-marker path should append reply section:\n%s", got)
	}
}
