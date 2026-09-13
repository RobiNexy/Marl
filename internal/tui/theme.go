package tui

// 主题层：语义色、图标、降级。所有"颜色+符号"成对出现（色盲友好：
// 状态永远有文字冗余）；NO_COLOR / 非 truecolor 终端自动退 16 色 + ASCII
// 图标（对齐 cmd/marl/status 的 -color auto|never 语义）。

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

// theme 是渲染的语义词汇表（结构体而非全局变量：测试可构造独立实例，
// 全局样式状态是测试不定性的常见来源 [权衡: 注入 vs 包级单例——注入
// 让每个测试可断言降级形态，代价只是函数多一个参数]）。
type theme struct {
	Style     Styles
	ColorProf string // 色彩档案名（诊断显示）
	Icons     Icons
}

// Styles 是全部语义样式（窄集合：每种视觉语义一个，不复用/不组合）。
type Styles struct {
	Root          lipgloss.Style // 人类（树根）
	AgentRoot     lipgloss.Style // 一级 agent
	AgentChild    lipgloss.Style // 子 agent
	Running       lipgloss.Style // 绿：健康
	Blocked       lipgloss.Style // 黄：注意
	Crashed       lipgloss.Style // 红：危险
	FocusedBorder lipgloss.Style // 焦点面板边框
	PaneBorder    lipgloss.Style // 非焦点面板边框
	PaneTitle     lipgloss.Style // 面板标题
	Selected      lipgloss.Style // 列表选中行
	Dim           lipgloss.Style // 次要信息
	Accent        lipgloss.Style // 强调（成本/游标）
	ActionBanner  lipgloss.Style // Action Center 横幅（最高优先级）
	ToastErr      lipgloss.Style
	ToastOK       lipgloss.Style
	Thinking      lipgloss.Style // 思维链折叠行
}

// Icons 是图标集（Nerd/emoji 档 + ASCII 降级档）。
type Icons struct {
	Human, Agent, Child       string
	Run, Block, Crash, Idle   string
	Gate, Spawn, Watch, Model string
	Disc, Escal, Commit, Orph string
}

// newTheme 按环境构造主题（自动降级）。
func newTheme() theme {
	prof := "truecolor"
	if os.Getenv("NO_COLOR") != "" {
		prof = "nocolor"
	}
	if os.Getenv("COLORTERM") == "" && os.Getenv("TERM_PROGRAM") == "" &&
		!lipgloss.HasDarkBackground() {
		// 无 256 色/truecolor 声明的终端：同样退 ASCII（显示完整比好看重要）。
		prof = "nocolor"
	}
	if prof == "nocolor" {
		return nocolorTheme()
	}
	return colorTheme()
}

// colorTheme 是全彩主题（emoji 图标 + 自适应明暗的语义色）。
func colorTheme() theme {
	c := func(color string) lipgloss.Color { return lipgloss.Color(color) }
	return theme{
		ColorProf: "truecolor",
		Icons: Icons{
			Human: "👤", Agent: "🤖", Child: "🔧",
			Run: "●", Block: "◐", Crash: "✕", Idle: "○",
			Gate: "🔐", Spawn: "🌱", Watch: "🐕", Model: "⬆",
			Disc: "💬", Escal: "🆘", Commit: "📌", Orph: "❓",
		},
		Style: Styles{
			Root:          lipgloss.NewStyle().Foreground(c("205")).Bold(true),
			AgentRoot:     lipgloss.NewStyle().Foreground(c("39")).Bold(true),
			AgentChild:    lipgloss.NewStyle().Foreground(c("75")),
			Running:       lipgloss.NewStyle().Foreground(c("40")),
			Blocked:       lipgloss.NewStyle().Foreground(c("214")),
			Crashed:       lipgloss.NewStyle().Foreground(c("196")).Bold(true),
			FocusedBorder: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c("51")),
			PaneBorder:    lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c("240")),
			PaneTitle:     lipgloss.NewStyle().Foreground(c("250")).Background(c("236")).Bold(true),
			Selected:      lipgloss.NewStyle().Bold(true).Reverse(false).Foreground(c("51")),
			Dim:           lipgloss.NewStyle().Foreground(c("244")),
			Accent:        lipgloss.NewStyle().Foreground(c("220")).Bold(true),
			ActionBanner:  lipgloss.NewStyle().Foreground(c("15")).Background(c("130")).Bold(true).Padding(0, 1),
			ToastErr:      lipgloss.NewStyle().Foreground(c("15")).Background(c("124")).Bold(true).Padding(0, 1),
			ToastOK:       lipgloss.NewStyle().Foreground(c("15")).Background(c("28")).Padding(0, 1),
			Thinking:      lipgloss.NewStyle().Foreground(c("243")),
		},
	}
}

// nocolorTheme 是降级主题（16 色/无色 + ASCII 图标；状态用文字冗余承载）。
func nocolorTheme() theme {
	s := func(fg string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(fg)) }
	return theme{
		ColorProf: "nocolor",
		Icons: Icons{
			Human: "[H]", Agent: "[A]", Child: "[-]",
			Run: ">", Block: "~", Crash: "x", Idle: ".",
			Gate: "G", Spawn: "+", Watch: "W", Model: "^",
			Disc: "D", Escal: "!", Commit: "#", Orph: "?",
		},
		Style: Styles{
			Root:          s("15").Bold(true),
			AgentRoot:     s("15").Bold(true),
			AgentChild:    s("7"),
			Running:       s("2"),
			Blocked:       s("3"),
			Crashed:       s("1").Bold(true),
			FocusedBorder: lipgloss.NewStyle().Border(lipgloss.NormalBorder()),
			PaneBorder:    lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Foreground(lipgloss.Color("8")),
			PaneTitle:     s("7").Bold(true),
			Selected:      s("15").Bold(true),
			Dim:           s("8"),
			Accent:        s("3").Bold(true),
			ActionBanner:  lipgloss.NewStyle().Bold(true).Reverse(true).Padding(0, 1),
			ToastErr:      lipgloss.NewStyle().Bold(true).Reverse(true).Padding(0, 1),
			ToastOK:       lipgloss.NewStyle().Reverse(true).Padding(0, 1),
			Thinking:      s("8"),
		},
	}
}

// stateGlyph 返回状态标记（图标 + 语义样式；文字冗余在行内另行给出）。
func (t theme) stateGlyph(state, pending string) (string, lipgloss.Style) {
	switch {
	case state == "crashed":
		return t.Icons.Crash, t.Style.Crashed
	case state == "blocked", pending != "":
		return t.Icons.Block, t.Style.Blocked
	case state == "running":
		return t.Icons.Run, t.Style.Running
	default:
		return t.Icons.Idle, t.Style.Dim
	}
}

// kindGlyph 返回角色图标（human/一级 agent/子 agent；未知 kind 按 child）。
func (t theme) kindGlyph(kind string, depth int) (string, lipgloss.Style) {
	switch {
	case kind == "human":
		return t.Icons.Human, t.Style.Root
	case depth <= 1:
		return t.Icons.Agent, t.Style.AgentRoot
	default:
		return t.Icons.Child, t.Style.AgentChild
	}
}
