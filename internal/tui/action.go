package tui

// 键路由：焦点与浮层的分发矩阵（每个分支的契约 = 转移的状态 + 副作用
// 命令；帮助面板的 keyHelp 表是本文件的用户面文档，两处由测试锚定）。

import (
	"strconv"
	"strings"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/frontend/gateway"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// onKey 是全局键路由（优先级：浮层 > 输入焦点 > 全局面板键）。
//
// 路由优先级契约（从高到低）：
//  1. 浮层（modal）独占键盘——决策卡打开时，面板键不生效（防误触）
//  2. 输入焦点独占字符键（Tab/Esc 逃生）
//  3. 全局面板键（Tab/数字/单字母）
func (m Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.modal != mNone {
		return m.onModalKey(msg)
	}
	if m.focus == FocusInput {
		return m.onInputKey(msg)
	}
	return m.onPaneKey(msg)
}

// onModalKey 是浮层的键面（互斥独占）。
func (m Model) onModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.modal {
	case mQuitConfirm:
		switch msg.String() {
		case "y", "Y":
			return m, tea.Quit
		case "n", "esc", "q":
			m.modal = mNone
			m.pendingG = ""
		}
		return m, nil
	case mHelp:
		m.modal = mNone
		return m, nil
	case mActions:
		return m.onActionsKey(msg)
	case mGate:
		return m.onGateKey(msg)
	case mEscal:
		return m.onEscalKey(msg)
	case mDiscuss:
		return m.onDiscussKey(msg)
	}
	m.modal = mNone
	return m, nil
}

// onActionsKey 是行动中心列表的键面（Enter 进入对应决策卡；数字直达）。
func (m Model) onActionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := m.actionItems()
	switch msg.String() {
	case "esc", "q":
		m.modal = mNone
	case "up", "k":
		if m.actionSel > 0 {
			m.actionSel--
		}
	case "down", "j":
		if m.actionSel < len(items)-1 {
			m.actionSel++
		}
	case "enter":
		if len(items) > 0 {
			m.openCard(items[m.actionSel])
		}
	default:
		// 数字 1-9 直达（StatusLine 的 "1/2/3 直达" 承诺）。
		if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= len(items) {
			m.actionSel = n - 1
			m.openCard(items[n-1])
		}
	}
	return m, nil
}

// openCard 按待办类型打开对应决策卡（类型分派的单一入口）。
func (m *Model) openCard(item gateway.ActionItem) {
	switch item.Kind {
	case gateway.ActionGate:
		m.modal = mGate
		m.gate = gateForm{item: item, step: 0}
		m.gate.num.Blur()
		m.gate.reason.Blur()
	case gateway.ActionEscalation:
		m.modal = mEscal
		m.esc = escForm{item: item}
		m.esc.reply = textarea.New()
		m.esc.reply.Placeholder = "写下回复…（Ctrl+S 提交，Esc 取消）"
		m.esc.reply.Focus()
		m.esc.reply.CharLimit = 4096
	case gateway.ActionDiscuss:
		m.modal = mDiscuss
		m.disc = discForm{item: item}
		m.disc.note = textarea.New()
		m.disc.note.Placeholder = "批注…（Ctrl+S 只发批注；p 通过；Esc 取消）"
		m.disc.note.Focus()
		m.disc.note.CharLimit = 4096
	}
}

// onGateKey 是审批卡的键面（决策语义映射是契约：测试锚定）。
//
// 决策语义（与 gate.GateDecision/文件通道语法一一对应）：
//
//	o → allow/once        n → allow/count(N)
//	k → allow/tokens(N)   a → allow/always
//	d → deny              Esc → 取消（不裁决）
//
// N 模式走两步：step1 输 N（Tab 可切到理由；理由全程可填），Enter 推进。
func (m Model) onGateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	g := &m.gate
	switch g.step {
	case 0: // 选模式
		switch msg.String() {
		case "esc":
			m.modal = mNone
		case "o":
			return m.submitGate("allow", "once", 0)
		case "n":
			g.mode, g.step, g.focusN = "count", 1, true
			m.setGateFocus()
		case "k":
			g.mode, g.step, g.focusN = "tokens", 1, true
			m.setGateFocus()
		case "a":
			return m.submitGate("allow", "always", 0)
		case "d":
			return m.submitGate("deny", "", 0)
		}
	case 1, 2: // 输入 N（step1）→ 理由（step2）
		if msg.String() == "esc" {
			m.modal = mNone
			return m, nil
		}
		if msg.String() == "tab" {
			// step1 内 Tab 在 N/理由间切换；step2 只剩理由。
			g.step = 2
			g.focusN = false
			m.setGateFocus()
			return m, nil
		}
		if msg.String() == "enter" {
			if g.step == 1 {
				n, err := strconv.ParseInt(strings.TrimSpace(g.num.Value()), 10, 64)
				if err != nil || n <= 0 {
					m.setToast("需要一个正整数（如 20）", true)
					return m, nil
				}
				g.pendingN = n
				g.step = 2
				g.focusN = false
				m.setGateFocus()
				return m, nil
			}
			// step2：理由（可空）→ 提交。
			return m.submitGate("allow", g.mode, g.pendingN)
		}
		var cmd tea.Cmd
		if g.focusN {
			g.num, cmd = g.num.Update(msg)
		} else {
			g.reason, cmd = g.reason.Update(msg)
		}
		return m, cmd
	}
	return m, nil
}

// setGateFocus 同步审批卡 N/理由两个输入框的焦点态。
func (m *Model) setGateFocus() {
	if m.gate.focusN {
		m.gate.num.Focus()
		m.gate.reason.Blur()
	} else {
		m.gate.num.Blur()
		m.gate.reason.Focus()
	}
}

// submitGate 提交审批决策（乐观语义：提交后立关卡片，生效确认来自
// 后续 gate_decision 审计事件——UI 不谎报"已生效"）。
//
// 字段互斥契约：count 模式只填 Count；tokens 模式只填 Tokens（GateDecision
// 的两个字段语义不同——都填会让 gate 兑现层二义）。
func (m Model) submitGate(action, mode string, n int64) (tea.Model, tea.Cmd) {
	reason := strings.TrimSpace(m.gate.reason.Value())
	d := contract.GateDecision{Action: action, Reason: reason}
	switch mode {
	case "count":
		d.Mode, d.Count = mode, int(n)
	case "tokens":
		d.Mode, d.Tokens = mode, n
	case "always", "once":
		d.Mode = mode
	}
	err := m.gw.ReplyGate(m.gate.item.ID, d)
	if err != nil {
		m.setToast("审批提交失败："+err.Error(), true)
		return m, nil // 卡片保留：错误可重试
	}
	m.modal = mNone
	m.setToast("审批已提交（结构化通道零延迟生效）", false)
	return m, nil
}

// onEscalKey 是求助回复卡的键面（Ctrl+S 提交；textarea 的 Enter 换行）。
func (m Model) onEscalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.modal = mNone
		return m, nil
	case "ctrl+s":
		reply := strings.TrimSpace(m.esc.reply.Value())
		if reply == "" {
			m.setToast("回复不能为空（空回复会被读侧忽略）", true)
			return m, nil
		}
		if err := m.gw.ReplyEscalation(m.esc.item.ID, reply); err != nil {
			m.setToast("回复失败："+err.Error(), true)
			return m, nil
		}
		m.modal = mNone
		m.setToast("求助已回复（写 done/，agent 将解除阻塞）", false)
		return m, nil
	}
	var cmd tea.Cmd
	m.esc.reply, cmd = m.esc.reply.Update(msg)
	return m, cmd
}

// onDiscussKey 是讨论裁决卡的键面（p=通过落地；Ctrl+S=只发批注）。
func (m Model) onDiscussKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.modal = mNone
		return m, nil
	case "p": // 通过：批注 + @approve
		if err := m.gw.ReplyDiscussion(m.disc.item.ID, strings.TrimSpace(m.disc.note.Value()), true); err != nil {
			m.setToast("讨论回复失败："+err.Error(), true)
			return m, nil
		}
		m.modal = mNone
		m.setToast("已通过：草稿将落地（author=human）", false)
		return m, nil
	case "ctrl+s": // 批注（不裁决）
		if err := m.gw.ReplyDiscussion(m.disc.item.ID, strings.TrimSpace(m.disc.note.Value()), false); err != nil {
			m.setToast("批注失败："+err.Error(), true)
			return m, nil
		}
		m.modal = mNone
		m.setToast("批注已提交（agent 下一轮响应）", false)
		return m, nil
	}
	var cmd tea.Cmd
	m.disc.note, cmd = m.disc.note.Update(msg)
	return m, cmd
}

// onInputKey 是输入焦点的键面（Enter 提交；Esc 归还焦点）。
func (m Model) onInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.focus = FocusWork
		m.input.Blur()
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		return m.submitInput(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// onPaneKey 是面板焦点的键面（Tab 循环/数字直达/单字母动作/双键序列）。
func (m Model) onPaneKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	// g 前缀序列（g t/e/i → 工作区页签）。
	if m.gPending {
		m.gPending = false
		switch key {
		case "t":
			m.tab, m.focus = TabConv, FocusWork
		case "e":
			m.tab, m.focus = TabEvents, FocusWork
		case "i":
			m.tab, m.focus = TabInspect, FocusWork
		}
		return m, nil
	}
	switch key {
	case "tab", "shift+tab":
		return m, m.cycleFocus(key == "tab")
	case "1":
		m.focus = FocusTree
	case "2":
		m.focus = FocusWork
	case "3":
		m.focus = FocusSide
	case "g":
		m.gPending = true
	case "a":
		if items := m.actionItems(); len(items) > 0 {
			m.modal, m.actionSel = mActions, 0
		} else {
			m.setToast("没有待你决策的事项", false)
		}
	case "/":
		m.focus = FocusInput
		m.input.SetValue("/")
		m.input.Focus()
		m.input.CursorEnd()
	case "i":
		m.focus = FocusInput
		m.input.Focus()
	case "s":
		m.sideOpen = !m.sideOpen
	case "t":
		m.showThinking = !m.showThinking
		m.syncConvView()
	case "f":
		m.follow = !m.follow
	case "?":
		m.modal = mHelp
	case "q":
		if m.st != nil && m.st.Run.Active {
			m.modal, m.pendingG = mQuitConfirm, "q"
		} else {
			return m, tea.Quit
		}
	case "up", "k":
		m.moveTree(-1)
	case "down", "j":
		m.moveTree(1)
	case "enter":
		if m.selected != "" {
			m.focus = FocusWork
			m.tab = TabConv
			m.syncConvView()
		}
	default:
		// 数字 1-9 在行动中心有专项语义——无待办时无害穿透。
	}
	return m, nil
}

// cycleFocus 循环焦点（Tab 正向 / Shift+Tab 反向；含输入框）。
func (m Model) cycleFocus(fwd bool) tea.Cmd {
	order := []Focus{FocusTree, FocusWork, FocusSide, FocusInput}
	cur := 0
	for i, f := range order {
		if f == m.focus {
			cur = i
		}
	}
	if fwd {
		cur = (cur + 1) % len(order)
	} else {
		cur = (cur + len(order) - 1) % len(order)
	}
	m.focus = order[cur]
	if m.focus == FocusInput {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	return nil
}

// moveTree 移动树选择（越界钳制；human 根在展平序首位）。
func (m *Model) moveTree(delta int) {
	if len(m.treeOrder) == 0 && humanRootID(m.st) != "" {
		m.treeOrder = append([]string{}, humanRootID(m.st))
	}
	if len(m.treeOrder) == 0 {
		return
	}
	// 当前选中在展平序中的位置；未选中 → 视作 -1（下一步落到 0）。
	cur := -1
	for i, id := range m.treeOrder {
		if id == m.selected {
			cur = i
		}
	}
	next := cur + delta
	if next < 0 {
		next = 0
	}
	if next >= len(m.treeOrder) {
		next = len(m.treeOrder) - 1
	}
	if next != cur {
		m.selected = m.treeOrder[next]
	}
}

// submitInput 处理输入提交（斜杠命令 vs 普通消息——parseSlash 的消费点）。
func (m Model) submitInput(text string) (tea.Model, tea.Cmd) {
	if text == "" {
		m.focus = FocusWork
		return m, nil
	}
	if name, args, isCmd := parseSlash(text); isCmd {
		return m.execCommand(name, args)
	}
	// 普通消息 → 选中节点（或 target 覆盖）。
	to := m.target
	if to == "" {
		to = m.selected
	}
	if to == "" {
		m.setToast("没有目标 agent——先在树中选择，或用 /say <agent>", true)
		return m, nil
	}
	if err := m.gw.SendMessage(to, text); err != nil {
		m.setToast("发送失败："+err.Error(), true)
		return m, nil
	}
	m.setToast("已送达 "+to+"（该 agent 下一轮编排可见）", false)
	return m, nil
}

// execCommand 执行斜杠命令（commands 注册表的分派面；未知命令报 toast）。
func (m Model) execCommand(name, args string) (tea.Model, tea.Cmd) {
	switch name {
	case "/start":
		if args == "" {
			m.setToast("用法：/start <任务描述>", true)
			return m, nil
		}
		if err := m.gw.StartTask(args); err != nil {
			m.setToast("启动失败："+err.Error(), true)
			return m, nil
		}
		m.setToast("任务已启动", false)
	case "/stop":
		force := strings.TrimSpace(args) == "force"
		if err := m.gw.StopTask(force); err != nil {
			m.setToast("停止失败："+err.Error(), true)
			return m, nil
		}
		m.setToast("任务已停止", false)
	case "/say":
		parts := strings.SplitN(args, " ", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			m.setToast("用法：/say <agent> <文本>", true)
			return m, nil
		}
		if err := m.gw.SendMessage(parts[0], parts[1]); err != nil {
			m.setToast("发送失败："+err.Error(), true)
			return m, nil
		}
		m.setToast("已送达 "+parts[0], false)
	case "/tree":
		m.focus = FocusTree
	case "/events":
		m.tab, m.focus = TabEvents, FocusWork
	case "/inspect":
		m.tab, m.focus = TabInspect, FocusWork
	case "/cost":
		m.sideOpen, m.focus = true, FocusSide
	case "/help":
		m.modal = mHelp
	case "/doctor":
		m.setToast("doctor：见 marl doctor（TUI 暂以 CLI 为准）", false)
	case "/quit":
		return m, tea.Quit
	default:
		m.setToast("未知命令 "+name+"（/? 查看命令表）", true)
	}
	return m, nil
}
