package tui

// 渲染工具（lipgloss 的薄包装——统一出口，避免各面板各自发明换行/填充
// 逻辑导致视觉漂移）。

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// wrapText 按显示宽度硬换行（lipgloss 的 Width 渲染即 ANSI 感知的折行）。
func wrapText(s string, width int) string {
	if width < 10 {
		width = 10
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}

// padLines 把内容填充到固定高度（最后一行后补空行——面板等高对齐）。
func padLines(content string, width, height int) string {
	if height < 1 {
		height = 1
	}
	pad := lipgloss.NewStyle().Width(width).Render
	lines := strings.Split(content, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, ln := range lines {
		lines[i] = pad(ln)
	}
	return strings.Join(lines, "\n")
}

// centerBlock 居中放置（浮层卡片；宽度收窄到 3/5——视觉聚焦）。
func centerBlock(body string, width, height int) string {
	w := width * 3 / 5
	if w < 40 {
		w = width - 4
	}
	if w < 20 {
		w = 20
	}
	if height < 5 {
		height = 5
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center,
		wrapText(body, w))
}

// lipglossJoinHorizontal 是 lipgloss.JoinHorizontal 的本包别名（渲染
// 组合的统一出口——将来加间距/分割线只改这里）。
func lipglossJoinHorizontal(blocks ...string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, blocks...)
}
