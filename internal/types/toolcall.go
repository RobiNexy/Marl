package types

import "encoding/json"

// ToolCall 是 LLM 产出的一次工具调用（Part 10.3 / 10.16）。
// 同名类型在 wire 的 Segment.ToolCalls / Outcome.ToolCalls 里复用。
//
// 不变量：
//   - Name 非空——没有名字的调用无法路由到任何技能，必须作为协议错误上报，
//     而不是"跳过这条"（跳过会让模型以为自己调用了工具并收到了结果）；
//   - Arguments 必须是合法 JSON。框架**只校验合法性、不解释语义**：坏 JSON
//     是协议失败的信号，属 10.10 的降级/重试范畴，必须可观测。
//
// ID 可以为空：部分厂商不带 tool_call_id（流式增量片段、一次性多工具的混合
// 形态等），此时由 Normalizer 合成稳定 ID（见 10.16）。所以"ID 为空"不是错误，
// 本层不设校验——合成是 Normalizer 的责任，因为只有它能看到同一 turn 的全貌。
type ToolCall struct {
	ID        string          // 协议层 tool_call_id；空 = 由 Normalizer 合成
	Name      string          // 技能/意图名，如 "file_read" / "spawn_subagent"
	Arguments json.RawMessage // 参数 JSON 原文，框架不解析内容
}

// ToolResult 是一次工具调用的机械结果（Part 4.3 通用准则 6：含 ok 字段）。
// 主循环把它序列化成 tool 消息回填给模型。错误用 ErrorType 结构化表达
// （如 NO_MATCH / PATH_READONLY / ENOENT / SKILL_NOT_ALLOWED），
// 不混进正文，方便上层做机械判断。
//
// 不变量（互斥，由构造点与 Validate 共同保证）：
//
//	OK == true   ⟹  ErrorType == ""
//	OK == false  ⟹  ErrorType != ""
//
// 这是本类型最要紧的契约。一个 OK=false 却没有 ErrorType 的结果，上层只能把
// 它呈现为"工具失败了，但不知道原因"，于是退化成让模型去猜——而模型的猜测
// 会被后续轮次当作事实（幻觉的温床）。反过来，OK=true 却带着 ErrorType
// 会让失败检测出现假阳性，触发不必要的重试与升级。
//
// ErrorType 保持 string 而非枚举：技能集可扩展（用户可加技能），错误码集合
// 是开放的。约定为大写下划线形式，便于机械匹配与 grep。
type ToolResult struct {
	ToolCallID string
	OK         bool
	ErrorType  string
	Content    string // JSON 序列化的结果体（技能返回 map 后序列化而来）
}

// Validate 报告该结果是否自洽（见上方互斥不变量）。
//
// 失败：违反 OK 与 ErrorType 的互斥规则。
// 并发：纯函数。
func (r ToolResult) Validate() error {
	panic("TODO(phase 0): placeholder")
}
