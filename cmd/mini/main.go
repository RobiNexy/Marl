// 命令 mini 是设计文档 13.4/13.5（阶段 2/3）的交付检查命令。
//
// 阶段 2：构造一个能"接收任务、调 LLM、解析 tool_call、调技能、再调 LLM"
// 的最小闭环，跑完打印 Log。交付判据：
//
//   - Log 里能看到完整的 tool_call → tool_result → assistant 链条；
//   - SQLite 文件里能 SELECT * FROM log_entries（见 -db 的落盘处）。
//
// 阶段 3（-task files）：加一个"读 10 个文件"的任务凑满上下文，触发压缩，
// 打印压缩前后的 token 估算。交付判据：
//
//   - 压缩事件发生，新 View 比旧 View 小 ≥20%；
//   - SUM 七章节齐全（机械校验通过才会被采纳）；
//   - 压缩后任务继续跑完（Log 真相完整保留）。
//
// 模型与接入点硬编码（阶段 2/3 没有配置层，与 13.3 probe 的做法一致）；
// DEEPSEEK_API_KEY 提供密钥（-dry-run 不需要，走脚本化回放）。
//
// 用法：
//
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/mini -task files   # 真跑（压缩闭环）
//	go run ./cmd/mini -task files -dry-run                 # 脚本化回放
//	go run ./cmd/mini                                      # 阶段 2 的 readme 任务
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
	"marl/internal/compress"
	"marl/internal/ns"
	"marl/internal/orchestrate"
	"marl/internal/skill"
	"marl/internal/store"
	"marl/internal/types"
	"marl/internal/wire"
)

const readmeTask = "列出当前目录，读 README.md"

// filesTaskTemplate 是阶段 3 的凑满上下文任务（一次一个文件——把轮数撑起来，
// 压缩区才有中段可言）。
const filesTaskTemplate = "依次读取 files/ 目录下的 %d 个文件（f01.txt 到 f%02d.txt）。"+
	"每次只调用一次 file_read、读完一个再读下一个；全部读完后给出不超过三句话的总结。"

// options 是命令行选项（阶段 2/3 的"配置层"就是它，与 probe 同一取舍）。
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
	task        string
	files       int
	ctxBudget   int
	keepTail    int
	minReclaim  float64
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
	fs.StringVar(&o.task, "task", "readme", "任务类型：readme（阶段 2）| files（阶段 3 压缩闭环）")
	fs.IntVar(&o.files, "files", 10, "files 任务的文件数")
	// ctx-budget 是演示口径的有效窗口（est token）：真实窗口来自能力表
	// （caps.MaxContext，64k），10 个小文件远凑不满；演示用小窗口让压缩
	// 在可观察的轮次内触发。阶段 4 的能力表接管后此 flag 退役。
	fs.IntVar(&o.ctxBudget, "ctx-budget", 0, "有效上下文窗口 est-token（0=禁用压缩；files 任务默认 4500）")
	fs.IntVar(&o.keepTail, "keep-tail", 2, "压缩保留的尾部轮数（设计默认 3，演示取 2 让中段更早出现）")
	fs.Float64Var(&o.minReclaim, "min-reclaim", 0.2, "压缩收益下限（交付判据：≥20%）")
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

// run 是一次完整交付检查：装配存储→技能→线路→Agent（→压缩器），跑完打印。
func run(o options) error {
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Minute)
	defer stop()

	// --- 工作区（files 任务用临时目录生成素材，不污染调用方 cwd）---
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}
	taskText := readmeTask
	if o.task == "files" {
		root, err = makeFilesWorkspace(o.files)
		if err != nil {
			return fmt.Errorf("files workspace: %w", err)
		}
		taskText = fmt.Sprintf(filesTaskTemplate, o.files, o.files)
		if o.maxRounds < o.files+4 {
			o.maxRounds = o.files + 4 // 每文件一轮 + 总结 + 余量
		}
		if o.ctxBudget == 0 {
			o.ctxBudget = 4500 // 演示口径（见 flag 注释）
		}
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
	// 挂载：整个工作区可写；.git 与 .marl* 隐藏——更长的模式在同等
	// 具体度规则下胜出（ns.bestMount 的既有口径）。
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

	// --- 线路层（阶段 1 的 Deepseek 适配器直连；Pool 的排队/熔断在阶段 4+）---
	key := os.Getenv(o.apiKeyEnv)
	if o.dryRun && key == "" {
		key = "dry-run-placeholder"
	}
	if !o.dryRun && key == "" {
		return fmt.Errorf("env %s is empty (real run needs a key; use -dry-run for offline replay)", o.apiKeyEnv)
	}
	// --- Agent（Binding 手工装配，逐字段按 types.Binding 不变量填）---
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
	var orchLLM compress.Executor = dl // 编排调用走同一条直连通路（结构化类型直通）
	if o.dryRun {
		canned := &cannedLine{task: o.task, files: o.files, root: root}
		llm = canned
		orchLLM = canned // 同一替身按请求形态分流（见 cannedLine.ExecuteTurn）
	}

	// --- 压缩器（阶段 3：独立预算 + 独立账本出口）---
	engine, err := compress.NewEngine(compress.EngineConfig{
		AgentID:        "mini-agent",
		TaskID:         "mini-task",
		LLM:            orchLLM,
		Sampling:       types.SamplingParams{MaxTokens: 1024, TimeoutMs: 120_000, Temperature: 0.3},
		MaxTotalTokens: 200000,
		Sink:           &stdoutSink{}, // 阶段 4 的 SQLite Ledger 接管（ADR-0026）
	})
	if err != nil {
		return fmt.Errorf("compress engine: %w", err)
	}
	comp, err := compress.NewCompressor(engine, root)
	if err != nil {
		return fmt.Errorf("compressor: %w", err)
	}

	a, err := agent.New(agent.Config{
		ID:           "mini-agent",
		Depth:        0,
		SystemPrompt: systemPromptFor(o.task),
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
		// 压缩装配：ctxBudget <= 0 即禁用（readme 任务保持阶段 2 行为）。
		MaxContextTokens: o.ctxBudget,
		Compression: func() *agent.CompressConfig {
			if o.ctxBudget <= 0 {
				return nil
			}
			return &agent.CompressConfig{
				Compressor: comp,
				Policy: orchestrate.CompressionPolicy{
					HeadroomThreshold:  1200,
					KeepTailTurns:      o.keepTail,
					MinReclaimFraction: o.minReclaim,
					MaxRetries:         1,
				},
				BudgetReserved: 1024,
			}
		}(),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if err := a.SetBinding(binding); err != nil {
		return fmt.Errorf("binding: %w", err)
	}
	if err := a.AppendUser(ctx, taskText); err != nil {
		return fmt.Errorf("append task: %w", err)
	}

	// --- 跑起来（主循环 + 压缩）---
	if err := a.Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	printCompressions(a)
	return printLog(ctx, a.ID(), st, o.dbPath)
}

// makeFilesWorkspace 在临时目录生成 files/f01..fNN.txt（每个 150 行，
// est ~10000 token/文件——单轮内容远大于 SUM 的固定成本，压缩收益才有
// 余量；50 行的小文件会让"一轮"与 SUM 等长，reclaim 贴地）。
func makeFilesWorkspace(n int) (string, error) {
	root, err := os.MkdirTemp("", "marl-mini-files-")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(root, "files"), 0o755); err != nil {
		return "", err
	}
	for i := 1; i <= n; i++ {
		var sb []byte
		for ln := 1; ln <= 150; ln++ {
			sb = append(sb, fmt.Sprintf("file %02d line %03d: the quick brown fox jumps over the lazy dog\r\n", i, ln)...)
		}
		if err := os.WriteFile(filepath.Join(root, "files", fmt.Sprintf("f%02d.txt", i)), sb, 0o644); err != nil {
			return "", err
		}
	}
	return root, nil
}

// stdoutSink 是编排账本的控制台出口（阶段 3 的"独立账本"可见形态；
// 阶段 4 换成 store.Ledger 的 SQLite 落地）。
type stdoutSink struct{}

func (stdoutSink) RecordOrchestration(_ context.Context, agentID types.AgentID, taskID types.TaskID, u *types.TokenUsage) error {
	if u == nil {
		return nil
	}
	fmt.Printf("[编排账本] agent=%s task=%s prompt=%d completion=%d cache_read=%d\n",
		agentID, taskID, u.PromptTokens, u.CompletionTokens, u.CacheReadTokens)
	return nil
}

// printCompressions 打印压缩事件（交付判据的"前后 token 估算"）。
func printCompressions(a *agent.Agent) {
	events := a.CompressionEvents()
	if len(events) == 0 {
		fmt.Println("━━━━━━━━━━ 压缩 ━━━━━━━━━━")
		fmt.Println("（未触发压缩）")
		return
	}
	fmt.Println("━━━━━━━━━━ 压缩 ━━━━━━━━━━")
	for _, ev := range events {
		kind := "SUM"
		if !ev.SUMAppended {
			kind = "L0-only"
		}
		fmt.Printf("[轮 %d] %-7s 估算 %d → %d est-token（收益 %.1f%%，L0 清理 %d 条）\n",
			ev.Round, kind, ev.OldTokens, ev.NewTokens, ev.Reclaim*100, ev.L0Pruned)
	}
}

// systemPromptFor 按任务返回冻结前缀（byte-stable 常量）。
func systemPromptFor(task string) string {
	if task == "files" {
		return `你是 Marl 的最小 Agent（阶段 3 压缩闭环验证）。
任务：按用户要求逐个读取 files/ 目录下的文件。每次回复只调用一次 file_read，
读完一个文件再读下一个；全部读完后给出简短总结。文件路径一律用工作区相对路径。`
	}
	return `你是 Marl 的最小 Agent（阶段 2）。
任务流程：先用 list_dir 了解目录结构，再用 file_read 读必要文件，需要落盘时用 file_write。
回答用中文、简短；文件路径一律用工作区相对路径。`
}

// capsFor 是阶段 2/3 的能力表替身（与 probe 的 staticCaps 同源同取舍，注释在 wire_line.go）。
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
		prov := string(e.Prov)
		if e.Prov != types.ProvOriginal {
			line = "«" + prov + "» " + line // 派生条目（SUM/分段）显式标注血缘
		}
		fmt.Printf("[%02d] %-7s %-14s %s\n", e.Seq, e.Role, prov, truncateLine(line, 160))
	}
	// 交付检查的 SQL 验证提示：
	fmt.Printf("\n（可执行 sqlite3 %s \"SELECT seq, role, prov, substr(content,1,80) FROM log_entries;\" 验证落库）\n", dbPath)
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
