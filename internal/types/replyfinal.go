package types

// 控制面文件通道的协议级常量（收件箱/裁决/求助的"结构化回复"标记）。

import "strings"

// ReplyFinalMarker 是结构化回复的"写完"声明（HTML 注释形态，人类阅读
// 不可见）。它解决的是**编辑窗悖论**：文件通道的静默窗（gate 10s）保护
// "人类就地编辑的半截状态"，但结构化前端（TUI/GUI/API）的回复在提交
// 那一刻就是最终态——为机器写入付 10s 编辑窗是纯死等。
//
// 协议：带本标记的内容 = 写入者声明"内容完整且最终"，读侧可立即消费
// （跳过静默窗）；无标记 = 人类手工编辑路径，静默窗照旧。
//
// 安全性：标记不是凭据（凭据是 nonce，原则 4 的对账不变）。它能被任何
// 能写控制面文件的人加上——而那个能力本身就等于"人类之笔"（写不进
// 控制面的进程才是 agent；写进来了就是人类侧，有权选择零延迟）。
//
// 撕裂自愈：若写入横跨崩溃/截断，标记不完整 → 读侧视为无标记 → 落回
// 静默窗慢路径——不会把半截内容当完整裁决。
const ReplyFinalMarker = "<!-- marl:reply-final -->"

// HasReplyFinal 报告内容是否带"写完"声明（读侧的快路径判据）。
// 并发：纯函数。
func HasReplyFinal(content string) bool {
	return len(content) >= len(ReplyFinalMarker) &&
		strings.Contains(content, ReplyFinalMarker)
}

// WithReplyFinal 为结构化写入附带"写完"声明（写侧的统一出口——标记
// 的字节形态只在此处拼装，读侧/写侧永不各自手写）。
func WithReplyFinal(content string) string {
	if HasReplyFinal(content) {
		return content
	}
	return content + "\n" + ReplyFinalMarker + "\n"
}
