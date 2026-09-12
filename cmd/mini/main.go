// 命令 mini 是设计文档 13.4/13.5/13.6（阶段 2/3/4）的交付检查命令。
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
// 阶段 4（-ladder）：配置阶梯（r0/r1/r2），从 r0 起跑；-task failing 是
// "tool_call 总是返回格式错误"的任务（dry-run 专用），两次失败后自动升级
// 到 r1。交付判据：
//
//   - 观察到升级日志（audit model_upgrade）；
//   - go run ./cmd/ladder_report 显示 r0/r1 两级的 token 与成本。
//
// 模型与接入点硬编码（配置层在阶段 7；DEEPSEEK_API_KEY 提供密钥，
// -dry-run 不需要，走脚本化回放）。
//
// 用法：
//
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/mini -task files -ladder ladder.yaml
//	go run ./cmd/mini -task files -dry-run
//	go run ./cmd/mini -task failing -dry-run   # 升级场景（确定性脚本）
//	go run ./cmd/mini                          # 阶段 2 的 readme 任务
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
	"marl/internal/ladder"
	"marl/internal/ledger"
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

// failingTask 是阶段 4 的升级触发任务（dry-run 专用：真跑无法强制模型
// 产出非法 JSON——升级路径的确定性验证由脚本回放承担）。
const failingTask = "读取 files/f01.txt（本任务的 tool_call 参数格式固定损坏，用于验证阶梯升级）。"

// options 是命令行选项（阶段 2-4 的"配置层"就是它，与 probe 同一取舍）。
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
	ladderPath  string
}

func parseOptions(args []string) options {
	var o options
	fs := flag.NewFlagSet("mini", flag.ContinueOnError)
	fs.StringVar(&o.apiKeyEnv, "api-key-env", "DEEPSEEK_API_KEY", "存放密钥的环境变量名")
	fs.StringVar(&o.baseURL, "base-url", "https://api.deepseek.com/v1", "接入点根地址")
	fs.StringVar(&o.modelID, "model", "deepseek-flash", "内部模型 id（= 厂商现行名，ADR-0031：消灭翻译间接层）")
	fs.StringVar(&o.remote, "remote", "deepseek-flash", "远端模型名（默认与 -model 相同；厂商改名时只改这里）")
	fs.StringVar(&o.bucketField, "bucket-field", wire.DefaultBucketField, "缓存桶字段名（user_id，见 ADR-0025）")
	fs.StringVar(&o.endpoint, "endpoint", "deepseek-main", "接入点名（缓存键维度）")
	fs.StringVar(&o.dbPath, "db", ".marl-mini/supervisor.db", "SQLite 数据库路径")
	fs.BoolVar(&o.dryRun, "dry-run", false, "脚本化回放：不联网，验证闭环骨架")
	fs.IntVar(&o.maxRounds, "max-rounds", 8, "主循环轮数上限")
	fs.StringVar(&o.task, "task", "readme", "任务类型：readme（阶段 2）| files（阶段 3 压缩）| failing（阶段 4 升级，dry-run 专用）")
	fs.IntVar(&o.files, "files", 10, "files 任务的文件数")
	// ctx-budget 是演示口径的有效窗口（est token）：真实窗口来自能力表
	// （caps.MaxContext，64k），10 个小文件远凑不满；演示用小窗口让压缩
	// 在可观察的轮次内触发。阶段 4 的能力表接管后此 flag 退役。
	fs.IntVar(&o.ctxBudget, "ctx-budget", 0, "有效上下文窗口 est-token（0=禁用压缩；files 任务默认 4500）")
	fs.IntVar(&o.keepTail, "keep-tail", 2, "压缩保留的尾部轮数（设计默认 3，演示取 2 让中段更早出现）")
	fs.Float64Var(&o.minReclaim, "min-reclaim", 0.2, "压缩收益下限（交付判据：≥20%）")
	fs.StringVar(&o.ladderPath, "ladder", "", "ladder.yaml 路径（空 = 单绑定，阶段 2/3 行为）")
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

// run 是一次完整交付检查：装配存储→技能→线路（→阶梯→账本）→Agent，跑完打印。
func run(o options) error {
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Minute)
	defer stop()

	// --- 工作区（files/failing 任务用临时目录生成素材，不污染调用方 cwd）---
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}
	taskText := readmeTask
	if o.task == "files" || o.task == "failing" {
		root, err = makeFilesWorkspace(o.files)
		if err != nil {
			return fmt.Errorf("files workspace: %w", err)
		}
		if o.task == "files" {
			taskText = fmt.Sprintf(filesTaskTemplate, o.files, o.files)
			if o.maxRounds < o.files+4 {
				o.maxRounds = o.files + 4
			}
			if o.ctxBudget == 0 {
				o.ctxBudget = 4500 // 演示口径（见 flag 注释）
			}
		} else {
			taskText = failingTask
			o.maxRounds = 8
		}
	}

	// --- 存储层（真相之源 + 投影 + 快照 + 账本 + 审计）---
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

	// --- 线路层与阶梯（阶段 4：-ladder 配置两阶段调度；无阶梯 = 单绑定）---
	key := os.Getenv(o.apiKeyEnv)
	if o.dryRun && key == "" {
		key = "dry-run-placeholder"
	}
	if !o.dryRun && key == "" {
		return fmt.Errorf("env %s is empty (real run needs a key; use -dry-run for offline replay)", o.apiKeyEnv)
	}
	if o.task == "failing" && !o.dryRun {
		return fmt.Errorf("task=failing 是 dry-run 专用（真跑无法强制模型产出非法 JSON）")
	}

	var (
		binding    types.Binding
		ladderCfg  *ladder.Config
		router     wire.Router
		cat        wire.Catalog
		upgrader   *agent.UpgradeConfig
		recorder   *ledger.Recorder
	)
	if o.ladderPath != "" {
		ladderCfg, err = ladder.Load(o.ladderPath)
		if err != nil {
			return fmt.Errorf("ladder: %w", err)
		}
		cat = buildCatalog(o, ladderCfg)
		router, err = ladder.NewRouter(cat.(*ladder.StaticCatalog), ladderCfg, ladder.RouterPolicy{PreferBonus: 1.0, CostWeight: 0.1})
		if err != nil {
			return fmt.Errorf("router: %w", err)
		}
		binding, err = router.Bind(types.Requirement{Require: []types.Capability{types.CapToolCall}},
			"mini-agent", ladderCfg.Ladder, 0)
		if err != nil {
			return fmt.Errorf("bind: %w", err)
		}
		recorder, err = ledger.New(st, cat)
		if err != nil {
			return fmt.Errorf("ledger: %w", err)
		}
		upgrader = &agent.UpgradeConfig{
			Router:      router,
			Ladder:      ladderCfg,
			Catalog:     cat,
			Requirement: types.Requirement{Require: []types.Capability{types.CapToolCall}},
			Policy: ladder.EvidencePolicy{
				Threshold: 0.8, FailureWeight: 0.4, FormatErrorWeight: 0.4,
				NoProgressWeight: 0.15, ChildFailureRateWeight: 1.0, ReclaimLowWeight: 0.1,
			},
		}
	} else {
		binding = types.Binding{
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
	}

	// --- Deepseek 适配器（远端名映射：阶梯模式从目录取，单绑定用 flag）---
	remoteNames := map[string]string{o.modelID: o.remote}
	if sc, ok := cat.(*ladder.StaticCatalog); ok {
		remoteNames = map[string]string{}
		for _, r := range ladderCfg.Ladder.Rungs {
			entry, err := sc.Model(r.Model)
			if err != nil {
				return fmt.Errorf("catalog model %s: %w", r.Model, err)
			}
			remoteNames[r.Model] = entry.RemoteName
		}
	}
	adapter, err := wire.NewDeepSeekChatAdapter(wire.DeepSeekChatConfig{
		EndpointName: o.endpoint,
		BaseURL:      o.baseURL,
		APIKey:       key,
		RemoteNames:  remoteNames,
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
	var orchLLM compress.Executor = dl
	if o.dryRun {
		canned := &cannedLine{task: o.task, files: o.files, root: root}
		llm = canned
		orchLLM = canned
	}

	// --- 压缩器（阶段 3：独立预算 + 独立账本出口；阶段 4 的 Ledger 接管）---
	engine, err := compress.NewEngine(compress.EngineConfig{
		AgentID:        "mini-agent",
		TaskID:         "mini-task",
		LLM:            orchLLM,
		Sampling:       types.SamplingParams{MaxTokens: 1024, TimeoutMs: 120_000, Temperature: 0.3},
		MaxTotalTokens: 200000,
		Sink:           orchSinkFor(recorder, binding),
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
		TaskID:  "mini-task",
		Ledger:  recorder,
		Audit:   store.AuditSQLite{SQLiteStore: st},
		Upgrader: upgrader,
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

	// --- 跑起来（主循环 + 压缩 + 升级）---
	if err := a.Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	printCompressions(a)
	return printLog(ctx, a.ID(), st, o.dbPath)
}

// buildCatalog 构造静态目录（阶段 4：代码注册，models.yaml 在阶段 7）。
//
// 计价是占位口径（DeepSeek 官方价随时间变化；报表的绝对金额在价格确认前
// 只做相对比较——见 StaticCatalog 的注释）。阶梯里出现的模型必须注册，
// 否则 Router fail fast。
func buildCatalog(o options, cfg *ladder.Config) *ladder.StaticCatalog {
	cat := ladder.NewStaticCatalog(cfg.Ladder)
	seen := map[string]bool{}
	for _, r := range cfg.Ladder.Rungs {
		if seen[r.Model] {
			continue
		}
		seen[r.Model] = true
		remote := r.Model
		if r.Model == o.modelID {
			remote = o.remote
		}
		if err := cat.AddModel(wire.ModelEntry{
			ID: r.Model, Provider: "deepseek", Wire: types.WireOpenAIChat, RemoteName: remote,
			Caps: wire.ModelCaps{
				Has:             []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
				MaxContext:      65536,
				MaxOutput:       8192,
				CacheMode:       wire.CacheImplicitPrefix,
				ThinkingControl: wire.ThinkControlLevel,
				ThinkingLevels:  []string{"none", "low", "high", "max"},
			},
		}); err != nil {
			panic(fmt.Sprintf("mini: register model %s: %v", r.Model, err))
		}
	}
	// 计价从 ladder.yaml 的 pricing 节注入（ADR-0029：单一来源；代码内
	// 不再有占位价格表）。
	for model, p := range cfg.Pricing {
		if err := cat.AddPricing(model, p); err != nil {
			panic(fmt.Sprintf("mini: register pricing %s: %v", model, err))
		}
	}
	if err := cat.AddEndpoint(wire.EndpointConfig{
		Name: o.endpoint, BaseURL: o.baseURL, KeyRef: "env:" + o.apiKeyEnv,
		MaxInflight: 4, RPM: 60,
	}); err != nil {
		panic(fmt.Sprintf("mini: register endpoint: %v", err))
	}
	return cat
}

// orchSinkFor 把 Ledger 适配成压缩引擎的编排账本出口（阶段 4 的接管点；
// 无阶梯模式返回控制台出口——没有计价来源）。
func orchSinkFor(rec *ledger.Recorder, binding types.Binding) compress.UsageSink {
	if rec == nil {
		return &stdoutSink{}
	}
	return &ledgerSink{rec: rec, binding: binding}
}

// ledgerSink 是 compress.UsageSink → ledger.Recorder 的适配（Binding 由
// 装配期固定：编排调用固定在起始档位 r0——Part 7.5"编排调用用 r0"）。
type ledgerSink struct {
	rec     *ledger.Recorder
	binding types.Binding
}

func (s *ledgerSink) RecordOrchestration(ctx context.Context, agentID types.AgentID, taskID types.TaskID, usage *types.TokenUsage) error {
	return s.rec.RecordOrchestration(ctx, taskID, agentID, s.binding, usage)
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

// stdoutSink 是编排账本的控制台出口（无阶梯模式的可见形态）。
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
	if task == "failing" {
		return `你是 Marl 的最小 Agent（阶段 4 升级验证）。
任务：读取用户指定的文件。`
	}
	return `你是 Marl 的最小 Agent（阶段 2）。
任务流程：先用 list_dir 了解目录结构，再用 file_read 读必要文件，需要落盘时用 file_write。
回答用中文、简短；文件路径一律用工作区相对路径。`
}

// capsFor 是阶段 2-4 的能力表替身（与 probe 的 staticCaps 同源同取舍，
// 注释在 wire_line.go；阶梯模式的目录在 buildCatalog）。
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
	fmt.Printf("\n（可执行 sqlite3 %s \"SELECT seq, role, prov, substr(content,1,80) FROM log_entries;\" 验证落库；"+
		"go run ./cmd/ladder_report -db %s -task mini-task 查看成本报表）\n", dbPath, dbPath)
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
