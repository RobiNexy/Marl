package skill

// 机械编排五件套的技能壳（阶段 11 补遗 §6 的总表：exclude / restore /
// reorder / annotate / pin——零 LLM 调用，全部走框架执行面 Orch）。
//
// 定位协议（补遗 §2）：target_text 主定位（精确子串 → 归一化兜底 → 不做
// 语义匹配）；relative 可组合过滤；匹配范围只在**可见条目**（frozen 段
// 不在 View，硬拒面在执行点）。pin 是零破坏（连破坏分级都免）。
// 语义拆分/压缩/摘要是 llm_call + 显式 View 操作（补遗 §4），不是本包技能。
//
// schema 冻结纪律：Parameters 字节一旦进工具表就不能改（缓存前缀）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"marl/internal/orchestrate"
)

// opSkill 是编排技能的公共骨架（参数定位协议的单一翻译点）。
type opSkill struct {
	name        string
	description string
}

// ExportedSingle 是共享实例（与 FileRead 等同一形态：无状态可并发）。
var (
	OrchExclude  Skill = shellOf(SkillExcludeMessage, "软删除一条消息（Visible=false）。定位：target_text（子串匹配，可加 relative 过滤）。")
	OrchRestore  Skill = shellOf(SkillRestoreMessage, "恢复一条被 exclude 的消息（Visible=true）。")
	OrchReorder  Skill = shellOf(SkillReorderMessage, "把一条消息移动到指定位置（Fractional Index 语义由框架换算）。")
	OrchAnnotate Skill = shellOf(SkillAnnotateMessage, "给一条消息附加批注（ ProvAnnotatedOf 血缘追加）。")
	OrchPin      Skill = shellOf(SkillPinMessage, "设置裁剪豁免（零破坏，不影响缓存前缀）。")
)

func shellOf(name, description string) Skill {
	return &opSkill{name: name, description: description}
}

func (os *opSkill) Name() string        { return os.name }
func (os *opSkill) Description() string { return os.description }

// Kind：全部 mutating（View 改动类；is 保守声明——执行面是 eventLoop 单
// goroutine 的串行面）。
func (os *opSkill) Kind() SkillKind { return SkillMutating }

// execute 转发给框架执行面（Orch nil → 如实失败）。
func (os *opSkill) Execute(ctx context.Context, raw map[string]any, env *SkillEnv) (*SkillResult, error) {
	if env == nil || env.Orch == nil {
		return NewFailure("ORCH_UNAVAILABLE", "%s 没有可用的编排执行面（框架装配缺失）", os.name), nil
	}
	sel, extra, verr := decodeOpArgs(raw)
	if verr != "" {
		return NewFailure("INVALID_ARGS", "%s", verr), nil
	}
	out := env.Orch.Execute(ctx, orchestrate.OpKind(os.name), sel, extra)
	ok, _ := out["ok"].(bool)
	if !ok {
		return &SkillResult{OK: false, ErrorType: stringOf(out["error"]),
			Message: stringOf(out["message"]), Data: out}, nil
	}
	res := &SkillResult{OK: true, Data: out}
	return res, nil
}

// decodeOpArgs 是参数解码（target_text/relative/note/position 的字段面；
// pin 的 zero-destruction 由执行面判——这里只看通用契约）。
func decodeOpArgs(raw map[string]any) (orchestrate.TargetSelector, map[string]any, string) {
	sel := orchestrate.TargetSelector{}
	if v, ok := raw["target_text"].(string); ok {
		sel.TargetText = v
	}
	if v, ok := raw["relative"].(string); ok {
		sel.Relative = v
	}
	if sel.TargetText == "" && sel.Relative == "" {
		return sel, nil, "target_text / relative 至少要给一个（定位凭什么寔谁）"
	}
	extra := map[string]any{}
	if v, ok := raw["note"].(string); ok {
		extra["note"] = v
	}
	if v, ok := raw["position"].(float64); ok {
		extra["position"] = v
	}
	return sel, extra, ""
}

// stringOf / resultFromMap 是 json map 的显示面（无行为的轻转换）。
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// Parameters 的 schema（冻结面：五个技能的字段集保持一致——工具表统一
// 形态，缓存前缀字节仅在 Named face 才分差）。
func opSchema(includeNote bool) string {
	base := `{"type":"object","properties":{` +
		`"target_text":{"type":"string","description":"Text to locate the entry (substring match; normalized whitespace fallback)"},` +
		`"relative":{"type":"string","description":"first|last|last_assistant|last_user|second_last_assistant|nth_from_top:N"}`
	if includeNote {
		base += `,"note":{"type":"string","description":"annotation note"}`
	}
	base += `}}`
	return base
}

// 编译期保证：opSkill 是 Skill。
var _ Skill = (*opSkill)(nil)

// 独立导出的 schema 调用点（MemRegistry.Register 会查询 Parameters）。
func (os *opSkill) Parameters() json.RawMessage {
	return json.RawMessage(opSchema(os.name == SkillAnnotateMessage))
}

var _ = fmt.Sprintf
var _ = strings.TrimSpace
