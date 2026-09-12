package agent

// 意图工具的 schema（Part 4.1 / ADR-0015：意图与技能在同一张工具表里呈现，
// 全项目唯一、逐字节冻结）。
//
// 这些 schema 与技能 schema 的唯一区别是处理路径：技能走 Registry，
// 意图走各自的裁决关口（executeIntent）。schema 字节一旦进入工具表就不能
// 改动（破全项目缓存前缀），因此写死为常量并由 golden 测试守护。

import (
	"encoding/json"

	"marl/internal/wire"
)

// 意图工具 schema（逐字节冻结；改动 = 全项目缓存前缀失效，必须评估）。
const (
	schemaSpawnSubagent = `{"type":"object","properties":{` +
		`"profile_id":{"type":"string","description":"Profile id of the child agent"},` +
		`"task":{"type":"string","description":"Self-contained task description for the child (it sees nothing else from your context)"},` +
		`"writable_paths":{"type":"array","items":{"type":"string"},"description":"Glob paths the child may write; MUST be a subset of your own writable scope"},` +
		`"readable_paths":{"type":"array","items":{"type":"string"},"description":"Optional extra readable globs (default: inherit yours)"},` +
		`"prompt_override":{"type":"string","description":"Optional prompt id overriding the profile default"},` +
		`"inject_message_seqs":{"type":"array","items":{"type":"integer"},"description":"Optional seq numbers of your log entries to inject into the child context"}},"required":["profile_id","task"]}`

	schemaReportToParent = `{"type":"object","properties":{` +
		`"report":{"type":"string","description":"Free-text summary of what you did, which files changed, test results, and any blockers"},` +
		`"status":{"type":"string","enum":["success","partial","failed"],"description":"Task outcome; the framework mechanically verifies success claims"}},"required":["report","status"]}`

	schemaRequestHuman = `{"type":"object","properties":{` +
		`"question":{"type":"string","description":"What you need from the human"},` +
		`"context":{"type":"string","description":"Background the human needs"}},"required":["question"]}`

	schemaRequestReconfigure = `{"type":"object","properties":{` +
		`"reason":{"type":"string","description":"Why you are stuck; the ladder upgrades based on evidence, not on this request"}},"required":["reason"]}`

	schemaRequestBranch = `{"type":"object","properties":{` +
		`"purpose":{"type":"string","description":"What the branch is for (high-risk experiment)"}},"required":["purpose"]}`

	schemaRequestDiscussion = `{"type":"object","properties":{` +
		`"topic":{"type":"string","description":"Discussion topic for the human"},` +
		`"draft":{"type":"string","description":"Your proposal to discuss"}},"required":["topic","draft"]}`

	schemaSpawnBatch = `{"type":"object","properties":{` +
		`"items":{"type":"array","items":{"type":"object","properties":{` +
		`"profile_id":{"type":"string","description":"Profile id of the child agent"},` +
		`"task":{"type":"string","description":"Self-contained task description for the child"},` +
		`"writable_paths":{"type":"array","items":{"type":"string"},"description":"Glob paths the child may write; MUST be a subset of your own writable scope"},` +
		`"readable_paths":{"type":"array","items":{"type":"string"},"description":"Optional extra readable globs"},` +
		`"prompt_override":{"type":"string","description":"Optional prompt id overriding the profile default"},` +
		`"inject_message_seqs":{"type":"array","items":{"type":"integer"},"description":"Optional seq numbers of your log entries to inject"}}},` +
		`"await":{"type":"string","enum":["all","any","n"],"description":"Resume you after all children report (default), after any one, or after n"},` +
		`"n":{"type":"integer","description":"Resume threshold when await=n (1..len(items))"}},"required":["items"]}`
)

// intentSchemas 返回意图工具的协议无关定义（顺序 = proto.IntentToolNames
// 的冻结序；与技能表拼接后构成全项目唯一的工具表）。
//
// 并发：纯函数（常量拼接）。
func intentSchemas() []wire.ToolDef {
	return []wire.ToolDef{
		{Name: "spawn_subagent", Description: "Fork a child agent for a self-contained subtask. The child gets ONLY the task text you write plus optionally injected messages; it cannot talk to you until it reports back. Use for parallelizable, well-bounded work.", Parameters: json.RawMessage(schemaSpawnSubagent)},
		{Name: "report_to_parent", Description: "Report task completion to your parent agent. Write a clear summary of what you did, which files changed, test results, and any blockers.", Parameters: json.RawMessage(schemaReportToParent)},
		{Name: "request_human", Description: "Ask the human operator for input (escalation channel).", Parameters: json.RawMessage(schemaRequestHuman)},
		{Name: "request_reconfigure", Description: "Signal that you are stuck; the framework decides ladder upgrades from evidence.", Parameters: json.RawMessage(schemaRequestReconfigure)},
		{Name: "request_branch", Description: "Request a version-control branch for a high-risk experiment.", Parameters: json.RawMessage(schemaRequestBranch)},
		{Name: "request_discussion", Description: "Open a human discussion branch and pause until the human approves.", Parameters: json.RawMessage(schemaRequestDiscussion)},
		{Name: "spawn_batch", Description: "Fork several children in one call. Use when subtasks are independent and parallelizable; results arrive as one batch of reports.", Parameters: json.RawMessage(schemaSpawnBatch)},
	}
}
