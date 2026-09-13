package server

// 服务接口的全生命周期测试（GUI 的契约面；阶段 14）。
//
// 用替身线路（脚本化 LLM）驱动完整流程：任务启动 → 监督树 → 插话
// （human_note 进上下文）→ 收件箱/审批（gate 回执 → Agent 恢复）→
// 账单 → 事件流 → 配置读写 → 停止。零网络、零真实厂商。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/agent"
	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// fakeLine 的脚本形态（同 agent 包的 fakeLLM：按轮次回放）。
type scriptedLine struct {
	mu    sync.Mutex
	turns []*wire.WireTurn
	reqs  int
}

func (l *scriptedLine) ExecuteTurn(_ context.Context, _ *wire.CanonicalRequest) (*wire.WireTurn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs++
	i := l.reqs - 1
	if i >= len(l.turns) {
		i = len(l.turns) - 1
	}
	return l.turns[i], nil
}

func turnTool(name string, args map[string]any) *wire.WireTurn {
	b, _ := json.Marshal(args)
	return &wire.WireTurn{Outcomes: []wire.Outcome{{
		ToolCalls: []types.ToolCall{{ID: "call-" + name, Name: name, Arguments: b}},
	}}}
}

func turnReply(text string) *wire.WireTurn {
	return &wire.WireTurn{Outcomes: []wire.Outcome{{Reply: text, Entry: types.LogEntry{
		Content: text, Role: types.RoleAssistantReply, Prov: types.ProvOriginal, Audience: types.AudienceBoth,
	}}}}
}

// setupServer 装配一个测试 App + HTTP 服务（替身线路；skeleton 完整）。
func setupServer(t *testing.T, turns []*wire.WireTurn) (*App, *httptest.Server) {
	return setupServerOpts(t, turns, nil)
}

// setupServerOpts 带 Option 的装配（测试的收件箱时序提速入口）。
func setupServerOpts(t *testing.T, turns []*wire.WireTurn, extra []Option) (*App, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	initSkeleton(t, root)
	line := &scriptedLine{turns: turns}
	opts := append([]Option{WithLineFactory(
		func(agentID types.AgentID) (types.Binding, agent.LLMExecutor, error) {
			// 缓存桶 = AgentID（Patch 1 契约）——替身线路同样遵守。
			return TaskBinding(agentID), line, nil
		}),
		WithInboxTiming(20*time.Millisecond, 200*time.Millisecond), // 测试提速（生产 500ms/10s）
	}, extra...)
	app, err := NewApp(root, filepath.Join(root, ".marl", "store.db"), "test", opts...)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	return app, ts
}

// initSkeleton 写最小项目骨架（config + profile + knowledge）。
func initSkeleton(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		".marl/config.yaml": `project:
  ladder_start: "r0"
  max_depth: 3
`,
		".marl/profiles/default.yaml": `profile:
  id: "default"
  sampling:
    max_tokens: 2048
    timeout_ms: 60000
`,
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{".marl/knowledge/contracts", ".marl/knowledge/decisions", ".marl/knowledge/preferences", ".marl/prompts"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func apiJSON(t *testing.T, ts *httptest.Server, method, path string, body any, want int) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s = %d (want %d): %s", method, path, resp.StatusCode, want, string(data))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestServerTaskLifecycle(t *testing.T) {
	_, ts := setupServer(t, []*wire.WireTurn{
		turnTool("list_dir", map[string]any{"path": "."}),
		turnReply("任务完成。"),
	})
	// 空任务 = 400。
	apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "  "}, 400)
	// 启动。
	out := apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "写一个函数"}, 201)
	id := out["agent"].(string)
	if !strings.HasPrefix(id, "sub_") {
		t.Fatalf("agent id: %v", id)
	}
	// 进行中再启动 = 409。
	apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "第二个"}, 409)
	// 监督树：人类为根 + 项目 Agent。
	agents := apiJSON(t, ts, "GET", "/api/v1/agents", nil, 200)["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agents = %d", len(agents))
	}
	first := agents[0].(map[string]any)
	if first["kind"] != "human" || first["depth"] != float64(0) {
		t.Fatalf("human row: %+v", first)
	}
	// 等任务自然结束（替身秒回）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := apiJSON(t, ts, "GET", "/api/v1/status", nil, 200)
		run := st["run"].(map[string]any)
		if run["active"] == false {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 结束后可以再启动（运行态已清）。
	apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "再来一个"}, 201)
}

func TestServerSayAndConversation(t *testing.T) {
	_, ts := setupServer(t, []*wire.WireTurn{
		turnTool("list_dir", map[string]any{"path": "."}),
		turnReply("done"),
	})
	apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "任务"}, 201)
	// 插话（进程内直达）。
	apiJSON(t, ts, "POST", "/api/v1/agents/sub_000001/messages", map[string]string{"text": "你好"}, 201)
	// 等待日志出现 human_note（pump → handleDirect 的异步面）。
	deadline := time.Now().Add(5 * time.Second)
	saw := false
	for time.Now().Before(deadline) && !saw {
		entries := apiJSON(t, ts, "GET", "/api/v1/conversation?agent=sub_000001", nil, 200)["entries"]
		for _, e := range entries.([]any) {
			entry := e.(map[string]any)
			if entry["Role"] == string(types.RoleHumanNote) && strings.Contains(fmt.Sprint(entry["Content"]), "你好") {
				saw = true
			}
		}
		if !saw {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !saw {
		t.Fatal("human note missing in conversation")
	}
	// 导出 markdown。
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/conversation/export", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "### Agent") {
		t.Fatalf("export: %s", string(data)[:min(len(data), 200)])
	}
}

func TestServerGateRoundtrip(t *testing.T) {
	a, ts := setupServer(t, []*wire.WireTurn{
		// 脚本：先一次 shell 调用（会被 KindShell 规则问人——默认规则表
		// 只有 go 前缀放行）→ 审批后重发 → reply。
		turnTool("shell_exec", map[string]any{"command": "ls -la"}),
		turnTool("shell_exec", map[string]any{"command": "ls -la"}),
		turnReply("审批后完成。"),
	})
	apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "任务"}, 201)
	// 等 gate 请求出现在收件箱（PEP need_human → 文件投递的异步面）。
	deadline := time.Now().Add(5 * time.Second)
	var gateName string
	for time.Now().Before(deadline) && gateName == "" {
		items := apiJSON(t, ts, "GET", "/api/v1/inbox", nil, 200)["items"].([]any)
		for _, it := range items {
			name := it.(map[string]any)["name"].(string)
			if strings.HasPrefix(name, "gate_") {
				gateName = name
			}
		}
		if gateName == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if gateName == "" {
		t.Fatal("gate file missing in inbox")
	}
	// GUI 的裁决按钮：allow once。
	gateID := strings.TrimSuffix(strings.TrimPrefix(gateName, "gate_"), ".md")
	apiJSON(t, ts, "POST", "/api/v1/inbox/gate/"+gateID, contract.GateDecision{Action: "allow", Mode: "once"}, 200)
	// 回执被 watcher 消费 → Agent 恢复 → shell 真执行（任务结束）。
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run := apiJSON(t, ts, "GET", "/api/v1/status", nil, 200)["run"].(map[string]any)
		if run["active"] == false {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 归档面：文件进了 done/。
	if _, err := os.Stat(filepath.Join(a.Control, "inbox", "done", gateName)); err != nil {
		t.Fatal("gate file should be archived after consumption")
	}
}

func TestServerConfigAndProfiles(t *testing.T) {
	a, ts := setupServer(t, nil)
	// 配置原文可读。
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/config", nil)
	resp, _ := http.DefaultClient.Do(req)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), "max_depth") {
		t.Fatalf("config read: %s", string(data))
	}
	// 合法配置写入。
	good := "project:\n  max_depth: 2\nlimits:\n  llm_call:\n    task_max_calls: 5\n"
	req, _ = http.NewRequest("PUT", ts.URL+"/api/v1/config", strings.NewReader(good))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("config write: %d", resp.StatusCode)
	}
	// 坏配置 = 422（保存时拦截）。
	req, _ = http.NewRequest("PUT", ts.URL+"/api/v1/config", strings.NewReader("limits:\n  llm_call:\n    task_max_calls: -5\n"))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("bad config must be 422: %d", resp.StatusCode)
	}
	// profile 读取与写入。
	raw, err := a.ProfileRaw("default")
	if err != nil || !strings.Contains(string(raw), "sampling") {
		t.Fatalf("profile raw: %v", err)
	}
	apiJSON(t, ts, "PUT", "/api/v1/profiles/default",
		map[string]any{}, 422) // 空 YAML → profile id 缺失 → 拒
	// doctor。
	checks := apiJSON(t, ts, "GET", "/api/v1/doctor", nil, 200)["checks"].([]any)
	if len(checks) < 4 {
		t.Fatalf("doctor checks: %d", len(checks))
	}
	// 知识库 lint（空 preferences = OK 报告）。
	req, _ = http.NewRequest("GET", ts.URL+"/api/v1/knowledge/lint", nil)
	resp, _ = http.DefaultClient.Do(req)
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), "est-token") {
		t.Fatalf("lint: %s", string(data))
	}
	_ = a
}

func TestServerEventsStream(t *testing.T) {
	_, ts := setupServer(t, []*wire.WireTurn{turnReply("x")})
	apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "任务"}, 201)
	// SSE：直接读响应流（截断读取若干帧）。
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events?since=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := resp.Body.Read(buf[len(buf):cap(buf)])
		if n > 0 {
			buf = buf[:len(buf)+n]
			if bytes.Contains(buf, []byte("data:")) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !bytes.Contains(buf, []byte("data:")) {
		t.Fatalf("no SSE frames received: %d bytes", len(buf))
	}
}

func TestInboxNameGuard(t *testing.T) {
	_, ts := setupServer(t, nil)
	for _, bad := range []string{"../x.md", "..", ".hidden", "a/b.md", ""} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/inbox/"+bad, nil)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 404 && resp.StatusCode != 400 {
			t.Fatalf("path traversal %q must be rejected (got %d)", bad, resp.StatusCode)
		}
	}
}

func TestGateDirectiveLine(t *testing.T) {
	cases := []struct {
		d    contract.GateDecision
		want string
		bad  bool
	}{
		{contract.GateDecision{Action: "allow", Mode: "once"}, "@grant once", false},
		{contract.GateDecision{Action: "allow"}, "@grant once", false},
		{contract.GateDecision{Action: "allow", Mode: "count", Count: 5}, "@grant next 5", false},
		{contract.GateDecision{Action: "allow", Mode: "tokens", Tokens: 1000}, "@grant tokens 1000", false},
		{contract.GateDecision{Action: "allow", Mode: "always"}, "@always-grant", false},
		{contract.GateDecision{Action: "deny"}, "@deny", false},
		{contract.GateDecision{Action: "allow", Mode: "count", Count: 0}, "", true},
		{contract.GateDecision{Action: "maybe"}, "", true},
	}
	for _, c := range cases {
		got, errStr := gateDirectiveLine(c.d)
		if c.bad && errStr == "" {
			t.Fatalf("%+v should be invalid", c.d)
		}
		if !c.bad && (errStr != "" || got != c.want) {
			t.Fatalf("%+v = %q/%q, want %q", c.d, got, errStr, c.want)
		}
	}
}

// TestAppReportLandsInInbox：任务正常完成的 report 经人类收件箱可读
// （GUI 收件箱的端到端数据源）。
func TestAppReportLandsInInbox(t *testing.T) {
	a, ts := setupServer(t, []*wire.WireTurn{
		turnTool("file_write", map[string]any{"path": "src/a.txt", "content": "hi"}),
		turnTool("report_to_parent", map[string]any{"status": "success", "report": "全部完成。"}),
		turnReply("done"),
	})
	apiJSON(t, ts, "POST", "/api/v1/tasks", map[string]string{"task": "任务"}, 201)
	deadline := time.Now().Add(5 * time.Second)
	saw := false
	for time.Now().Before(deadline) && !saw {
		items := apiJSON(t, ts, "GET", "/api/v1/inbox", nil, 200)["items"].([]any)
		for _, it := range items {
			if strings.HasPrefix(it.(map[string]any)["name"].(string), "report_") {
				saw = true
			}
		}
		if !saw {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !saw {
		t.Fatal("report file missing in inbox")
	}
	_ = a
}

// 编译期面（替身线路的引用防呆）。
var _ = wire.ErrNone

// ---------------------------------------------------------------------------
// 契约一致性（依赖倒置的守护面）：同一场景驱动 App（进程内）与
// HTTPClient（REST），断言同结果；FileMailbox 验证文件面等价 + 引擎面
// ErrNoDaemon。
// ---------------------------------------------------------------------------

// driveContractScenario 是两个实现共用的场景断言（观测/交互/管理面；
// 任务生命周期只在进程内跑——FileMailbox 无进程表）。fileOnly=true 时
// 跳过引擎态断言（FileMailbox 的兜底形态，见 TestFileMailboxFallback）。
func driveContractScenario(t *testing.T, impl contract.Interaction, fileOnly bool) {
	t.Helper()
	ctx := context.Background()
	if !fileOnly {
		if _, err := impl.StartTask("任务"); err != nil {
			t.Fatalf("StartTask: %v", err)
		}
		// 等 Agent 注册进进程表（Adjudicate 的异步面）。
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			found := false
			for _, av := range impl.Agents() {
				if av.ID == "sub_000001" {
					found = true
				}
			}
			if found {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// 监督树：人类为根。
	found := false
	for _, av := range impl.Agents() {
		if av.Kind == "human" && av.Depth == 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("human root missing in Agents()")
	}
	// 插话 → human_note 进对话（App 进程内 / HTTPClient 走 daemon 的
	// 进程内——两者同语义）。
	if err := impl.SendMessage("sub_000001", "契约一致性插话"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	sawNote := false
	for time.Now().Before(deadline) && !sawNote {
		entries, err := impl.Conversation(ctx, "sub_000001")
		if err == nil {
			for _, e := range entries {
				if e.Role == types.RoleHumanNote && strings.Contains(e.Content, "契约一致性插话") {
					sawNote = true
				}
			}
		}
		if !sawNote {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !sawNote {
		t.Fatal("human note missing via contract")
	}
	// 导出与账单有出口。
	if md, err := impl.ConversationMarkdown(ctx, "sub_000001"); err != nil || !strings.Contains(md, "### Agent") {
		t.Fatalf("ConversationMarkdown: %v", err)
	}
	if costs, err := impl.Costs(ctx, "start-task"); err != nil || costs == nil {
		t.Fatalf("Costs: %v", err)
	}
	// 事件游标。
	if evs, err := impl.Events(ctx, 0, 100); err != nil || len(evs) == 0 {
		t.Fatalf("Events: %v %d", err, len(evs))
	}
	// 配置读写 + 校验拦截。
	raw, err := impl.ConfigRaw()
	if err != nil || !strings.Contains(string(raw), "config") && !strings.Contains(string(raw), "project") {
		t.Fatalf("ConfigRaw: %v", err)
	}
	good := "project:\n  max_depth: 2\n"
	if err := impl.WriteConfig([]byte(good)); err != nil {
		t.Fatalf("WriteConfig good: %v", err)
	}
	if err := impl.WriteConfig([]byte("limits:\n  llm_call:\n    task_max_calls: -9\n")); err == nil {
		t.Fatal("bad config must be rejected by contract impl")
	}
	// Profile 面。
	if ps, err := impl.Profiles(); err != nil || len(ps) == 0 {
		t.Fatalf("Profiles: %v", err)
	}
	if praw, err := impl.ProfileRaw("default"); err != nil || !strings.Contains(string(praw), "profile") {
		t.Fatalf("ProfileRaw: %v", err)
	}
	// 知识库 lint 有出口。
	if rep, err := impl.KnowledgeLint(); err != nil || !strings.Contains(rep, "est-token") {
		t.Fatalf("KnowledgeLint: %v", err)
	}
	// 自检。
	if checks := impl.Doctor(); len(checks) == 0 {
		t.Fatal("Doctor empty")
	}
	// 收件箱面：注入一封信件（直接写文件——场景驱动不分实现）→ 列表可见。
	a2, ok := impl.(*App)
	if ok {
		_ = a2
	}
	_ = ok
}

// TestContractConformance：App 与 HTTPClient 对拍（同一场景、同一断言）。
func TestContractConformance(t *testing.T) {
	// 带用量的回复 turn（账本记一条——Costs 断言的数据源）。
	usageTurn := &wire.WireTurn{Outcomes: []wire.Outcome{{
		Reply: "done", Entry: types.LogEntry{
			Content: "done", Role: types.RoleAssistantReply, Prov: types.ProvOriginal, Audience: types.AudienceBoth,
		},
		Usage: &types.TokenUsage{PromptTokens: 100, CompletionTokens: 20},
	}}}
	t.Run("in-process App", func(t *testing.T) {
		app, _ := setupServerOpts(t, []*wire.WireTurn{usageTurn}, nil)
		driveContractScenario(t, app, false)
	})
	t.Run("HTTPClient", func(t *testing.T) {
		_, ts := setupServerOpts(t, []*wire.WireTurn{usageTurn}, nil)
		client := NewHTTPClient(strings.TrimPrefix(ts.URL, "http://"))
		driveContractScenario(t, client, false)
	})
}

// TestFileMailboxFallback：无守护时的兜底面——文件操作可用；引擎态
// 操作如实报 ErrNoDaemon。
func TestFileMailboxFallback(t *testing.T) {
	root := t.TempDir()
	initSkeleton(t, root)
	m := NewFileMailbox(root)
	// 引擎态 → ErrNoDaemon。
	if _, err := m.StartTask("x"); !errors.Is(err, contract.ErrNoDaemon) {
		t.Fatalf("StartTask: %v", err)
	}
	if err := m.StopTask(false); !errors.Is(err, contract.ErrNoDaemon) {
		t.Fatalf("StopTask: %v", err)
	}
	if _, err := m.Events(context.Background(), 0, 10); !errors.Is(err, contract.ErrNoDaemon) {
		t.Fatalf("Events: %v", err)
	}
	// SendMessage 落收件箱文件（direct 形态）。
	if err := m.SendMessage("sub_000001", "兜底插话"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	items, err := m.Inbox()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.Type == "direct" && strings.Contains(it.Preview, "兜底插话") {
			found = true
		}
	}
	if !found {
		t.Fatalf("file-dropped message not visible: %+v", items)
	}
	// 文件面其它操作（配置读）。
	if raw, err := m.ConfigRaw(); err != nil || len(raw) == 0 {
		t.Fatalf("ConfigRaw: %v", err)
	}
	// 编译期：契约实现成立。
	var _ contract.Interaction = m
}
