// 命令 mini 是设计文档 13.4（阶段 2）的交付检查命令。
//
// 它构造一个能"接收任务、调 LLM、解析 tool_call、调技能、再调 LLM"的最小
// 闭环：手写初始 View（一条 user 消息："列出当前目录，读 README.md"），
// 跑 agent.Run，最后把 Log 打印出来。交付判据：
//
//   - Log 里能看到完整的 tool_call → tool_result → assistant 链条；
//   - SQLite 文件里能 SELECT * FROM log_entries（见 -db 的落盘处）。
//
// 模型与接入点硬编码（阶段 2 没有配置层，与 13.3 probe 的做法一致）；
// DEEPSEEK_API_KEY 提供密钥（-dry-run 不需要，走脚本化回放）。
//
// 用法：
//
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/mini   # 真跑（一次循环，几分钱）
//	go run ./cmd/mini -dry-run                  # 脚本化回放，验证闭环骨架
//
// 任务固定为"列出当前目录，读 README.md"：mini 是交付检查工具，不是通用
// REPL（自由输入属于阶段 8 的人机协作最小闭环）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"marl/internal/agent"
	"marl/internal/ns"
	"marl/internal/skill"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

const defaultTask = "列出当前目录，读 README.md"

// options 是命令行选项（阶段 2 的"配置层"就是它，与 probe 同一取舍）。
type options struct {
	apiKeyEnv   string
	baseURL     string
	modelID     string
	remote      string
	bucketField string
	endpoint    string
	dbPath      string
	dryRun      bool
	maxRounds   int
}

func parseOptions(args []string) options {
	var o options
	fs := flag.NewFlagSet("mini", flag.ContinueOnError)
	fs.StringVar(&o.apiKeyEnv, "api-key-env", "DEEPSEEK_API_KEY", "存放密钥的环境变量名")
	fs.StringVar(&o.baseURL, "base-url", "https://api.deepseek.com/v1", "接入点根地址")
	fs.StringVar(&o.modelID, "model", "deepseek/chat", "内部模型 id（能力表键）")
	fs.StringVar(&o.remote, "remote", "deepseek-flash", "远端模型名（发给厂商的名字）")
	fs.StringVar(&o.bucketField, "bucket-field", wire.DefaultBucketField, "缓存桶字段名（user_id，见 ADR-0025）")
	fs.StringVar(&o.endpoint, "endpoint", "deepseek-main", "接入点名（缓存键维度）")
	fs.StringVar(&o.dbPath, "db", ".marl-mini/supervisor.db", "SQLite 数据库路径")
	fs.BoolVar(&o.dryRun, "dry-run", false, "脚本化回放：不联网，验证闭环骨架")
	fs.IntVar(&o.maxRounds, "max-rounds", 8, "主循环轮数上限")
	_ = fs.Parse(args)
	return o
}

func main() {
	o := parseOptions(os.Args[1:])
	if err := run(o); err != nil {
		fmt.Fprintf(os.Stderr, "mini: %v\n", err)
		os.Exit(1)
	}
}

// run 是一次完整交付检查：装配存储→技能→线路→Agent，跑完打印 Log。
func run(o options) error {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Minute)
	defer stop()
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}

	// --- 存储层（真相之源 + 投影 + 快照通道）---
	dbDir := filepath.Dir(o.dbPath)
	if dbDir != "." {
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			return fmt.Errorf("mkdir db dir: %w", err)
		}
	}
	st, err := store.OpenSQLite(o.dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()
	snapshots, err := store.NewSnapshotStore(root)
	if err != nil {
		return fmt.Errorf("snapshots: %w", err)
	}

	// --- 技能层（注册 + 命名空间沙箱）---
	// 挂载：整个工作区可写；.git 与 .marl/-mini 隐藏——更长的模式在同等
	// 具体度规则下胜出（ns.bestMount 的既有口径），于是共享前缀范围的
	// 目录对模型不可见。
	workNS := &types.Namespace{AgentID: "mini-agent", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
		{Pattern: ".git/**", Mode: types.PathHidden},
		{Pattern: ".marl/**", Mode: types.PathHidden},
		{Pattern: ".marl-mini/**", Mode: types.PathHidden},
	}}
	resolver, err := ns.NewResolver(root)
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite} {
		if err := reg.Register(sk); err != nil {
			return fmt.Errorf("register %s: %w", sk.Name(), err)
		}
	}

	// --- 线路层（阶段 1 的 Deepseek 适配器直连；Pool 的排队/熔断在阶段 3+）---
	key := os.Getenv(o.apiKeyEnv)
	if o.dryRun && key == "" {
		key = "dry-run-placeholder"
	}
	if !o.dryRun && key == "" {
		return fmt.Errorf("env %s is empty (real run needs a key; use -dry-run for offline replay)", o.apiKeyEnv)
	}
	// --- Agent（Binding 手工装配，逐字段按 types.Binding 不变量填）---
	// Binding 在 directLine 构造之前先定义——BuildRequest 与缓存桶字段都读它。
	binding := types.Binding{
		RungID:      "r0",
		RungIndex:   0,
		Endpoint:    o.endpoint,
		Model:       o.modelID,
		CacheBucket: "mini-agent", // 恒等于 AgentID（Patch 1）
		Wire:        types.WireOpenAIChat,
		Thinking:    types.ThinkingSpec{Level: "off"},
		BoundAt:     time.Now(),
		CachePrefix: wire.ModelCachePrefix(o.modelID, o.endpoint),
	}
	adapter, err := wire.NewDeepSeekChatAdapter(wire.DeepSeekChatConfig{
		EndpointName: o.endpoint,
		BaseURL:      o.baseURL,
		APIKey:       key,
		RemoteNames:  map[string]string{o.modelID: o.remote},
		BucketField:  o.bucketField,
	})
	if err != nil {
		return fmt.Errorf("adapter: %w", err)
	}
	norm, err := wire.NewOpenAICompatNormalizer(capsFor(o), nil)
	if err != nil {
		return fmt.Errorf("normalizer: %w", err)
	}
	// 直连通路的实现在 wire_line.go；Binding 先装配再注入（BuildRequest 需要）。
	dl := &directLine{
		norm:        norm,
		denorm:      wire.NewOpenAICompatDenormalizer(),
		adapter:     adapter,
		binding:     binding,
		bucketField: o.bucketField,
	}
	var llm agent.LLMExecutor = dl
	if o.dryRun {
		llm = &cannedLine{}
	}

	a, err := agent.New(agent.Config{
		ID:           "mini-agent",
		Depth:        0,
		SystemPrompt: systemPrompt,
		MaxRounds:    o.maxRounds,
		Log:          st,
		Views:        st,
		LLM:          llm,
		Skills:       reg,
		Namespace:    workNS,
		Resolver:     resolver,
		ProjectRoot:  root,
		Snapshots:    snapshots,
		Sampling:     types.SamplingParams{MaxTokens: 1024, TimeoutMs: 120_000},
		Thinking:     binding.Thinking,
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if err := a.SetBinding(binding); err != nil {
		return fmt.Errorf("binding: %w", err)
	}
	if err := a.AppendUser(ctx, defaultTask); err != nil {
		return fmt.Errorf("append task: %w", err)
	}

	// --- 跑起来（主循环）---
	if err := a.Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return printLog(ctx, a.ID(), st, o.dbPath)
}

// systemPrompt 是阶段 2 的 system 前缀（frozen；逐字节稳定的常量）。
const systemPrompt = `你是 Marl 的最小 Agent（阶段 2）。
任务流程：先用 list_dir 了解目录结构，再用 file_read 读必要文件，需要落盘时用 file_write。
回答用中文、简短；文件路径一律用工作区相对路径。`

// capsFor 是阶段 2 的能力表替身（与 probe 的 staticCaps 同源同取舍，注释在 wire_line.go）。
func capsFor(o options) *miniCaps {
	return &miniCaps{endpoint: o.endpoint, modelID: o.modelID}
}

// printLog 把 Log 全量打印（交付检查的"链条可见"判据）。
func printLog(ctx context.Context, id types.AgentID, st *store.SQLiteStore, dbPath string) error {
	last, err := st.LastSeq(ctx, id)
	if err != nil {
		return fmt.Errorf("lastSeq: %w", err)
	}
	if last == 0 {
		fmt.Println("（Log 为空）")
		return nil
	}
	entries, err := st.Range(ctx, id, 1, last)
	if err != nil {
		return fmt.Errorf("range: %w", err)
	}
	fmt.Println("━━━━━━━━━━ Log ━━━━━━━━━━")
	for _, e := range entries {
		line := e.Content
		if line == "" {
			if b, jerr := json.Marshal(e.Meta); jerr == nil {
				line = "meta=" + string(b)
			}
		}
		fmt.Printf("[%02d] %-7s %-14s %s\n", e.Seq, e.Role, e.Prov, truncateLine(line, 160))
	}
	// 交付检查的 SQL 验证提示：
	fmt.Printf("\n（可执行 sqlite3 %s \"SELECT seq, role, substr(content,1,80) FROM log_entries;\" 验证落库）\n", dbPath)
	return nil
}

// truncateLine 按字符截断（len 是字节口径，硬切多字节字符会打出乱码——
// 与 probe 的 truncate 同一教训）。
func truncateLine(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
