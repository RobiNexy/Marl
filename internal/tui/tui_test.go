package tui

// TUI 层的规格测试。三条主线：
//  1. 键位→语义映射（尤其审批决策——错误映射 = 错误授权，最不可接受的
//     缺陷，逐键表驱动锚定）
//  2. 渲染规格（各面板包含什么、按什么角色分派、未知事件的兜底）
//  3. 布局与生命周期（断点、resize、过期会话丢弃、退出确认）
//
// 全部用 gatewaytest.Stub 驱动（确定性；无真实终端）。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/frontend/gateway"
	"github.com/RobiNexy/Marl/internal/frontend/gateway/gatewaytest"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbletea"
)

// newTestModel 造一个已带首帧快照的模型（stub 预置数据 + 手工喂
// updateMsg——不走真 gateway 轮询，渲染断言确定性优先）。
func newTestModel(t *testing.T, stub *gatewaytest.Stub) Model {
	t.Helper()
	// Tick 10ms：快照持续流动（渲染断言不依赖首帧时序竞态）。
	g := gateway.NewWithInteraction(stub, gateway.Config{Tick: 10 * time.Millisecond})
	t.Cleanup(func() { _ = g.Close() })
	m := New(g)
	// 注入首帧快照（绕过轮询时序——Update 是公开状态转移入口）。
	next, _ := m.Update(updateMsg{State: snapOf(g)})
	return next.(Model)
}

// snapOf 从 gateway 取当前快照（NewWithInteraction 的首帧是同步循环首拍；
// 用 WaitForState 语义等一帧）。测试里直接驱动一轮：Subscribe 等首帧。
func snapOf(g *gateway.Gateway) *gateway.State {
	ch, cancel := g.Subscribe()
	defer cancel()
	select {
	case u := <-ch:
		return u.State
	case <-time.After(3 * time.Second):
		return nil
	}
}

// TestGateKeyMapping 逐键锚定审批卡的决策映射（错误映射=错误授权）。
func TestGateKeyMapping(t *testing.T) {
	cases := []struct {
		name      string
		keys      []string
		want      contract.GateDecision
		wantCards int
	}{
		{name: "o → allow once", keys: []string{"o"},
			want: contract.GateDecision{Action: "allow", Mode: "once"}, wantCards: 1},
		{name: "a → allow always", keys: []string{"a"},
			want: contract.GateDecision{Action: "allow", Mode: "always"}, wantCards: 1},
		{name: "d → deny", keys: []string{"d"},
			want: contract.GateDecision{Action: "deny"}, wantCards: 1},
		{name: "n 20 → allow count 20", keys: []string{"n", "2", "0", "enter", "enter"},
			want: contract.GateDecision{Action: "allow", Mode: "count", Count: 20}, wantCards: 1},
		{name: "k 5000 → allow tokens 5000", keys: []string{"k", "5", "0", "0", "0", "enter", "enter"},
			want: contract.GateDecision{Action: "allow", Mode: "tokens", Tokens: 5000}, wantCards: 1},
		{name: "n with reason → count + reason", keys: []string{"n", "5", "enter", "先看一眼", "enter"},
			want: contract.GateDecision{Action: "allow", Mode: "count", Count: 5, Reason: "先看一眼"}, wantCards: 1},
		{name: "esc cancels without decision", keys: []string{"esc"},
			want: contract.GateDecision{}, wantCards: 0},
		{name: "invalid N rejected", keys: []string{"n", "x", "enter"},
			want: contract.GateDecision{}, wantCards: 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			stub := gatewaytest.New()
			stub.InboxData = []contract.InboxItem{{Name: "gate_g1.md", Type: "gate", From: "sub_000001", Preview: "请求审批"}}
			m := newTestModel(t, stub)
			m.modal = mGate
			m.gate = gateForm{item: gateway.ActionItem{Kind: gateway.ActionGate, ID: "g1", From: "sub_000001", Title: "请求审批"},
				num: m.gate.num, reason: m.gate.reason}
			var got []contract.GateDecision
			stub.OnReplyGate = func(id string, d contract.GateDecision) error {
				got = append(got, d)
				return nil
			}
			any := interface{}(m)
			for _, k := range tc.keys {
				next, _ := any.(Model).Update(keyMsg(k))
				any = next
			}
			if len(got) != tc.wantCards {
				t.Fatalf("提交次数 want %d got %d: %+v", tc.wantCards, len(got), got)
			}
			if tc.wantCards == 1 && got[0] != tc.want {
				t.Fatalf("决策映射错误:\n want %+v\n got  %+v", tc.want, got[0])
			}
		})
	}
}

// keyMsg 把测试按键字符串折算成 tea.KeyMsg（单字符走 Runes；命名键走
// KeyType——与 Key.String() 的显示契约对齐）。
func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

// TestActionCenterNavigation 验证行动中心的导航与打开（Enter → 对应卡）。
func TestActionCenterNavigation(t *testing.T) {
	stub := gatewaytest.New()
	stub.InboxData = []contract.InboxItem{{Name: "gate_g1.md", Type: "gate"}}
	stub.EscalData = []contract.EscalationView{{ID: "escalation_x", From: "sub_000001", Question: "怎么办"}}
	m := newTestModel(t, stub)
	m.modal = mActions
	any := interface{}(m)

	// Enter 打开第一项（escalation 优先序）。
	next, _ := any.(Model).Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := next.(Model)
	if m2.modal != mEscal {
		t.Fatalf("第一项应是 escalation 卡，got modal=%d", m2.modal)
	}
	// Esc 关闭。
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if next.(Model).modal != mNone {
		t.Fatal("Esc 应关闭浮层")
	}
	// 数字直达 2 → gate 卡。
	m.modal, m.actionSel = mActions, 0
	next, _ = m.Update(keyMsg("2"))
	if next.(Model).modal != mGate {
		t.Fatalf("数字 2 应直达 gate 卡，got %d", next.(Model).modal)
	}
}

// TestEscalationReplyFlow 验证求助回复的完整路径（空回复拒绝、成功提交）。
func TestEscalationReplyFlow(t *testing.T) {
	stub := gatewaytest.New()
	m := newTestModel(t, stub)
	m.modal = mEscal
	m.esc = escForm{item: gateway.ActionItem{Kind: gateway.ActionEscalation, ID: "escalation_x"}}
	m.esc.reply = textarea.New()
	m.esc.reply.Placeholder = ""
	any := interface{}(m)

	// 空回复 + ctrl+s → 拒绝。
	next, _ := any.(Model).Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if next.(Model).modal != mEscal {
		t.Fatal("空回复必须保留卡片")
	}
	// 写回复 + ctrl+s → 提交并关闭。
	m2 := next.(Model)
	m2.esc.reply.SetValue("用位运算方案")
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if next.(Model).modal != mNone {
		t.Fatal("成功提交后卡片应关闭")
	}
}

// TestSlashCommandParse 表驱动命令解析（parseSlash 的契约）。
func TestSlashCommandParse(t *testing.T) {
	cases := []struct {
		in   string
		name string
		args string
		ok   bool
	}{
		{"/start 任务内容", "/start", "任务内容", true},
		{"/stop force", "/stop", "force", true},
		{"/stop", "/stop", "", true},
		{"/say sub_000001 你好", "/say", "sub_000001 你好", true},
		{"/help", "/help", "", true},
		{"/", "/", "", true},
		{"hello", "", "", false},
		{"你好", "", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			name, args, ok := parseSlash(tc.in)
			if ok != tc.ok || name != tc.name || args != tc.args {
				t.Fatalf("parseSlash(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, name, args, ok, tc.name, tc.args, tc.ok)
			}
		})
	}
}

// TestInputSendMessage 验证普通输入 → SendMessage 的转发与目标选择。
func TestInputSendMessage(t *testing.T) {
	stub := gatewaytest.New()
	stub.AgentsData = []contract.AgentView{
		{ID: "sub_000001", Parent: "human:1000", Kind: "agent", Depth: 1},
		{ID: "human:1000", Kind: "human", Depth: 0},
	}
	m := newTestModel(t, stub)
	m.selected = "sub_000001"
	any := interface{}(m)

	// 按 'i' 进入输入模式（真实按键流；textinput.Focus 是 'i' 的副作用）。
	next, _ := any.(Model).Update(keyMsg("i"))
	any = next
	// 输入文本 + Enter。
	for _, r := range "检查一下构建" {
		next, _ = any.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		any = next
	}
	next, _ = any.(Model).Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(stub.SentMessages) != 1 || stub.SentMessages[0] != "sub_000001|检查一下构建" {
		t.Fatalf("消息未按契约转发: %+v", stub.SentMessages)
	}
	if toast := next.(Model).toast; !strings.Contains(toast, "已送达") {
		t.Fatalf("应有送达提示（含下一轮编排语义）, got %q", toast)
	}
}

// TestSlashExec 表驱动命令执行（成功/失败/未知命令）。
func TestSlashExec(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool // toast 是否为错误
		check   func(t *testing.T, m Model, stub *gatewaytest.Stub)
	}{
		{name: "/start", input: "/start 写个求解器", check: func(t *testing.T, m Model, stub *gatewaytest.Stub) {
			if len(stub.StartedTasks) != 1 || stub.StartedTasks[0] != "写个求解器" {
				t.Fatalf("StartTask 未转发: %+v", stub.StartedTasks)
			}
		}},
		{name: "/start missing args", input: "/start", wantErr: true, check: func(t *testing.T, m Model, stub *gatewaytest.Stub) {
			if len(stub.StartedTasks) != 0 {
				t.Fatal("空参数不应启动")
			}
		}},
		{name: "/say", input: "/say sub_000001 注意缓存", check: func(t *testing.T, m Model, stub *gatewaytest.Stub) {
			if len(stub.SentMessages) != 1 {
				t.Fatalf("SendMessage 未转发: %+v", stub.SentMessages)
			}
		}},
		{name: "unknown command", input: "/nope", wantErr: true},
		{name: "/quit", input: "/quit"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			stub := gatewaytest.New()
			m := newTestModel(t, stub)
			any := interface{}(m)
			next, _ := any.(Model).submitInput(tc.input)
			mm := next.(Model)
			if mm.toastErr != tc.wantErr {
				t.Fatalf("toastErr want %v got %v (toast=%q)", tc.wantErr, mm.toastErr, mm.toast)
			}
			if tc.check != nil {
				tc.check(t, mm, stub)
			}
		})
	}
}

// TestConversationRoleRendering 验证角色 → 气泡分派（thinking 折叠契约）。
func TestConversationRoleRendering(t *testing.T) {
	stub := gatewaytest.New()
	m := newTestModel(t, stub)
	m.conv = []*types.LogEntry{
		{Role: types.RoleUserInput, Content: "任务提示"},
		{Role: types.RoleThinking, Content: "思考内容不应默认出现", TokenEst: 1200},
		{Role: types.RoleAssistantReply, Content: "回复正文"},
		{Role: types.RoleToolResult, Content: "file_read 返回"},
		{Role: types.RoleEscalation, Content: "卡住了"},
	}
	m.selected = "sub_000001"
	out := stripANSI(m.renderConversation(80))
	if !strings.Contains(out, "任务提示") || !strings.Contains(out, "回复正文") {
		t.Fatalf("正文缺失:\n%s", out)
	}
	if strings.Contains(out, "思考内容不应默认出现") {
		t.Fatal("thinking 默认必须折叠（隐私+噪声双重理由）")
	}
	if !strings.Contains(out, "thinking") || !strings.Contains(out, "1.2k") {
		t.Fatalf("折叠行应有摘要与 token 数:\n%s", out)
	}
	if !strings.Contains(out, "工具") || !strings.Contains(out, "求助") {
		t.Fatalf("工具/求助标签缺失:\n%s", out)
	}
	// 展开 thinking。
	m.showThinking = true
	out = stripANSI(m.renderConversation(80))
	if !strings.Contains(out, "思考内容不应默认出现") {
		t.Fatal("展开后 thinking 正文应可见")
	}
}

// TestEventFallbackRendering 未知 Action 的兜底契约（开放集合不崩）。
func TestEventFallbackRendering(t *testing.T) {
	stub := gatewaytest.New()
	m := newTestModel(t, stub)
	m.st.Events = []gateway.DomainEvent{
		eventOf(1, "sub_000001", "totally_new_action", "x"),
		eventOf(2, "sub_000002", "gate_request", "shell"),
	}
	out := stripANSI(m.renderEvents(100))
	if !strings.Contains(out, "totally_new_action") {
		t.Fatalf("未知 action 原文必须透传:\n%s", out)
	}
	if !strings.Contains(out, "#1") || !strings.Contains(out, "#2") {
		t.Fatalf("Seq 应显示:\n%s", out)
	}
}

// eventOf 造审计事件（测试辅助；DomainEvent 内嵌 store.AuditEvent）。
func eventOf(seq int64, agent, action, target string) gateway.DomainEvent {
	return gateway.DomainEvent{
		AuditEvent: store.AuditEvent{Seq: seq, AgentID: types.AgentID(agent), Action: action, Target: target},
		Kind:       gateway.KindOther,
	}
}

// TestViewSmoke 三档宽度的整屏冒烟（不 panic + 关键区块在场）。
func TestViewSmoke(t *testing.T) {
	stub := gatewaytest.New()
	stub.AgentsData = []contract.AgentView{
		{ID: "sub_000001", Parent: "human:1000", Kind: "agent", Depth: 1, State: "running", StartedAt: time.Now().Add(-2 * time.Minute)},
		{ID: "human:1000", Kind: "human", Depth: 0, State: "idle"},
	}
	stub.InboxData = []contract.InboxItem{{Name: "gate_g1.md", Type: "gate", From: "sub_000001", Preview: "审批请求"}}
	stub.EscalData = []contract.EscalationView{{ID: "escalation_x", From: "sub_000001", Question: "用哪种方案"}}
	m := newTestModel(t, stub)
	for _, w := range []int{200, 100, 60} {
		w := w
		t.Run(fmt.Sprintf("w%d", w), func(t *testing.T) {
			next, _ := interface{}(m).(Model).Update(tea.WindowSizeMsg{Width: w, Height: 30})
			mm := next.(Model)
			out := stripANSI(mm.View())
			if out == "" {
				t.Fatal("View 不应为空")
			}
			if !strings.Contains(out, "待你决策") {
				t.Fatalf("Action 横幅应出现在宽度 %d:\n%s", w, out)
			}
			// 树面板只在 ≥80 宽度下可见（单栏兜底没有树面板——契约见
			// mainArea 的断点表）。
			if w >= 80 && !strings.Contains(out, "监督树") {
				t.Fatalf("树面板标题缺失 (w=%d):\n%s", w, out)
			}
			if !strings.Contains(out, "human:1000") {
				t.Fatalf("human 根应在树中 (w=%d):\n%s", w, out)
			}
		})
	}
	// 极小终端不 panic。
	next, _ := interface{}(m).(Model).Update(tea.WindowSizeMsg{Width: 20, Height: 6})
	_ = stripANSI(next.(Model).View())
}

// TestQuitConfirmRunning 验证退出确认（任务运行中 q 需二次确认；输入
// 焦点下 q 是字符——面板键只在面板焦点下生效，与键路由契约一致）。
func TestQuitConfirmRunning(t *testing.T) {
	stub := gatewaytest.New()
	m := newTestModel(t, stub)
	// 置运行态 + 面板焦点。
	m.st.Run.Active = true
	m.focus = FocusWork
	any := interface{}(m)
	next, cmd := any.(Model).Update(keyMsg("q"))
	m2 := next.(Model)
	if m2.modal != mQuitConfirm {
		t.Fatalf("运行中 q 应弹确认，got modal=%d", m2.modal)
	}
	_ = cmd
	// y → Quit 命令。
	_, cmd = m2.Update(keyMsg("y"))
	if cmd == nil {
		t.Fatal("y 应产生 Quit 命令")
	}
	// n → 回到界面。
	next, _ = m2.Update(keyMsg("n"))
	if next.(Model).modal != mNone {
		t.Fatal("n 应取消退出")
	}
	// 空闲时 q 直接退出（无确认）。
	m3 := newTestModel(t, gatewaytest.New())
	m3.focus = FocusWork
	_, cmd = interface{}(m3).(Model).Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("空闲 q 应直接退出")
	}
}

// TestWindowSizeBeforeSnapshot 回归（真机实测）：WindowSizeMsg 先于首帧
// 快照到达时，resize→syncConvView→renderEvents 路径必须 nil 安全。
func TestWindowSizeBeforeSnapshot(t *testing.T) {
	g := gateway.NewWithInteraction(gatewaytest.New(), gateway.Config{Tick: time.Hour})
	t.Cleanup(func() { _ = g.Close() })
	m := New(g) // m.st == nil（快照未到）
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	mm := next.(Model)
	if mm.st != nil {
		t.Fatal("前置条件：快照应为 nil")
	}
	_ = stripANSI(mm.View()) // View 的 nil 守护
}

// TestExpiredConvDropped 验证会话缓存的键一致性（切换 agent 丢弃过期响应）。
func TestExpiredConvDropped(t *testing.T) {
	stub := gatewaytest.New()
	m := newTestModel(t, stub)
	m.convFor = "sub_000001"
	// 过期响应（agent 已切换）。
	next, _ := interface{}(m).(Model).Update(convMsg{agent: "sub_old", entries: []*types.LogEntry{{Content: "过期内容", Role: types.RoleAssistantReply}}})
	mm := next.(Model)
	if len(mm.conv) != 0 {
		t.Fatal("过期会话响应必须丢弃")
	}
	// 匹配响应。
	next, _ = mm.Update(convMsg{agent: "sub_000001", entries: []*types.LogEntry{{Content: "新内容", Role: types.RoleAssistantReply}}})
	if len(next.(Model).conv) != 1 {
		t.Fatal("匹配响应应被接受")
	}
}

// TestGatewayConversationPassthrough 验证 TUI 经 gateway 读会话（集成线）。
func TestGatewayConversationPassthrough(t *testing.T) {
	stub := gatewaytest.New()
	m := newTestModel(t, stub)
	ctx := context.Background()
	entries, err := m.gw.Conversation(ctx, "sub_000001")
	if err != nil || len(entries) == 0 {
		t.Fatalf("gateway.Conversation: %v %d", err, len(entries))
	}
}
