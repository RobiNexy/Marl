package agent

// agent 包的测试基建（拼装 sqlite store + resolver + 文件工作区 +
// 统一 Actor 模型的人类替身）.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"marl/internal/ns"
	"marl/internal/proto"
	"marl/internal/store"
	"marl/internal/types"
)

// newStoreForTest 是 SQLite 后端的最小装配（文件在临时目录，随 t 结束清理）。
func newStoreForTest(t *testing.T) (*store.SQLiteStore, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { s.Close() })
	return s, nil
}

// workspaceRoot 返回工作区根（测试里既是 resolver 的锚也是 file_write 的基线）。
func workspaceRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// mustResolver 构造 ns.Resolver（root 必须存在）。
func mustResolver(t *testing.T, root string) types.Resolver {
	t.Helper()
	r, err := ns.NewResolver(root)
	if err != nil {
		t.Fatalf("ns.NewResolver: %v", err)
	}
	return r
}

// writeFile 在工作区里放一个文件（测试素材）。
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

// fakeHumanLink 是人类 Actor 的测试替身（Part 14.9 ScriptedHuman 的
// 进程内最小形态）：审批请求 → 按剧本立即回信到 Agent 的信箱 channel
// ——即时投递，不等文件静默期。
type fakeHumanLink struct {
	mailbox chan proto.Envelope
	// reply 是剧本：收到 MsgGateRequest → 回信清单（缺省 = allow +
	// count 2 的 grant——"问一次给一批"的测试形态）。
	reply func(req *proto.GateRequest) []proto.Envelope

	mu       sync.Mutex
	sent     []proto.Envelope
	pendings []string
}

func newFakeHumanLink(reply func(*proto.GateRequest) []proto.Envelope) *fakeHumanLink {
	if reply == nil {
		reply = func(req *proto.GateRequest) []proto.Envelope {
			return []proto.Envelope{{
				From: "human:tester", To: req.AgentID,
				TraceID: types.TraceID("gate-" + req.RequestID),
				Type:    proto.MsgGateReply,
				Payload: &proto.GateReply{RequestID: req.RequestID, Nonce: req.Nonce,
					Action: "allow", GrantMode: "count", Count: 2, Reason: "人类放行（替身）"},
			}}
		}
	}
	return &fakeHumanLink{mailbox: make(chan proto.Envelope, 32), reply: reply}
}

func (f *fakeHumanLink) HumanID() types.AgentID { return "human:tester" }

func (f *fakeHumanLink) SendToHuman(_ context.Context, env proto.Envelope) error {
	f.mu.Lock()
	f.sent = append(f.sent, env)
	f.mu.Unlock()
	if env.Type == proto.MsgGateRequest {
		req, _ := env.Payload.(*proto.GateRequest)
		for _, rep := range f.reply(req) {
			f.mailbox <- rep // 缓冲 32：测试面不会满
		}
	}
	return nil
}

func (f *fakeHumanLink) MarkPending(agent types.AgentID, kind string) {
	f.mu.Lock()
	f.pendings = append(f.pendings, "+"+kind)
	f.mu.Unlock()
}

func (f *fakeHumanLink) ClearPending(agent types.AgentID) {
	f.mu.Lock()
	f.pendings = append(f.pendings, "-")
	f.mu.Unlock()
}

// sentRequests 返回发出的审批请求（断言用）。
func (f *fakeHumanLink) sentRequests() []proto.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.Envelope(nil), f.sent...)
}

// wireHuman 把人类替身接进 Agent（mailbox 是 Agent 收件箱的装配点——
// 替身的回信由此进入 pump）。
func wireHuman(a *Agent, f *fakeHumanLink) {
	a.mailbox = f.mailbox
	a.human = f
}
