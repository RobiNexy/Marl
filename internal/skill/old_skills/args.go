//go:build ignore

package skill

import (
	"reflect"
	"strconv"
)

// ── 参数提取辅助：LLM 提供的 args 是 map[string]any，类型不可信，逐项容错 ──

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

func boolArg(args map[string]any, key string, def bool) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func strSliceArg(args map[string]any, key string) []string {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil
	}
	rv := reflect.ValueOf(raw)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		out := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			ev := rv.Index(i)
			switch ev.Kind() {
			case reflect.String:
				out = append(out, ev.String())
			case reflect.Interface:
				if s, ok := ev.Interface().(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
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
