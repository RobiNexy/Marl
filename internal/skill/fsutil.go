package skill

// 技能共享的低层工具（参数容错 / 二进制探测）。
//
// 来源：沿用 old_skills（上一版本的实战实现）的最小可用部分，适配到
// 阶段 0/2 的契约（SkillEnv + Resolver + SkillResult）。逐项的取舍理由
// 见各函数注释；原子写模块在 atomic_write.go（理由纪念碑式地留在那里）。

import (
	"strconv"
)

// ---------------------------------------------------------------------------
// 参数提取：LLM 提供的 args 是 map[string]any，类型不可信，逐项容错。
// 容错方向统一是"回默认值"而不是报错——参数类型的容错由 schema 声明兜底
// （错误 JSON 已被 Normalizer 拦截），运行时的再校验只收窄越界幅度。
// ---------------------------------------------------------------------------

func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64: // JSON 数字默认反序列化为 float64
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// isBinary 魔数探测：前 512 字节含 NUL 即视为二进制（git 的经典启发式）。
func isBinary(b []byte) bool {
	n := len(b)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// strSliceArg 从 args 提取字符串切片（exclude 之类），类型不可信。
func strSliceArg(args map[string]any, key string) []string {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
