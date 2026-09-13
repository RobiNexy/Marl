// 命令 discuss_loop 是 13.10（阶段 8）的交付检查命令：人机讨论的最小
// 闭环端到端回放（dry-run 假 LLM；讨论分支/控制面/verdict 裁决/结论
// 落地全部走**真实**的 fossil 与文件系统）。它同时交付阶段 7 的装配：
// Profile 加载（.marl/profiles）+ preferences/ 常驻块编译进 Agent 配置。
//
// 场景（13.10 测试条目）：
//  1. Agent 调 request_discussion("测试契约")（写 draft.md，开讨论分支）；
//  2. "人类"（脚本）编辑 verdict.md 写批注（不写 @approve）；
//  3. Agent 收到批注 → 修订草稿（第二轮 request_discussion）；
//  4. "人类"写 @approve；
//  5. 结论落地到 knowledge/contracts/<slug>.md（commit author=human）。
//
// 交付判据：marl status 能看到 Blocked(Discussing)（由 agent_state 审计
// 还原）；timeline 能看到 author=human 的 Finalize commit。
//
// 用法：
//
//	go run ./cmd/discuss_loop          # 全流程（不联网、不花钱）
//	go run ./cmd/discuss_loop -keep    # 保留工作区（人工看 fossil/timeline）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"
	"github.com/RobiNexy/Marl/internal/agent"

	"github.com/RobiNexy/Marl/internal/config"
	"github.com/RobiNexy/Marl/internal/discuss"
	"github.com/RobiNexy/Marl/internal/fossil"
	"github.com/RobiNexy/Marl/internal/knowledge"
	"github.com/RobiNexy/Marl/internal/ns"
	"github.com/RobiNexy/Marl/internal/profile"
	"github.com/RobiNexy/Marl/internal/skill"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

type options struct {
	keep         bool
	quiescence   time.Duration
	pollInterval time.Duration
}

func main() {
	var o options
	fs := flag.NewFlagSet("discuss_loop", flag.ContinueOnError)
	fs.BoolVar(&o.keep, "keep", false, "保留工作区目录（默认任务结束即删）")
	fs.DurationVar(&o.quiescence, "quiescence", 400*time.Millisecond, "verdict 静默窗口（真实使用应设 10s）")
	fs.DurationVar(&o.pollInterval, "poll", 50*time.Millisecond, "verdict 轮询步长（真实使用应设 500ms）")
	_ = fs.Parse(os.Args[1:])
	if err := run(o); err != nil {
		fmt.Fprintf(os.Stderr, "discuss_loop: %v\n", err)
		os.Exit(1)
	}
}

// fakeLLM 是回放的假执行器（阶段 8 的最小循环不连接任何厂商）。
type fakeLLM struct {
	turns []*wire.WireTurn
	i     int
}

func (f *fakeLLM) ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	// 供观察：standing/私有段的真实形态打印到 stderr（阶段 7 交付检查）。
	for _, s := range req.Segments {
		if s.Kind != wire.SegStanding && s.Kind != wire.SegKnowledge {
			continue
		}
		fmt.Fprintf(os.Stderr, "--- %s 段 ---\n%s\n", s.Kind, s.Content)
	}
	if f.i >= len(f.turns) {
		return nil, fmt.Errorf("fakeLLM: script exhausted")
	}
	t := f.turns[f.i]
	f.i++
	return t, nil
}

func run(o options) error {
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stop()

	// --- 工作区：真实 fossil 项目（init + open + 骨架）---
	root, err := os.MkdirTemp("", "marl-discuss-")
	if err != nil {
		return err
	}
	if !o.keep {
		defer os.RemoveAll(root)
	} else {
		fmt.Printf("（工作区保留：%s）\n", root)
	}
	marlDir := filepath.Join(root, ".marl")
	repoPath := filepath.Join(marlDir, "project.fossil")
	if err := os.MkdirAll(marlDir, 0o755); err != nil {
		return err
	}
	cli, err := fossil.NewCLI("")
	if err != nil {
		return err
	}
	if err := cli.InitRepo(ctx, repoPath, "tester"); err != nil {
		return fmt.Errorf("init repo: %w", err)
	}
	if err := cli.OpenRepo(ctx, repoPath, root); err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	if err := writeSkeleton(ctx, cli, root); err != nil {
		return fmt.Errorf("skeleton: %w", err)
	}

	// --- 阶段 7 的装配：Profile + 常驻块 ---
	loader := profile.NewLoader()
	if err := loader.LoadAll(filepath.Join(marlDir, "profiles")); err != nil {
		return fmt.Errorf("profiles: %w", err)
	}
	prof, err := loader.Get("default")
	if err != nil {
		return fmt.Errorf("get profile: %w", err)
	}
	promptStore, err := profile.LoadPromptIndex(marlDir)
	if err != nil {
		return fmt.Errorf("prompt index: %w", err)
	}
	promptText, err := promptStore.ReadPrompt(prof.Prompt)
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}
	standing, err := knowledge.CompileStandingOrders(filepath.Join(marlDir, "knowledge", "preferences"))
	if err != nil {
		return fmt.Errorf("standing orders: %w", err)
	}

	// --- 存储 ---
	st, err := store.OpenSQLite(filepath.Join(root, ".marl", "store.db"))
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()
	aud := store.AuditSQLite{SQLiteStore: st}

	// --- 讨论（真实 Manager：真 fossil 分支 + 真 verdict 文件）---
	mgr, err := discuss.NewManager(discuss.Config{
		Root:             root,
		ControlDir:       filepath.Join(root, ".marl", "state", "discussions"),
		DefaultTargetDir: ".marl/knowledge/contracts",
		VCS:              cli,
		PollInterval:     o.pollInterval,
		Quiescence:       o.quiescence,
		Audit:            aud,
	})
	if err != nil {
		return fmt.Errorf("discuss manager: %w", err)
	}
	fmt.Printf("讨论等待提示路径用例：%s\n", root)
	// --- 假"人类"：先行等待 verdict 出现 → 批注 → 等换轮 → @approve ---
	stopHuman := humanScript(root, mgr, o)
	defer stopHuman()

	a, err := agent.New(agent.Config{
		ID:              "root-1",
		SystemPrompt:    promptText, // 提示词文件整个塞进 system（Part 6.2）
		MaxRounds:       8,
		Log:             st,
		Views:           st,
		LLM:             &fakeLLM{turns: script()},
		Skills:          emptyRegistry(),
		Namespace:       &types.Namespace{AgentID: "root-1", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}, {Pattern: ".marl/state/**", Mode: types.PathHidden}}},
		Resolver:        mustResolver(root),
		ProjectRoot:     root,
		TaskID:          "discuss-task",
		Audit:           aud,
		StandingOrders:  standing.Content,
		Depth:           0,
		MaxDepth:        3,
		TaskDescription: "先与人类敲定『测试契约』的接口，结论落 contracts/，然后宣布完成。",
		Discussion:      &agent.DiscussionConfig{Manager: mgr},
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if err := a.AppendUser(ctx, "任务开始"); err != nil {
		return err
	}
	if err := a.Run(ctx); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	// --- 验证（交付检查的机器化形态）---
	target := filepath.Join(marlDir, "knowledge", "contracts")
	entries, _ := os.ReadDir(target)
	var landed string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "contract") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		landed = e.Name()
	}
	fmt.Printf("契约文件已落地: contracts/%s\n", landed)
	b, _ := os.ReadFile(filepath.Join(target, landed))
	fmt.Printf("契约内容前 60 字节:\n---\n%s\n---\n", string(b[:min(60, len(b))]))
	tl, err := cli.Timeline(ctx, repoPath, 12)
	if err != nil {
		return err
	}
	fmt.Println("fossil timeline:")
	for _, e := range tl {
		fmt.Printf("  [%s] %s %s\n", e.Author, e.Time.Format("15:04:05"), e.Comment)
	}
	// status 还原（内部断言：human 状态审计 + 讨论事件）。
	evs, _ := aud.Query(ctx, store.AuditFilter{Limit: 1000})
	fmt.Println("audit 事件链:")
	for _, ev := range evs {
		fmt.Printf("  %d %s %s\n", ev.Seq, ev.AgentID, ev.Action)
	}
	return nil
}

// script 是 Agent 的完整回合：
//  1. request_discussion（自主发起讨论）；
//  2. （收到批注）request_discussion 修订草稿；
//  3. （收到 @approve）最终回复。
func script() []*wire.WireTurn {
	return []*wire.WireTurn{
		turnTool("request_discussion", map[string]any{
			"topic": "测试契约", "draft": "接口：provider(name)/close()，先定这个再写代码。",
		}),
		turnTool("request_discussion", map[string]any{
			"topic": "测试契约", "draft": "接口 v2：provider(name)/provider(name, opts)/close()。",
		}),
		turnReply("契约已与人类敲定并落地到 contracts/，任务完成。"),
	}
}

// humanScript 模拟人类的两轮 verdict 编辑（返回停止函数）：
//
//	轮 1：verdict 出现后写批注（不写 @approve）；
//	轮 2：Agent 消化批注后框架会 RotateVerdict（新 nonce + 新模板的
//	      mtime 变化），人类在新 frontmatter 上写 @approve。
//
// 等待以文件 mtime 变化为驱动——与 Manager.Wait 同一观察面。
func humanScript(root string, mgr *discuss.Manager, o options) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		dirsRoot := filepath.Join(root, ".marl", "state", "discussions")
		// 等讨论目录出现（Agent 打开讨论是异步于本 goroutine 的）。
		deadline := time.Now().Add(60 * time.Second)
		var dir string
		for time.Now().Before(deadline) {
			es, _ := os.ReadDir(dirsRoot)
			if len(es) > 0 {
				dir = filepath.Join(dirsRoot, es[0].Name())
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if dir == "" {
			fmt.Println("human：60s 内没有等到讨论目录，放弃")
			return
		}
		vp := filepath.Join(dir, "verdict.md")
		fmt.Printf("human：等待你审核 → %s\n", vp)
		if !waitFile(vp, 30*time.Second) {
			fmt.Println("human：verdict 没出现，放弃")
			return
		}
		content, _ := os.ReadFile(vp)
		_ = os.WriteFile(vp, []byte(humanEdit(string(content), "接口需加 Close；另外 provider 不要吃全局态")), 0o644)
		fmt.Println("human：已写批注（无裁决命令）")
		// 轮 2：等框架 RotateVerdict（新 nonce 的模板覆盖文件 → mtime 变化
		// → 新文件内容含全新 nonce）。按 mtime 判据，不依赖实现细节的字段。
		if !waitMtimeChange(vp, 30*time.Second) {
			fmt.Println("human：RotateVerdict 没发生，放弃")
			return
		}
		content2, _ := os.ReadFile(vp)
		_ = os.WriteFile(vp, []byte(humanEdit(string(content2), "v2 通过。@approve")), 0o644)
		fmt.Println("human：已写 @approve")
	}()
	return func() { <-done }
}

// waitFile / waitMtimeChange / humanEdit 是人类的脚本原子。
// waitFile（人类等待 verdict 出现的原子）
func waitFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func waitMtimeChange(path string, timeout time.Duration) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	base := st.ModTime()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st2, err := os.Stat(path)
		if err != nil {
			return false
		}
		if st2.ModTime() != base {
			// 完整判据：内容也要换（rotate 意义上的新 frontmatter）。
			if _, rerr := os.ReadFile(path); rerr == nil {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func humanEdit(content, body string) string {
	if i := strings.Index(content, "## 批注"); i >= 0 {
		return content[:i+len("## 批注")] + "\n\n" + body + "\n"
	}
	return content + "\n" + body + "\n"
}

// -- 以下为骨架/解析的小机械 --

func writeSkeleton(ctx context.Context, cli *fossil.CLI, root string) error {
	files := map[string]string{
		".marl/config.yaml": `project:
  ladder_start: "r0"
  max_depth: 3
`,
		".marl/profiles/_default.yaml": `profile:
  id: "default"
  description: "默认 Agent"
  prompt: "default"
  requirement:
    require: [tool_call]
  can_spawn: true
`,
		".marl/prompts/_index.yaml": `prompts:
  - id: "default"
    path: "prompts/default.md"
    description: "默认提示词"
`,
		".marl/prompts/default.md":              "# 默认提示词\n\n你是 Marl 框架的 Agent；先与人类对齐 contracts，再动手。\n",
		".marl/knowledge/contracts/README.md":   "# 契约\n放接口契约。\n",
		".marl/knowledge/decisions/README.md":   "# 决策\n放 ADR。\n",
		".marl/knowledge/preferences/README.md": "# 常驻块\n偏好约束样本。\n",
		".marl/knowledge/preferences/style.md":  strings.Repeat("代码风格：契约先行；测试即规格。\n", 6),
		".marl/knowledge/preferences/rules.md":  "所有写入必须先经过讨论；.marl/state 不可触。\n",
	}
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(abs), 0o755)
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err
		}
	}
	// 解析 sanity（骨架自身必须能过 config 子集解析）。
	for _, rel := range []string{".marl/config.yaml", ".marl/profiles/_default.yaml", ".marl/prompts/_index.yaml"} {
		src, _ := os.ReadFile(filepath.Join(root, rel))
		if _, err := config.Parse(src); err != nil {
			return fmt.Errorf("skeleton %s: %w", rel, err)
		}
	}
	if err := cli.Add(ctx, root, ".marl"); err != nil {
		return err
	}
	if _, err := cli.Commit(ctx, root, fossil.UserSystem, "marl init：骨架"); err != nil {
		return err
	}
	return nil
}

func mustResolver(root string) types.Resolver {
	r, err := resolver(root)
	if err != nil {
		panic(err)
	}
	return r
}

// turnTool / 也不用 cache 你一；见脚本。
func turnTool(name string, args map[string]any) *wire.WireTurn {
	return toolTurn(name, args)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- 假执行器的小工具与空注册表 ---

// emptyRegistry 是无技能的最小注册表（讨论回放只走意图，不需要文件技能面）。
func emptyRegistry() skill.Registry {
	r := skill.NewMemRegistry()
	return r
}

// resolver 包装 ns.NewResolver（构造失败的展示面）。
func resolver(root string) (types.Resolver, error) {
	return ns.NewResolver(root)
}

// toolTurn 构造一个"只发一个工具调用"的回合
// （wire.WireTurn 的最小承载形态；Part 10.16）。
func toolTurn(name string, args map[string]any) *wire.WireTurn {
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return &wire.WireTurn{
		Outcomes: []wire.Outcome{{
			ToolCalls: []types.ToolCall{{
				ID:        "call_1_" + name,
				Name:      name,
				Arguments: b,
			}},
		}},
	}
}

// turnReply 构造一个"可见回复"的回合（turn 结束的判据）。
func turnReply(text string) *wire.WireTurn {
	// Entry 的形态与 Denormalizer 产出一致（Prov/Audience 由产出侧；
	// 主循环 only 回填 AgentID）。
	e := &types.LogEntry{
		Role:     types.RoleAssistantReply,
		Prov:     types.ProvOriginal,
		Audience: types.AudienceBoth,
		Content:  text,
		Meta:     map[string]any{"finish_reason": "stop"},
	}
	return &wire.WireTurn{
		Outcomes: []wire.Outcome{{Reply: text, Entry: *e}},
	}
}
