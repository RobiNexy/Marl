//go:build ignore

package skill

import (
	"context"
	"fmt"
)

// Skill 原子技能契约。
//
// 契约（Part 4.3 通用准则）：
//   - 原子性：Execute 内部要么完成要么失败，不留半写状态（文件类用 temp+rename 保证）
//   - JSON 可序列化：参数与返回均为 map[string]any，返回必含 "ok"（批处理 DSL 兼容）
//   - 失败以 *SkillError 表达；其他 error 视为内部错误，由 Registry 包装
type Skill interface {
	Name() string
	Description() string
	Capability() Capability
	// ctx 用于取消传播（如 shell_exec 超时击杀进程组）。
	Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error)
}

// SkillError 结构化技能错误。Type 对应设计文档的 error_type 枚举；
// Hint 是给 LLM 的纠错建议——Token 经济性的关键：看一眼就能纠正，不必重读文件。
type SkillError struct {
	Type  string
	Msg   string
	Hint  string
	Extra map[string]any // 额外结构化字段（如 MULTIPLE_MATCHES 的 locations）
}

func (e *SkillError) Error() string { return e.Type + ": " + e.Msg }

func errf(typ, format string, a ...any) *SkillError {
	return &SkillError{Type: typ, Msg: fmt.Sprintf(format, a...)}
}

// Registry 技能注册表。M1 固定注册全部技能；Profile 的 include/exclude 过滤
// 发生在编排层（决定哪些技能暴露给 LLM），不在注册表层——两层职责分离。
type Registry struct {
	skills map[string]Skill
}

func NewRegistry() *Registry {
	r := &Registry{skills: make(map[string]Skill)}
	for _, s := range []Skill{
		&listDirSkill{},
		&fileReadSkill{},
		&fileSearchSkill{},
		&fileWriteSkill{},
		&fileEditSkill{},
		&restoreSnapshotSkill{},
		&shellExecSkill{},
		&shellSpawnSkill{}, // M4：异步事件循环（后台进程 + 观察者推送）
		&shellKillSkill{},
		&getEnvSkill{},
		&listPromptsSkill{}, // Patch 2/3：环境类第 9 个技能
		&webFetchSkill{},    // Patch 7 路线图：M1 可选提前项——至此 14 技能中已注册 10 个
		&arxivSearchSkill{}, // Patch 1/3：文献类（arXiv 专项）
		&arxivFetchSkill{},
	} {
		r.skills[s.Name()] = s
	}
	return r
}

func (r *Registry) Get(name string) (Skill, bool) {
	s, ok := r.skills[name]
	return s, ok
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.skills))
	for n := range r.skills {
		out = append(out, n)
	}
	return out
}

// Result 成功返回的便捷构造：kv 成对传入。
func Result(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2+1)
	m["ok"] = true
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

// Execute 统一入口。任何 panic 转为结构化失败而非击穿 actor goroutine；
// 返回值恒为含 "ok" 的可序列化 map，供 LLM 与批处理 DSL 直接消费。
// 注意：mutating 所有权校验在各技能内部调用（需要先 Resolve 才有规范路径），
// Registry 层不做重复检查。
func (r *Registry) Execute(ctx context.Context, name string, ec *ExecContext, args map[string]any) (result map[string]any) {
	s, ok := r.skills[name]
	if !ok {
		return map[string]any{"ok": false, "error_type": "UNKNOWN_SKILL",
			"message": fmt.Sprintf("unknown skill %q; available: %v", name, r.Names())}
	}
	defer func() {
		if rec := recover(); rec != nil {
			result = map[string]any{"ok": false, "error_type": "INTERNAL_PANIC",
				"message": fmt.Sprintf("skill %s panicked: %v", name, rec)}
		}
	}()
	res, err := s.Execute(ctx, ec, args)
	if err != nil {
		out := map[string]any{"ok": false}
		if se, isSE := err.(*SkillError); isSE {
			out["error_type"] = se.Type
			out["message"] = se.Msg
			if se.Hint != "" {
				out["hint"] = se.Hint
			}
			for k, v := range se.Extra {
				out[k] = v
			}
		} else {
			out["error_type"] = "INTERNAL_ERROR"
			out["message"] = err.Error()
		}
		return out
	}
	if res == nil {
		res = map[string]any{}
	}
	res["ok"] = true
	return res
}
