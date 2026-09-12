// 子命令 marl models 的 probe 面（13.12 阶段 10"能力探测 marl models
// probe"；Part 10.13）。与 cmd/probe（阶段 1 的完整探测程序）的关系：
// probe 是全量、-save 缓存、分析报告；这里是**轻探测**（任务运行前的
// 现场检查面——模型可用、工具调用是否吐 tool_call、二次同款请求的
// cached_tokens 是否 >0）。
//
// 用法：
//
//	marl models probe -base-url https://api.deepseek.com/v1 \
//	    -key-env DEEPSEEK_API_KEY -models deepseek-flash[,deepseek-v4-pro]
//
// 判定：每项给出 PASS/FAIL/BLOCKED（厂商侧错误分类 → 不算 FAIL 记 warn）；
// 报告打印 stdout（caps_override 的接管盘面留给 daemon 化后的统一写入）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// probeReport 是一次探测报告（model × check 矩阵）。
type probeReport struct {
	Model string
	Rows  []probeRow
}

type probeRow struct {
	Check  string
	Status string // PASS / FAIL / BLOCKED
	Detail string
}

// runModelsProbe 是 models probe 的入口。
func runModelsProbe(args []string) error {
	fs := flag.NewFlagSet("marl models probe", flag.ContinueOnError)
	baseURL := fs.String("base-url", "https://api.deepseek.com/v1", "接入点根地址（OpenAI 兼容形态）")
	keyEnv := fs.String("key-env", "DEEPSEEK_API_KEY", "存放密钥的环境变量名")
	models := fs.String("models", "deepseek-flash", "逗号分隔的模型名清单")
	timeout := fs.Duration("timeout", 60*time.Second, "单次请求超时")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := os.Getenv(*keyEnv)
	if key == "" {
		return fmt.Errorf("env %s is empty", *keyEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reports := []probeReport{}
	for _, m := range strings.Split(*models, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		client := &http.Client{Timeout: *timeout}
		reports = append(reports, probeModel(ctx, client, *baseURL, key, m, *timeout))
	}
	printProbeReports(reports)
	failed := false
	for _, r := range reports {
		for _, row := range r.Rows {
			if row.Status == "FAIL" {
				failed = true
			}
		}
	}
	if failed {
		return fmt.Errorf("probe failed (see report; BLOCKED 不计失败)")
	}
	return nil
}

// probeModel 一轮探测：chat / tool_call / cache 三个判据。
func probeModel(ctx context.Context, client *http.Client, baseURL, key, model string, timeout time.Duration) probeReport {
	rep := probeReport{Model: model}
	row := probeChatRow(ctx, client, baseURL, key, model)
	rep.Rows = append(rep.Rows, row)
	if row.Status == "PASS" {
		rep.Rows = append(rep.Rows, probeToolCallRow(ctx, client, baseURL, key, model))
		rep.Rows = append(rep.Rows, probeCacheRow(ctx, client, baseURL, key, model))
	}
	return rep
}

func ioReadAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

func chatBodySimple(model string) map[string]any {
	return map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "只回答两个字母：OK"},
		},
		"max_tokens": 16,
	}
}

func probeChatRow(ctx context.Context, client *http.Client, baseURL, key, model string) probeRow {
	out, status, err := probeCall(ctx, client, baseURL, key, model, chatBodySimple(model))
	if err != nil || status != 200 {
		return probeRow{Check: "chat", Status: "BLOCKED", Detail: fmt.Sprintf("status=%d err=%v", status, err)}
	}
	if n := len(choicesOf(out)); n == 0 {
		return probeRow{Check: "chat", Status: "FAIL", Detail: fmt.Sprintf("status=%d no choices: %v", status, briefOf(out))}
	}
	return probeRow{Check: "chat", Status: "PASS", Detail: fmt.Sprintf("status=%d", status)}
}

// probeToolCallRow：带一个最简工具的请求 → 模型是否建议 tool_calls（
// 工具调用是"有无"两种，见到 tool_calls 判 PASS；没见到 tool_calls 且
// 文本里表达了"想调用"也按 FAIL 记——因为它不是结构化调用）。
func probeToolCallRow(ctx context.Context, client *http.Client, baseURL, key, model string) probeRow {
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "请调用 list_files 工具（内容随意）"},
		},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "list_files",
				"description": "读取目录清单（探测用）",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
		"max_tokens": 64,
	}
	out, status, err := probeCall(ctx, client, baseURL, key, model, body)
	if err != nil {
		return probeRow{Check: "tool_call", Status: "BLOCKED", Detail: fmt.Sprintf("status=%d err=%v", status, err)}
	}
	choice := firstChoiceOf(out)
	mmsg, _ := choice["message"].(map[string]any)
	if calls, _ := mmsg["tool_calls"].([]any); len(calls) > 0 {
		return probeRow{Check: "tool_call", Status: "PASS", Detail: fmt.Sprintf("tool_calls=%d", len(calls))}
	}
	return probeRow{Check: "tool_call", Status: "FAIL",
		Detail: fmt.Sprintf("message=%v（模型没有建议 tool_call）", briefOf(mmsg))}
}

// probeCacheRow：两同款请求 → 第二次 usage.prompt_tokens_details.cached_tokens>0
// （DeepSeek 的隐式前缀缓存；无缓存协议按 detail 里见到的字段判）。
func probeCacheRow(ctx context.Context, client *http.Client, baseURL, key, model string) probeRow {
	fill := strings.Repeat("缓存判据填充文本。", 200)
	body := chatBodyLarge(model, fill)
	_, status, err := probeCall(ctx, client, baseURL, key, model, body)
	if err != nil {
		return probeRow{Check: "cache", Status: "BLOCKED", Detail: fmt.Sprintf("first status=%d err=%v", status, err)}
	}
	out, status, err := probeCall(ctx, client, baseURL, key, model, body)
	if err != nil {
		return probeRow{Check: "cache", Status: "BLOCKED", Detail: fmt.Sprintf("second status=%d err=%v", status, err)}
	}
	// probeCall 的结果是响应 envelope——usage 在其 "usage" 键下。
	cached := cachedTokensOf(usageOf(out))
	if cached > 0 {
		return probeRow{Check: "cache", Status: "PASS", Detail: fmt.Sprintf("cached_tokens=%d", cached)}
	}
	return probeRow{Check: "cache", Status: "FAIL", Detail: fmt.Sprintf("cached_tokens=0 usage=%v", briefOf(usageOf(out)))}
}

// chatBodyLarge：填充一段重复文本让第二次请求命中前缀缓存（探测的
// 上下文预留 > 厂商单次计价的最小粒度——真实 PodsDummy 见 cmd/probe）。
func chatBodyLarge(model, fill string) map[string]any {
	return map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": fill + "\\n只回答 OK"},
		},
		"max_tokens": 16,
	}
}

// printProbeReports 的输出面（stdout 的报表形态：每 model 一段矩阵）。
func printProbeReports(reports []probeReport) {
	for _, r := range reports {
		fmt.Printf("model %s:\n", r.Model)
		for _, row := range r.Rows {
			fmt.Printf("  %-12s %-8s %s\n", row.Check, row.Status, row.Detail)
		}
	}
}

// choicesOf / firstChoice / briefOf 是通用 json 的取值辅助（探测的机械
// 面；错误面是"格式漂移"——厂商响应契约寻常见）。
func probeCall(ctx context.Context, client *http.Client, baseURL, key, model string, body map[string]any) (map[string]any, int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(baseURL, "/")+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Add("authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := ioReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return map[string]any{"raw": string(raw)}, resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

func choicesOf(out map[string]any) []any {
	c, _ := out["choices"].([]any)
	return c
}

func firstChoiceOf(out map[string]any) map[string]any {
	if cs := choicesOf(out); len(cs) > 0 {
		m, _ := cs[0].(map[string]any)
		return m
	}
	return map[string]any{}
}

// briefOf 是 json 的最小可读简写（probe 报告的 detail 支持）。
func briefOf(detail any) string {
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Sprintf("%v", detail)
	}
	s := string(b)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func usageOf(out map[string]any) map[string]any {
	u, _ := out["usage"].(map[string]any)
	return u
}

func cachedTokensOf(usage map[string]any) int {
	// 三个字段的容差面：prompt_cache_hit_tokens（DeepSeek）→
	// prompt_tokens_details.cached_tokens（通用 OpenAI 语境）。
	if n, ok := usage["prompt_cache_hit_tokens"].(float64); ok {
		return int(n)
	}
	det, _ := usage["prompt_tokens_details"].(map[string]any)
	if det == nil {
		return 0
	}
	n, _ := det["cached_tokens"].(float64)
	return int(n)
}

// cmdModels 处理 models 子命令（probe 是当前唯一面；报规后面留
// "models list -db"（catalog 表）的占位注释见 status.go 的同一形态）。
func cmdModels(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: marl models probe [flags]")
	}
	switch args[0] {
	case "probe":
		return runModelsProbe(args[1:])
	default:
		return fmt.Errorf("unknown models subcommand %q (want: probe)", args[0])
	}
}
