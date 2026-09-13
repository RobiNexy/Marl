package actor

// 零延迟结构化回复的规格（types.ReplyFinalMarker 协议的读侧行为）：
//
//   - 带标记的 gate 回执 → 静默窗未满即消费（快路径，~几个 poll tick）
//   - 无标记的 gate 回执 → 等满静默窗（慢路径，原行为不变）
//
// 用缩短的 PollInterval/Quiescence 驱动真实 watcher（契约测"静默窗的
// 语义"，不测具体数值——与 backend_test 同纪律）。

import (
	"strings"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/types"
)

// TestGateReplyFastPath 表驱动：快/慢路径的唯一差异是标记的有无。
// 断言语义：快路径 = 在静默窗明显未满时已消费（<400ms）；慢路径 = 至少
// 等过了静默窗的大半（>=1.5s，留 tick 对齐余量）——两个方向都不放过。
func TestGateReplyFastPath(t *testing.T) {
	cases := []struct {
		name     string
		marker   bool
		wantFast bool
	}{
		{name: "structured reply with marker is immediate", marker: true, wantFast: true},
		{name: "manual reply waits quiescence", marker: false, wantFast: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Poll 20ms / Quiescence 2s：快路径应在几个 tick（~百 ms）内
			// 完成；慢路径必须等满静默窗。
			root := t.TempDir()
			fb, err := NewFileBackend(FileConfig{Root: root, Human: HumanID("u7"),
				PollInterval: 20 * time.Millisecond, Quiescence: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer fb.Stop()
			if err := fb.Deliver(Envelope{From: "sub_1", To: HumanID("u7"),
				Type: proto.MsgGateRequest,
				Payload: &proto.GateRequest{RequestID: "01hq", Nonce: "abcd1234",
					Kind: "llm_call", AgentID: "sub_1", RuleID: "review-all", Reason: "超限",
					Attributes: map[string]any{"task_call_count": 21.0}},
			}); err != nil {
				t.Fatalf("deliver: %v", err)
			}

			// 回执：唯一差异 = 是否附带"写完"声明。
			path := inboxPath(root, "gate_01hq.md")
			body := mustRead(t, path)
			reply := body + "\n@grant once\n（批注）\n"
			if tc.marker {
				reply = types.WithReplyFinal(reply)
			}
			start := time.Now()
			mustWrite(t, path, reply)

			select {
			case env := <-fb.Receive():
				elapsed := time.Since(start)
				if tc.wantFast && elapsed > 400*time.Millisecond {
					t.Fatalf("快路径消费耗时 %v > 400ms（标记未生效）", elapsed)
				}
				if !tc.wantFast && elapsed < 1500*time.Millisecond {
					t.Fatalf("慢路径仅等了 %v（<静默窗 2s 的大半）——静默窗被意外跳过", elapsed)
				}
				got, ok := env.Payload.(*proto.GateReply)
				if !ok || got.Action != "allow" || got.GrantMode != "once" {
					t.Fatalf("回执折算错误: %+v", env.Payload)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("回执未在 4s 内被消费")
			}
		})
	}
}

// TestGateReplyMarkerNotInReason 验证标记不泄漏进回执的批注（协议字节是
// 管道——agent 的上下文不应看到 HTML 注释；Reason 与批注逐字节等值）。
func TestGateReplyMarkerNotInReason(t *testing.T) {
	root := t.TempDir()
	fb, err := NewFileBackend(FileConfig{Root: root, Human: HumanID("u7"),
		PollInterval: 10 * time.Millisecond, Quiescence: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Stop()
	if err := fb.Deliver(Envelope{From: "sub_1", To: HumanID("u7"),
		Type: proto.MsgGateRequest,
		Payload: &proto.GateRequest{RequestID: "01hn", Nonce: "abcd1234",
			Kind: "shell", AgentID: "sub_1", Reason: "需要审批"},
	}); err != nil {
		t.Fatal(err)
	}
	path := inboxPath(root, "gate_01hn.md")
	body := mustRead(t, path)
	mustWrite(t, path, types.WithReplyFinal(body+"\n@grant once\n"))

	select {
	case env := <-fb.Receive():
		got, ok := env.Payload.(*proto.GateReply)
		if !ok {
			t.Fatalf("payload: %T", env.Payload)
		}
		if strings.Contains(got.Reason, types.ReplyFinalMarker) {
			t.Fatalf("标记泄漏进 Reason: %q", got.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("回执未消费")
	}
}
