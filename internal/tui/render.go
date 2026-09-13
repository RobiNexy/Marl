package tui

// 渲染层：布局组合 + 各面板的纯渲染（输入 = Model 状态，输出 = 字符串；
// 渲染不做状态转移——View 与 Update 的分离纪律）。

import (
	"fmt"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/frontend/gateway"
	"github.com/RobiNexy/Marl/internal/types"
)

// View 组装整屏（自上而下：TopBar → 主区三栏 → Action 横幅 → 输入 → 状态行）。
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "正在启动…"
	}
	if m.st == nil {
		return "连接 Marl（进程内或守护进程）…\n\n（若长期停留：确认在 Marl 项目根目录，且 fossil / DEEPSEEK_API_KEY 就绪——marl doctor）"
	}
	parts := []string{
		m.topBar(),
		m.mainArea(),
		m.actionBanner(),
		m.inputBar(),
		m.statusLine(),
	}
	if m.modal != mNone {
		parts = append(parts, m.modalView())
	}
	return strings.Join(filterEmpty(parts), "\n")
}

// filterEmpty 去掉空段（折叠 Action 横幅等条件区块）。
func filterEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if strings.TrimSpace(stripANSI(s)) != "" {
			out = append(out, s)
		}
	}
	return out
}

// stripANSI 是 ANSI 转义剥离（空判断用；显示长度计算不在这里——
// lipgloss 的 Width 自带 ANSI 感知）。
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// topBar 是顶栏：项目态 · 运行态 · 成本 · 连接模式（1 行）。
func (m Model) topBar() string {
	st := m.st
	run := st.Run
	runState := "空闲"
	if run.Active {
		runState = "任务运行中"
	}
	cost := "成本 -"
	if st.CostView.TotalTokens > 0 {
		cost = fmt.Sprintf("成本 ¥%.4f %s · %s tok · 缓存 %.0f%%",
			st.CostView.TotalCost, st.CostView.Currency,
			humanCount(st.CostView.TotalTokens), st.CostView.CacheHitRate*100)
	}
	return strings.Join(filterEmpty([]string{
		m.th.Style.PaneTitle.Render(" Marl "),
		fmt.Sprintf("%s · %s", runState, clip(run.Task, 40)),
		m.th.Style.Accent.Render(cost),
		m.th.Style.Dim.Render("[" + st.Conn.Mode + "]"),
	}), " · ")
}

// mainArea 是主区三栏（宽度断点：≥120 三栏 / ≥80 两栏 / 其余单栏）。
func (m Model) mainArea() string {
	w, h := m.width, m.workHeight()
	switch {
	case w >= 120:
		tw := int(float64(w) * 0.22)
		sw := int(float64(w) * 0.24)
		cw := w - tw - sw - 4
		return lipglossJoinHorizontal(
			m.paneBox("监督树", m.renderTree(tw), tw, h, m.focus == FocusTree),
			m.paneBox(m.tabTitle(), m.renderWork(cw), cw, h, m.focus == FocusWork),
			m.paneBox("成本与告警", m.renderSidebar(sw), sw, h, m.focus == FocusSide),
		)
	case w >= 80:
		tw := w / 3
		cw := w - tw - 3
		return lipglossJoinHorizontal(
			m.paneBox("监督树", m.renderTree(tw), tw, h, m.focus == FocusTree),
			m.paneBox(m.tabTitle(), m.renderWork(cw), cw, h, m.focus == FocusWork),
		)
	default:
		return m.paneBox(m.tabTitle(), m.renderWork(w-2), w-2, h, true)
	}
}

// workHeight 是主区可用高度（总高 - 顶栏/横幅/输入/状态行的预留）。
func (m *Model) workHeight() int {
	reserve := 4 // 顶栏 + 输入 + 状态行 + 空行
	if m.actionItems() != nil {
		reserve++
	}
	return max(m.height-reserve, 5)
}

// workWidth 是工作区宽度（窄屏单栏 = 全宽）。
func (m *Model) workWidth() int {
	if m.width < 120 && m.width >= 80 {
		return m.width - m.width/3 - 3
	}
	if m.width >= 120 {
		return m.width - int(float64(m.width)*0.22) - int(float64(m.width)*0.24) - 4
	}
	return m.width - 2
}

// tabTitle 是工作区标题（当前页签）。
func (m Model) tabTitle() string {
	switch m.tab {
	case TabEvents:
		return "事件流（不可篡改审计）"
	case TabInspect:
		return "检视"
	default:
		return "对话 · " + m.selectedOr()
	}
}

// selectedOr 是选中节点的显示名（空 → 全局）。
func (m *Model) selectedOr() string {
	if m.selected == "" {
		return "(选择 agent)"
	}
	return m.selected
}

// paneBox 面板盒（焦点边框高亮 + 标题行）。
func (m Model) paneBox(title, content string, width, height int, focused bool) string {
	style := m.th.Style.PaneBorder
	if focused {
		style = m.th.Style.FocusedBorder
	}
	head := m.th.Style.PaneTitle.Render(" " + title + " ")
	return style.Render(head + "\n" + padLines(content, width, height-2))
}

// renderTree 渲染监督树（图标/状态/时长/阻塞副行；选中高亮）。
func (m Model) renderTree(width int) string {
	if m.st == nil {
		return ""
	}
	var b strings.Builder
	gateway.Walk(m.st.Tree, func(n *gateway.TreeNode) {
		glyph, style := m.th.kindGlyph(n.Kind, n.Depth)
		sg, sstyle := m.th.stateGlyph(n.State, n.BlockReason)
		marker := "  "
		if n.ID == m.selected {
			marker = m.th.Style.Selected.Render("▸ ")
		}
		line := fmt.Sprintf("%s%s%s %s %s %s",
			indentOf(max(n.Depth-0, 0)), marker,
			glyph, style.Render(n.ID),
			sstyle.Render(sg+" "+n.State),
			m.th.Style.Dim.Render(humanDur(time.Since(n.StartedAt))),
		)
		if n.BlockReason != "" {
			line += "\n" + indentOf(n.Depth+1) + m.th.Style.Blocked.Render("└ ⏳ "+n.BlockReason)
		}
		b.WriteString(line + "\n")
	})
	return b.String()
}

// renderWork 渲染工作区（按页签分派）。
func (m Model) renderWork(width int) string {
	switch m.tab {
	case TabEvents:
		return m.evView.View()
	case TabInspect:
		return m.renderInspect(width)
	default:
		return m.convView.View()
	}
}

// renderConversation 渲染会话（角色 → 气泡；thinking 默认折叠）。
func (m Model) renderConversation(width int) string {
	var b strings.Builder
	for _, e := range m.conv {
		switch e.Role {
		case types.RoleThinking:
			if !m.showThinking {
				b.WriteString(m.th.Style.Thinking.Render(fmt.Sprintf("💭 thinking（%s tok，按 t 展开）", humanCount(int64(e.TokenEst)))) + "\n")
				continue
			}
			b.WriteString(m.th.Style.Thinking.Render("💭 thinking:") + "\n" + wrapText(e.Content, width) + "\n\n")
		case types.RoleUserInput:
			b.WriteString(m.th.Style.Root.Render("你") + "\n" + wrapText(e.Content, width) + "\n\n")
		case types.RoleHumanNote:
			b.WriteString(m.th.Style.Accent.Render("✍ 插话") + "\n" + wrapText(e.Content, width) + "\n\n")
		case types.RoleToolResult:
			b.WriteString(m.th.Style.Dim.Render("🔧 工具") + "\n" + m.th.Style.Dim.Render(clip(e.Content, min(width*4, 600))) + "\n\n")
		case types.RoleSubTaskResult:
			b.WriteString(m.th.Style.AgentRoot.Render("📦 子任务回报") + "\n" + wrapText(e.Content, width) + "\n\n")
		case types.RoleEscalation:
			b.WriteString(m.th.Style.Blocked.Render("🆘 求助（见行动中心）") + "\n" + wrapText(e.Content, width) + "\n\n")
		case types.RoleAssistantReply:
			b.WriteString(m.th.Style.AgentRoot.Render(m.selectedOr()) + "\n" + wrapText(e.Content, width) + "\n\n")
		default:
			b.WriteString(m.th.Style.Dim.Render("· "+string(e.Role)) + "\n" + m.th.Style.Dim.Render(clip(e.Content, 200)) + "\n\n")
		}
	}
	return b.String()
}

// renderEvents 渲染事件流（图标 + Seq + agent + action + target）。
func (m Model) renderEvents(width int) string {
	if len(m.st.Events) == 0 {
		return m.th.Style.Dim.Render("（暂无事件——启动任务后此处实时流动）")
	}
	var b strings.Builder
	for _, ev := range m.st.Events {
		b.WriteString(m.renderEventLine(ev) + "\n")
	}
	return b.String()
}

// renderEventLine 是事件的单行渲染（未知 Kind 的兜底样式是契约——开放集合）。
func (m Model) renderEventLine(ev gateway.DomainEvent) string {
	icon := m.th.Icons.Orph
	style := m.th.Style.Dim
	switch ev.Kind {
	case gateway.KindGate:
		icon, style = m.th.Icons.Gate, m.th.Style.Blocked
	case gateway.KindSpawn:
		icon = m.th.Icons.Spawn
	case gateway.KindWatchdog:
		icon, style = m.th.Icons.Watch, m.th.Style.Crashed
	case gateway.KindModel:
		icon = m.th.Icons.Model
	case gateway.KindDiscussion:
		icon = m.th.Icons.Disc
	case gateway.KindEscalation:
		icon, style = m.th.Icons.Escal, m.th.Style.Blocked
	case gateway.KindCommit:
		icon = m.th.Icons.Commit
	case gateway.KindAgentState, gateway.KindOrchestrate:
		style = m.th.Style.Dim
	}
	return fmt.Sprintf("%s %s %s %s %s",
		m.th.Style.Dim.Render(fmt.Sprintf("#%d", ev.Seq)),
		m.th.Style.Dim.Render(ev.Timestamp.Format("15:04:05")),
		icon,
		style.Render(clip(string(ev.AgentID), 12)),
		m.th.Style.Dim.Render(clip(ev.Action+" "+ev.Target, 48)),
	)
}

// renderInspect 渲染检视（选中节点的绑定/证据/写作用域摘要）。
func (m Model) renderInspect(width int) string {
	var b strings.Builder
	node := m.findNode(m.selected)
	if node == nil {
		return m.th.Style.Dim.Render("（在树中选择一个 agent）")
	}
	fmt.Fprintf(&b, "ID: %s\n状态: %s\n深度: %d\n启动: %s\n", node.ID, node.State, node.Depth, node.StartedAt.Format("15:04:05"))
	if node.BlockReason != "" {
		fmt.Fprintf(&b, "挂起: %s（自 %s，已 %s）\n", node.BlockReason, node.PendingAt.Format("15:04:05"), humanDur(time.Since(node.PendingAt)))
	}
	b.WriteString("\n" + m.th.Style.Dim.Render("升级证据/绑定详情：见侧栏成本与事件流的 model_* 事件"))
	return b.String()
}

// findNode 在树中找节点（渲染辅助；未找到 → nil）。
func (m *Model) findNode(id string) *gateway.TreeNode {
	var found *gateway.TreeNode
	gateway.Walk(m.st.Tree, func(n *gateway.TreeNode) {
		if n.ID == id {
			found = n
		}
	})
	return found
}

// renderSidebar 渲染侧栏（成本仪表盘 + 建议 + watchdog 告警）。
func (m Model) renderSidebar(width int) string {
	var b strings.Builder
	cv := m.st.CostView
	if cv.TotalTokens == 0 {
		b.WriteString(m.th.Style.Dim.Render("暂无记账（任务启动后逐调用累计）") + "\n")
	} else {
		fmt.Fprintf(&b, "%s %s\n", m.th.Style.Accent.Render(fmt.Sprintf("¥%.4f", cv.TotalCost)), m.th.Style.Dim.Render(cv.Currency))
		fmt.Fprintf(&b, "tokens %s · 调用 %d\n", humanCount(cv.TotalTokens), cv.Calls)
		fmt.Fprintf(&b, "思维链 %.0f%% · 缓存命中 %.0f%%\n\n", cv.ReasoningShare*100, cv.CacheHitRate*100)
		for _, lv := range cv.Levels {
			fmt.Fprintf(&b, "%-4s %3d 调用 ¥%.4f\n", lv.Rung, lv.Calls, lv.Cost)
		}
		for _, s := range cv.Suggestions {
			b.WriteString("\n" + m.th.Style.Blocked.Render("建议") + " " + wrapText(s, width-4) + "\n")
		}
	}
	if alerts := m.watchAlerts(); len(alerts) > 0 {
		b.WriteString("\n" + m.th.Style.Crashed.Render("Watchdog") + "\n")
		for _, a := range alerts {
			b.WriteString(wrapText(a, width-2) + "\n")
		}
	}
	return b.String()
}

// watchAlerts 从事件窗口聚合 watchdog 告警（黄=无进展/停摆，红=终止）。
func (m *Model) watchAlerts() []string {
	var out []string
	for _, ev := range m.st.Events {
		if ev.Kind != gateway.KindWatchdog {
			continue
		}
		switch ev.Action {
		case "watchdog_no_progress":
			out = append(out, m.th.Style.Blocked.Render("⏳ "+ev.Target+" 无进展")+" "+quietOf(ev.Payload))
		case "watchdog_terminated":
			out = append(out, m.th.Style.Crashed.Render("☠ "+ev.Target+" 已强制终止"))
		case "watchdog_pending_overrun":
			out = append(out, m.th.Style.Blocked.Render("⏳ "+ev.Target+" 挂起超时")+" "+quietOf(ev.Payload))
		}
	}
	if len(out) > 5 {
		out = out[len(out)-5:] // 侧栏窗口上限：最新 5 条
	}
	return out
}

// quietOf 提取 quiet_seconds（无 → 空串；payload 形状开放，防御式取值）。
func quietOf(payload any) string {
	if m, ok := payload.(map[string]any); ok {
		if v, ok := m["quiet_seconds"]; ok {
			return fmt.Sprintf("(%v)", v)
		}
	}
	return ""
}

// actionBanner 是 Action Center 横幅（有待办时 1 行高亮；无 → 不渲染）。
func (m Model) actionBanner() string {
	items := m.actionItems()
	if len(items) == 0 {
		return ""
	}
	first := items[0]
	return m.th.Style.ActionBanner.Render(fmt.Sprintf(" ⚡ 待你决策 (%d)：[%s] %s —— 按 a 处理 ", len(items), first.Kind, clip(first.Title, 48)))
}

// inputBar 是输入行（焦点提示 + 目标显示）。
func (m Model) inputBar() string {
	target := m.target
	if target == "" {
		target = m.selectedOr()
	}
	prefix := m.th.Style.Dim.Render("到 " + target + " ▸ ")
	return prefix + m.input.View()
}

// statusLine 是底部状态行（键位速查 + toast）。
func (m Model) statusLine() string {
	if m.toastAlive() {
		style := m.th.Style.ToastOK
		if m.toastErr {
			style = m.th.Style.ToastErr
		}
		return style.Render(" " + clip(m.toast, m.width-4) + " ")
	}
	if m.st != nil && m.st.Run.Active {
		return m.th.Style.Dim.Render(" a 审批 · ↑↓ 选 agent · i 输入 · f 跟随 · q 退出(任务保持)")
	}
	return m.th.Style.Dim.Render(" /start 启动任务 · a 行动中心 · i 输入 · ? 帮助 · q 退出")
}

// modalView 渲染当前浮层（居中卡片；modal 互斥——一次只有一张）。
func (m Model) modalView() string {
	var body string
	switch m.modal {
	case mActions:
		body = m.renderActions()
	case mGate:
		body = m.renderGateCard()
	case mEscal:
		body = m.renderEscCard()
	case mDiscuss:
		body = m.renderDiscCard()
	case mHelp:
		body = m.renderHelp()
	case mQuitConfirm:
		body = m.th.Style.Blocked.Render("任务仍在运行。退出后任务保持后台（marl status 可查看）。") +
			"\n\n确认退出？(y=退出 / n=留下)"
	}
	return m.th.Style.FocusedBorder.Render(centerBlock(body, m.width, m.height/2))
}

// renderActions 渲染行动中心列表（待办序 = gateway.ActionItems 的契约序）。
func (m Model) renderActions() string {
	items := m.actionItems()
	var b strings.Builder
	b.WriteString(m.th.Style.PaneTitle.Render(" 行动中心 —— 等待你的决策 ") + "\n\n")
	for i, it := range items {
		cursor := "  "
		if i == m.actionSel {
			cursor = m.th.Style.Selected.Render("▸ ")
		}
		style := m.th.Style.Dim
		switch it.Kind {
		case gateway.ActionEscalation:
			style = m.th.Style.Crashed
		case gateway.ActionGate:
			style = m.th.Style.Blocked
		}
		fmt.Fprintf(&b, "%s%d. %s %s\n   %s\n", cursor, i+1,
			style.Render(string(it.Kind)), it.Title, m.th.Style.Dim.Render(clip(it.Detail, 60)))
	}
	b.WriteString("\n" + m.th.Style.Dim.Render("↑↓ 选择 · Enter 打开 · Esc 关闭"))
	return b.String()
}

// renderGateCard 渲染审批卡（请求 + 决策键位 + N 输入步）。
func (m Model) renderGateCard() string {
	var b strings.Builder
	g := m.gate
	b.WriteString(m.th.Style.PaneTitle.Render(" 审批请求 · "+g.item.Title+" ") + "\n")
	fmt.Fprintf(&b, "来自 %s\n\n", g.item.From)
	if d := m.inboxDetail(g.item.ID); d != "" {
		b.WriteString(m.th.Style.Dim.Render(clip(d, 500)) + "\n\n")
	}
	switch g.step {
	case 0:
		b.WriteString("决策（单键）：\n")
		b.WriteString(m.th.Style.Running.Render("  [o] 放行一次") + "   ")
		b.WriteString(m.th.Style.Running.Render("[n] 放行接下来 N 次") + "\n")
		b.WriteString(m.th.Style.Running.Render("  [k] 追加 token 额度") + "   ")
		b.WriteString(m.th.Style.Blocked.Render("[a] 永久放行此类") + "   ")
		b.WriteString(m.th.Style.Crashed.Render("[d] 拒绝") + "\n")
		b.WriteString("\n" + m.th.Style.Dim.Render("n/k 进入额度输入；批注在随后一步填写。Esc 稍后处理。"))
	case 1:
		b.WriteString("输入额度（" + g.mode + "）：\n\n")
		b.WriteString(g.num.View())
		b.WriteString("\n\n" + m.th.Style.Dim.Render("Tab 切理由 · Enter 继续 · Esc 取消"))
	case 2:
		b.WriteString("额度 " + g.mode + " = " + fmt.Sprint(g.pendingN) + "\n\n")
		b.WriteString("批注/理由（可空）：\n\n")
		b.WriteString(g.reason.View())
		b.WriteString("\n\n" + m.th.Style.Dim.Render("Enter 提交 · Esc 取消"))
	}
	return b.String()
}

// inboxDetail 读审批文件全文（决策卡展示"人类会看到什么"——文件通道
// 的原始内容，不经二手转述）。
func (m *Model) inboxDetail(id string) string {
	data, err := m.gw.ReadInbox("gate_" + id + ".md")
	if err != nil {
		return ""
	}
	return string(data)
}

// renderEscCard 渲染求助回复卡。
func (m Model) renderEscCard() string {
	var b strings.Builder
	b.WriteString(m.th.Style.PaneTitle.Render(" 求助 · "+m.esc.item.Title+" ") + "\n")
	fmt.Fprintf(&b, "来自 %s\n\n", m.esc.item.From)
	if m.esc.item.Detail != "" {
		b.WriteString(m.th.Style.Dim.Render(m.esc.item.Detail) + "\n\n")
	}
	b.WriteString(m.esc.reply.View())
	b.WriteString("\n" + m.th.Style.Dim.Render("Ctrl+S 提交 · Esc 取消"))
	return b.String()
}

// renderDiscCard 渲染讨论裁决卡。
func (m Model) renderDiscCard() string {
	var b strings.Builder
	b.WriteString(m.th.Style.PaneTitle.Render(" 讨论 · "+m.disc.item.Title+" ") + "\n\n")
	b.WriteString(m.disc.note.View())
	b.WriteString("\n" + m.th.Style.Dim.Render("p 通过（草稿落地） · Ctrl+S 只发批注 · Esc 取消"))
	return b.String()
}

// renderHelp 渲染帮助（keyHelp/commands 表的自动生成——单一来源）。
func (m Model) renderHelp() string {
	var b strings.Builder
	b.WriteString(m.th.Style.PaneTitle.Render(" 键位 ") + "\n\n")
	for _, k := range focusKeys {
		fmt.Fprintf(&b, "  %-12s %s\n", k.key, k.desc)
	}
	b.WriteString("\n" + m.th.Style.PaneTitle.Render(" 命令 ") + "\n\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-10s %-16s %s\n", c.name, c.args, c.desc)
	}
	b.WriteString("\n" + m.th.Style.Dim.Render("Esc/? 关闭"))
	return b.String()
}

// --- 小件 ---

// humanDur 时长人性化（3h08m / 5m20s / 45s——对齐 status.go 口径；
// 零值时刻 → "-"，避免溢出渲染（time.Time 零值距 now 极大））。
func humanDur(d time.Duration) string {
	if d < 0 || d > 24*365*time.Hour {
		return "-"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// humanCount 数字人性化（1234 → 1.2k，12345 → 12.3k）。
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// clip 按显示宽度截断（超长加 …；[推断] 按字节近似——中文场景按 rune
// 截，标点对齐误差可接受）。
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
