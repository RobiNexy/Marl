//go:build ignore

package skill

import "context"

// listPromptsSkill 列出可用认知提示词索引（Patch 2/3，M1 第 9 个技能）。
// 设计意图（补丁 3）：父 Agent 在 Planning 阶段用它选择给子 Agent 塞哪个
// prompt。无参数，一次返回完整索引——prompt 本体不经过本技能，
// 需要全文时 LLM 直接 file_read 对应 md（提示词内容由人类维护）。
type listPromptsSkill struct{}

func (s *listPromptsSkill) Name() string { return "list_prompts" }
func (s *listPromptsSkill) Description() string {
	return "List all available cognitive prompts from prompts/_index.yaml."
}
func (s *listPromptsSkill) Capability() Capability { return CapReadOnly }

func (s *listPromptsSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	if ec == nil || ec.Prompts == nil {
		return nil, &SkillError{
			Type: "PROMPTS_UNAVAILABLE",
			Msg:  "no prompt index wired into this agent's exec context",
			Hint: "Prompt listing is configured by the framework at startup.",
		}
	}
	prompts := ec.Prompts()
	if prompts == nil {
		prompts = []PromptSummary{}
	}
	return Result("prompts", prompts), nil
}
