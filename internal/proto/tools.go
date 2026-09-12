package proto

import "marl/internal/types"

// 意图工具名常量（Part 4.1 的"意图 vs 技能"边界）。
//
// 这些不是技能：技能只包含对外部世界（文件系统、命令行、网络）的读写；
// 任何改变 Agent 自身或系统结构的能力都是"意图"，走各自的裁决关口。
// 但它们在 LLM 的工具表里与技能并列呈现（schema 全项目唯一、冻结），
// 调用时由框架按各裁决关口的规则处理。
//
// 名字是**协议的一部分**（模型按名字调用，历史 Log 里存的是名字），
// 因此不得改值；改名等于让历史工具调用无法归因。
const (
	ToolSpawnSubagent      = "spawn_subagent"      // → Spawner 裁决
	ToolRequestHuman       = "request_human"       // → Escalation 走上报链
	ToolRequestReconfigure = "request_reconfigure" // → 发 Mailbox 给自己，经能力校验
	ToolRequestBranch      = "request_branch"      // → Spawner 裁决
	ToolRequestDiscussion  = "request_discussion"  // → 开 Fossil 分支并进入讨论状态
	ToolSpawnBatch         = "spawn_batch"         // → Spawner 批量裁决（阶段 9）
	ToolLLMCall            = "llm_call"            // → 一次受控副调用（阶段 11 逃生舱）
	ToolReportToParent     = "report_to_parent"    // → 报告完成，走机械检查
)

// intentToolList 是意图工具名的唯一来源（数组 + 非导出，理由同 skill 包
// 的 skillNameList：工具表的顺序是冻结的，共享可变切片会被静默改序）。
//
// 与技能表的关系：意图与技能在**同一张**工具表里呈现（Part 4.1），
// 因此这张列表的顺序也会影响最终表序；两处都必须由 golden 测试守护
// （见 ADR-0015）。
var intentToolList = [...]string{
	ToolSpawnSubagent, ToolRequestHuman, ToolRequestReconfigure,
	ToolRequestBranch, ToolRequestDiscussion, ToolReportToParent,
	ToolSpawnBatch, ToolLLMCall,
}

// IntentToolCount 是意图工具数量。
const IntentToolCount = 8

// 编译期断言：列表与常量数量必须一致（阶段 9 加入 spawn_batch）。
var _ [IntentToolCount - len(intentToolList)]struct{}
var _ [len(intentToolList) - IntentToolCount]struct{}

// IntentToolNames 返回意图工具名的**副本**（调用方可排序/拼接而不影响全局表序）。
//
// 并发：安全（只读全局数组 + 本地分配）。
func IntentToolNames() []string {
	out := make([]string, len(intentToolList))
	copy(out, intentToolList[:])
	return out
}

// IsIntentTool 报告 name 是否为已知意图工具名。
//
// 用途：主循环分发工具调用时先做这一步，避免把未知名字当成技能去查表
// （那会走到"技能不存在"的错误路径，掩盖真正的协议不一致）。
//
// 并发：纯函数。
func IsIntentTool(name string) bool {
	for _, n := range intentToolList {
		if n == name {
			return true
		}
	}
	return false
}

// IntentCall 是主循环对意图调用的统一处理入口（阶段 2 起实现）。
// 与技能不同，意图调用不返回 ToolResult 形式的 JSON，直接驱动框架行为。
//
// 不变量：
//   - Tool 必须是上面的常量之一（用 IsIntentTool 判定）。**不做前缀猜测、
//     不做模糊匹配**：意图会改系统结构，一次拼写错误必须变成"未知工具"，
//     不能被容错成某个意图；
//   - FromAgent 非空，且由**框架按代码路径填写**，不取自模型输出（原则 4：
//     模型不能自称是谁）；
//   - CallID 非空：它是与 LLM 那次 tool_call 的对应关系，用于把裁决结果
//     （批准/拒绝/错误码）回填到正确的调用上。CallID 丢失会导致结果错配，
//     而"错配"表现为模型收到一个看似合理但属于别的调用的结论。
type IntentCall struct {
	Tool      string
	CallID    string
	FromAgent types.AgentID
}
