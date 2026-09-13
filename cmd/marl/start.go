// start/say：统一 Actor 模型的人类侧命令面（Part 14.6 / 14.12 #6）。
//
//	marl start "任务"   —— 人类 spawn 项目 Agent：HumanActor 登记为监督树
//	                      的根（depth=0，caps 全量），项目 Agent 经正常
//	                      Spawner 裁决创建（requester = 人类——旧 bootstrap
//	                      特例删除）；任务作为 fork 的不可变输入（Part 8.3）。
//	                      完成后项目 Agent 的 report 投给人类收件箱。
//	marl say "文本"     —— 人类发给项目 Actor 的 MsgDirect（Part 14.5；
//	                      异步注入，下一轮编排自然看到）。
//
// 装配边界（诚实清单）：本命令是"attached 运行"形态——start 的生命周期
// 就是命令进程的生命周期（没有 daemon）。say 靠控制面文件后端的跨进程
// 通道：写 inbox/direct_<ulid>.md，运行中的 start 经 Receive 泵把信封投
// 进 Agent 的信箱。daemon 化（长驻 + IPC）是后续阶段；本命令不假装它是。
//
// [运行纪律] 不要把 stdout/stderr 重定向进项目目录（`> start.log` 之类）
// ——工作区是 Agent 的命名空间，运行期产物落进去会被模型 file_read（真机
// 实录：模型读 start.log 后把绝对路径带进工具调用，触发一串失败）；日志
// 的归宿是控制面（~/.local/state/marl/<project>/）。
//
// 用法：
//
//	DEEPSEEK_API_KEY=sk-... go run ./cmd/marl start -dir ~/demo "读 README.md 并总结"
//	go run ./cmd/marl say -dir ~/demo -to sub_000001 "补充一句要求"

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/agent"
	"github.com/RobiNexy/Marl/internal/config"
	"github.com/RobiNexy/Marl/internal/discuss"
	"github.com/RobiNexy/Marl/internal/escalate"
	"github.com/RobiNexy/Marl/internal/fossil"
	"github.com/RobiNexy/Marl/internal/gate"
	"github.com/RobiNexy/Marl/internal/ladder"
	"github.com/RobiNexy/Marl/internal/ledger"
	"github.com/RobiNexy/Marl/internal/ns"
	"github.com/RobiNexy/Marl/internal/profile"
	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/skill"
	"github.com/RobiNexy/Marl/internal/spawner"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
	"github.com/RobiNexy/Marl/internal/wire"
)

// cmdStart 处理 start 子命令（人类 spawn 项目 Agent 并 attached 等完成）。
func cmdStart(args []string) error {
	fs := flag.NewFlagSet("marl start", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	db := fs.String("db", "", "存储数据库路径（缺省 <dir>/.marl/store.db）")
	detach := fs.Bool("detach", false, "后台运行（日志落控制面；marl stop 停止 / marl status 跟踪）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.Join(fs.Args(), " ")
	if task == "" {
		return fmt.Errorf("usage: marl start [-dir <dir>] \"任务描述\"")
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if *db == "" {
		*db = filepath.Join(root, ".marl", "store.db")
	}
	if *detach {
		return detachStart(root, *db, task)
	}
	return runStart(root, *db, task, nil)
}

// detachStart 把 runStart 放进后台进程（setsid 新会话——脱离当前进程组，
// 终端关闭不带走它；日志与 PID 落控制面）。控制面上的 run.lock 是"已有
// 后台任务在跑"的判据（stop/status 与防双跑都读它）。
func detachStart(root, dbPath, task string) error {
	controlRoot := controlRootOf(root)
	lockPath := filepath.Join(controlRoot, "run.lock")
	if pid, ok := readRunLock(lockPath); ok && processAlive(pid) {
		return fmt.Errorf("已有后台任务在跑（PID %d，控制面 %s）；先 marl stop 或继续观察 marl status", pid, controlRoot)
	}
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir control plane: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(controlRoot, "start.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open control-plane log: %w", err)
	}
	defer logFile.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "start", "-dir", root, "-db", dbPath, task)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("pid: %d\nstarted_at: %s\ntask: %s\n",
		cmd.Process.Pid, time.Now().UTC().Format(time.RFC3339), task)), 0o644); err != nil {
		return err
	}
	fmt.Printf("后台任务已启动（PID %d）\n  日志：%s\n  跟踪：marl status；插话：marl say；停止：marl stop\n", cmd.Process.Pid, controlRoot)
	return nil
}

// cmdStop 终止后台任务（Part 14.5 的 MsgShutdown 语义 + 兜底强杀）。
func cmdStop(args []string) error {
	fs := flag.NewFlagSet("marl stop", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	force := fs.Bool("force", false, "跳过优雅等待直接 SIGKILL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	controlRoot := controlRootOf(filepathAbs(*dir))
	lockPath := filepath.Join(controlRoot, "run.lock")
	pid, ok := readRunLock(lockPath)
	if !ok || !processAlive(pid) {
		_ = os.Remove(lockPath) // 陈旧锁清理
		return fmt.Errorf("没有在跑的后台任务（控制面 %s）", controlRoot)
	}
	if !*force {
		if p, perr := os.FindProcess(pid); perr == nil {
			_ = p.Signal(syscall.SIGTERM) // 优雅信号（进程自行收尾：view 落盘 / 代报）
		}
		// 等待收尾（最多 8s；活着一律转强杀）。
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) && processAlive(pid) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if processAlive(pid) {
		if p, perr := os.FindProcess(pid); perr == nil {
			_ = p.Kill()
		}
		fmt.Printf("已强制终止（PID %d）\n", pid)
	} else {
		fmt.Printf("后台任务已停止（PID %d）\n", pid)
	}
	_ = os.Remove(lockPath)
	return nil
}

// readRunLock 读 run.lock 的 pid（文件损坏/无 pid = false——当作没在跑）。
func readRunLock(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "pid:"); ok {
			if pid, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && pid > 0 {
				return pid, true
			}
		}
	}
	return 0, false
}

// processAlive 报告进程是否存在（信号 0 探测）。
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// filepathAbs 的小包装（cmdStop 的参数收敛）。
func filepathAbs(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// cmdSay 处理 say 子命令（人类 → Actor 的直接消息）。
func cmdSay(args []string) error {
	fs := flag.NewFlagSet("marl say", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录")
	to := fs.String("to", "", "目标 Actor id（缺省 project——运行中 start 的项目 Agent 约定 id）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" {
		return fmt.Errorf("usage: marl say [-dir <dir>] [-to <actor-id>] \"文本\"")
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	backend, err := actor.NewFileBackend(actor.FileConfig{
		Root: controlRootOf(root), Human: actor.HumanID(osUID()),
	})
	if err != nil {
		return err
	}
	defer backend.Stop()
	env := actor.Envelope{
		From:    actor.HumanID(osUID()),
		To:      actorIDOf(*to),
		Type:    proto.MsgDirect,
		Payload: &proto.DirectMessage{Text: text},
	}
	if err := backend.Deliver(env); err != nil {
		return err
	}
	controlRoot := controlRootOf(root)
	fmt.Printf("已投递 MsgDirect → %s（收件箱 %s/inbox/）\n", env.To, controlRoot)
	fmt.Println("运行中的 marl start 会在下一轮编排看到这条消息；无运行中的 start 时它留在收件箱。")
	return nil
}

// runStart 是一次完整的 attached 运行：装配 → 人类登记 → 正常裁决建
// 项目 Agent → 等它的 report 回到收件箱。
func runStart(root, dbPath, task string, _ []string) error {
	// 优雅终止的双入口：Ctrl-C（attached）与 marl stop（SIGTERM/detach）。
	// ctx 取消 = Agent 的 Run 收尾（View 落盘 + 代报兜底），不是硬杀。
	sigCtx, sigStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer sigStop()
	ctx, cancel := context.WithTimeout(sigCtx, 30*time.Minute)
	defer cancel()

	// 后台任务（detach）的运行锁在进程退出时摘除。
	defer os.Remove(filepath.Join(controlRootOf(root), "run.lock"))

	// --- 存储与技能（与 mini 同一形态：真相之源 + 投影 + 账本 + 审计）---
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()
	aud := store.AuditSQLite{SQLiteStore: st}
	reg := skill.NewMemRegistry()
	for _, sk := range []skill.Skill{skill.ListDir, skill.FileRead, skill.FileWrite,
		skill.ShellExec,
		skill.OrchExclude, skill.OrchRestore, skill.OrchReorder, skill.OrchAnnotate, skill.OrchPin} {
		if err := reg.Register(sk); err != nil {
			return fmt.Errorf("register %s: %w", sk.Name(), err)
		}
	}
	resolver, err := ns.NewResolver(root)
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	chk, err := spawner.NewReportChecker(spawner.ReportCheckerConfig{Root: root, MaxScanFiles: 50})
	if err != nil {
		return fmt.Errorf("report checker: %w", err)
	}

	// --- 项目配置（[阶段 12 修正/真机发现 #4] start 接 .marl/config.yaml
	// ——limits / gate_rules 的项目覆盖面；笔误显式报错不静默）---
	cfgRaw, err := os.ReadFile(filepath.Join(root, ".marl", "config.yaml"))
	if err != nil {
		return fmt.Errorf("read project config: %w（先 marl init）", err)
	}
	cfgNode, err := config.Parse(cfgRaw)
	if err != nil {
		return fmt.Errorf("parse config.yaml: %w", err)
	}
	limits, err := config.ParseLimits(cfgNode)
	if err != nil {
		return fmt.Errorf("config limits: %w", err)
	}
	projectRules, err := config.ParseGateRules(cfgNode)
	if err != nil {
		return fmt.Errorf("config gate_rules: %w", err)
	}
	if len(projectRules) == 0 {
		projectRules = orchRules() // 项目未配规则表 → 框架缺省（发现 #5 的维度缺省政策兜底）
	}

	// --- Gate（PDP + grants/ 落盘；重启回插人类批过的 always）---
	controlRoot := controlRootOf(root)
	grants, err := gate.NewGrantStore(controlRoot)
	if err != nil {
		return err
	}
	pdp, err := gate.NewManager(gate.ManagerConfig{
		Rules:     projectRules,
		LLMLimits: &gate.LLMLimits{MaxCalls: limits.CallTaskMax, MaxTokens: int64(limits.CallTaskMaxTokens)},
		Grants:    grants, Audit: aud,
	})
	if err != nil {
		return err
	}
	saved, err := grants.LoadGrants(ctx)
	if err != nil {
		return fmt.Errorf("load grants: %w", err)
	}
	for _, g := range saved {
		if err := pdp.AddGrant(ctx, &gate.Request{Kind: gate.Kind(g.Rule.Match["kind"])},
			gate.Grant{Mode: gate.GrantAlways, Reason: g.Rule.Reason}, g.GrantedBy); err != nil {
			return fmt.Errorf("regrant %s: %w", g.Rule.ID, err)
		}
	}
	if len(saved) > 0 {
		fmt.Printf("已恢复 %d 条永久放行规则（grants/）\n", len(saved))
	}

	// --- Profile（[发现 #3] 采样来自 .marl/profiles——消除硬编码 2048 的
	// 截断根源；Part 6 的既有设计）---
	profLoader := profile.NewLoader()
	if err := profLoader.LoadAll(filepath.Join(root, ".marl", "profiles")); err != nil {
		return fmt.Errorf("load profiles: %w", err)
	}
	prof, err := profLoader.Get("default")
	if err != nil {
		return fmt.Errorf("profile default: %w", err)
	}
	sampling := prof.Sampling
	if sampling.MaxTokens <= 0 || sampling.MaxTokens > 8192 {
		sampling.MaxTokens = 8192 // 目录能力的输出上限（startCatalog 的 ModelCaps）
	}
	if sampling.TimeoutMs <= 0 {
		sampling.TimeoutMs = 120_000
	}

	// --- 线路（真跑形态：DEEPSEEK_API_KEY 在场；每 Agent 独立 Binding
	// ——缓存桶 = AgentID 的 Patch 1 契约）---
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		return fmt.Errorf("env DEEPSEEK_API_KEY is empty（真跑形态需要密钥）")
	}
	newLine := func(agentID types.AgentID) (types.Binding, agent.LLMExecutor, error) {
		adapter, err := wire.NewDeepSeekChatAdapter(wire.DeepSeekChatConfig{
			EndpointName: "deepseek-main",
			BaseURL:      "https://api.deepseek.com/v1",
			APIKey:       key,
			RemoteNames:  map[string]string{"deepseek-flash": "deepseek-flash"},
			BucketField:  wire.DefaultBucketField,
		})
		if err != nil {
			return types.Binding{}, nil, err
		}
		// 能力源 = 目录（Normalizer 的档位翻译面；nil 是装配错误）。
		norm, err := wire.NewOpenAICompatNormalizer(startCatalog(), nil)
		if err != nil {
			return types.Binding{}, nil, err
		}
		b := startBinding(agentID)
		return b, &startLine{
			norm: norm, denorm: wire.NewOpenAICompatDenormalizer(),
			adapter: adapter, binding: b,
		}, nil
	}

	// --- 账本（单模型静态目录；价格从内嵌最小面——相对比较口径）---
	rec, err := ledger.New(st, startCatalog())
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}

	// --- Spawner（统一进程表；max_depth 只记 AI→AI——Part 14.6）---
	spw, err := spawner.New(spawner.Config{
		MaxDepth:        3,
		MaxActive:       16,
		MaxForkRounds:   8,
		CanSpawnAtDepth: func(int) bool { return true },
		Log:             st,
		Audit:           aud,
	})
	if err != nil {
		return fmt.Errorf("spawner: %w", err)
	}

	// --- 人类 Actor（监督树的根；文件后端 = 收件箱）---
	backend, err := actor.NewFileBackend(actor.FileConfig{
		Root: controlRoot, Human: actor.HumanID(osUID()),
	})
	if err != nil {
		return err
	}
	defer backend.Stop()
	human, err := actor.NewHuman(actor.HumanID(osUID()), backend, actor.HumanCaps())
	if err != nil {
		return err
	}
	if err := spw.RegisterHuman(ctx, human); err != nil {
		return fmt.Errorf("register human: %w", err)
	}
	fmt.Printf("人类 Actor：%s\n收件箱：%s/inbox/\n", human.ID(), controlRoot)

	// --- 人类侧装配（HumanLink）+ 收件箱泵（回执 → 目标 Agent 信箱）---
	humanID := human.ID()
	link := spawnerHumanLink{spw: spw, human: humanID}
	go actor.PumpReceive(ctx, backend, func(env actor.Envelope) error {
		// MsgGateReply / MsgDirect 的回程路由（From/To 由文件 frontmatter
		// 与解析器保证——原则 4 的通道属性）。
		return spw.SendTo(env.To, env)
	})

	// --- 讨论 / escalation / fossil（[发现 #4] 装配补全：引擎全实现，
	// start 此前未接）---
	cli, err := fossil.NewCLI("")
	if err != nil {
		return fmt.Errorf("fossil: %w", err)
	}
	repo := filepath.Join(root, ".marl", "project.fossil")
	discussMgr, err := discuss.NewManager(discuss.Config{
		Root: root, MARLDir: filepath.Join(root, ".marl"),
		ControlDir:       filepath.Join(controlRoot, "discussions"),
		DefaultTargetDir: filepath.Join(".marl", "knowledge", "contracts"),
		VCS:              cli,
		Audit:            aud,
	})
	if err != nil {
		return fmt.Errorf("discuss manager: %w", err)
	}
	escMailbox, err := escalate.NewMailbox(escalate.MailboxConfig{
		ControlRoot: controlRoot, Audit: aud,
	})
	if err != nil {
		return fmt.Errorf("escalate mailbox: %w", err)
	}
	escMgr, err := escalate.NewManager(escalate.Config{
		// 根 Agent（项目 Agent）的父就是人类 Actor——链条到人类是拓扑
		// 事实（Part 14.6）；FallbackHuman 的文件信箱 = 人类收件箱的
		// escalation 编码（requests/ 子树）。
		Rule: func() (proto.EscalationRule, error) {
			return proto.EscalationRule{FallbackHuman: true}, nil
		},
		Mailbox: escMailbox,
	})
	if err != nil {
		return fmt.Errorf("escalate manager: %w", err)
	}

	// --- 工厂（项目 Agent 与子 Agent 同一入口）---
	factory := &startFactory{st: st, reg: reg, root: root, resolver: resolver, chk: chk,
		spw: spw, newLine: newLine, pdp: pdp, rec: rec, human: link,
		sampling: sampling, discuss: discussMgr, esc: escMgr, vcs: cli, repo: repo}
	if err := spw.SetFactory(factory); err != nil {
		return err
	}

	// --- 正常裁决创建项目 Agent（requester = 人类；AI 第 1 层）---
	dec, err := spw.Adjudicate(ctx, &proto.SpawnRequest{
		RequesterID:     humanID,
		ProfileID:       "default",
		TaskDescription: task,
		WritablePaths:   []string{"**"},
	})
	if err != nil {
		return fmt.Errorf("project agent adjudicate: %w", err)
	}
	if dec.Status != proto.SpawnApproved {
		return fmt.Errorf("project agent rejected: %+v", dec)
	}

	// --- attached 等待：项目 Agent 的 report 回到人类收件箱 ---
	fmt.Printf("项目 Agent：%s（运行中——Ctrl-C 中断）\n\n", dec.ChildAgentID)
	reportSeen := false
	for !reportSeen {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待超时/中断（收件箱：%s/inbox/）", controlRoot)
		case <-time.After(300 * time.Millisecond):
		}
		// report 是纯投递形态（无回执路径）——收件箱里等第一份。
		entries, rerr := os.ReadDir(filepath.Join(controlRoot, "inbox"))
		if rerr != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "report_") {
				continue
			}
			reportSeen = true
			body, _ := os.ReadFile(filepath.Join(controlRoot, "inbox", e.Name()))
			fmt.Printf("━━ 收件箱：%s ━━\n%s\n", e.Name(), frontmatterStripped(string(body)))
		}
	}
	fmt.Printf("任务完成：report 已在收件箱（%s/inbox/）；Agent 树与账本可用 marl status / ladder_report 查看。\n", controlRoot)
	return nil
}

// ---- 装配小件 ----

// spawnerHumanLink 是 agent.HumanLink 的装配实现（包着 Spawner 的统一
// 投递面 + 人类 Actor 的注册行——Watchdog 的挂起登记也在这里）。
type spawnerHumanLink struct {
	spw   *spawner.Spawner
	human types.AgentID
}

func (l spawnerHumanLink) HumanID() types.AgentID { return l.human }

func (l spawnerHumanLink) SendToHuman(ctx context.Context, env proto.Envelope) error {
	return l.spw.SendTo(l.human, env)
}

func (l spawnerHumanLink) MarkPending(agent types.AgentID, kind string) {
	l.spw.MarkPending(agent, kind)
}

func (l spawnerHumanLink) ClearPending(agent types.AgentID) { l.spw.ClearPending(agent) }

// startFactory 是 attached 运行的 ChildFactory（项目 Agent 与子同一入口；
// 每个独立 Binding——缓存桶 = AgentID）。
type startFactory struct {
	st       *store.SQLiteStore
	reg      skill.Registry
	root     string
	resolver types.Resolver
	chk      proto.ReportChecker
	spw      *spawner.Spawner
	newLine  func(types.AgentID) (types.Binding, agent.LLMExecutor, error)
	pdp      *gate.Manager
	rec      *ledger.Recorder
	human    spawnerHumanLink
	// [发现 #3/#4] 采样来自 Profile；讨论/escalation/fossil 的装配块。
	sampling types.SamplingParams
	discuss  *discuss.Manager
	esc      *escalate.Manager
	vcs      *fossil.CLI
	repo     string
}

func (f *startFactory) BuildChild(ctx context.Context, plan *spawner.ChildPlan, req *proto.SpawnRequest) (spawner.ChildRunner, error) {
	b, llm, err := f.newLine(plan.ID)
	if err != nil {
		return nil, err
	}
	cfg := agent.Config{
		ID: plan.ID, ParentID: plan.ParentID, Depth: plan.Depth, MaxDepth: 3,
		Mailbox:      plan.Mailbox,
		SystemPrompt: "你是 Marl 的 Agent：按任务工作；需要分治时 fork 子 Agent；完成时 report_to_parent。",
		MaxRounds:    16,
		Log:          f.st, Views: f.st,
		LLM: llm, Skills: f.reg,
		Namespace: plan.Namespace, Resolver: f.resolver, ProjectRoot: f.root,
		Sampling: f.sampling,
		TaskID:   "start-task",
		Audit:    store.AuditSQLite{SQLiteStore: f.st},
		Spawner:  f.spw, ReportSink: f.spw, ReportChecker: f.chk,
		Human: f.human,
		LLMCall: &agent.LLMCallConfig{
			Limits: config.DefaultLimits(),
			Gates:  f.pdp,
		},
		Ledger:     f.rec,
		Discussion: &agent.DiscussionConfig{Manager: f.discuss},
		Escalation: &agent.EscalationConfig{Manager: f.esc},
	}
	if plan.Depth == 1 {
		// 项目 Agent 根：单写者提交面（只有父 Agent 装配——提交点在
		// 父的唯一代码路径上，Part 8.4；仓库由 marl init 建）。
		cfg.Committer = &agent.CommitConfig{VCS: f.vcs, RepoPath: f.repo}
	}
	a, err := agent.New(cfg)
	if err != nil {
		return nil, err
	}
	if err := a.SetBinding(b); err != nil {
		return nil, err
	}
	if err := a.AppendUser(ctx, req.TaskDescription); err != nil {
		return nil, err
	}
	return a, nil
}

// startBinding 是单绑定（r0 形态；CacheBucket = AgentID 的 Patch 1 契约）。
func startBinding(id types.AgentID) types.Binding {
	return types.Binding{
		RungID: "r0", RungIndex: 0,
		Endpoint: "deepseek-main", Model: "deepseek-flash",
		CacheBucket: id, Wire: types.WireOpenAIChat,
		Thinking:    types.ThinkingSpec{Level: "off"},
		BoundAt:     time.Now(),
		CachePrefix: wire.ModelCachePrefix("deepseek-flash", "deepseek-main"),
	}
}

// startLine 是 LLMExecutor 的直连实现（CanonicalRequest → Normalizer →
// Adapter → Denormalize；与 cmd/mini 的 directLine 同形——各命令私有的
// 既有取舍）。
type startLine struct {
	norm    *wire.OpenAICompatNormalizer
	denorm  *wire.OpenAICompatDenormalizer
	adapter *wire.DeepSeekChatAdapter
	binding types.Binding
}

func (l *startLine) ExecuteTurn(ctx context.Context, req *wire.CanonicalRequest) (*wire.WireTurn, error) {
	wr, _, err := l.norm.BuildRequest(req, l.binding)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := l.norm.Assert(wr); err != nil {
		return nil, fmt.Errorf("assert: %w", err)
	}
	resp, err := l.adapter.Execute(ctx, wr, l.binding)
	if err != nil {
		return nil, err
	}
	return l.denorm.Denormalize(resp)
}

// startCatalog 是 ledger 的最小静态目录（单模型计价；DeepSeek 官方价随
// 时间变化——报表的绝对金额是相对比较口径，ADR-0029 的单一来源原则下
// 正式部署从 ladder.yaml 装载）。
func startCatalog() *ladder.StaticCatalog {
	cfg := &ladder.Config{
		Pricing: map[string]wire.Pricing{
			"deepseek-flash": {InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"},
		},
		Ladder: &types.Ladder{
			Rungs: []types.Rung{{
				ID: "r0", Endpoint: "deepseek-main", Model: "deepseek-flash",
				CostPerMTok: 1.5, Currency: "CNY",
			}},
			Start: "r0",
		},
	}
	cat := ladder.NewStaticCatalog(cfg.Ladder)
	if err := cat.AddModel(wire.ModelEntry{
		ID: "deepseek-flash", Provider: "deepseek", Wire: types.WireOpenAIChat, RemoteName: "deepseek-flash",
		Caps: wire.ModelCaps{
			Has:        []types.Capability{types.CapToolCall, types.CapJSONMode, types.CapThinking},
			MaxContext: 65536, MaxOutput: 8192,
			CacheMode:       wire.CacheImplicitPrefix,
			ThinkingControl: wire.ThinkControlLevel,
			ThinkingLevels:  []string{"none", "low", "high", "max"},
		},
	}); err != nil {
		panic(fmt.Sprintf("marl start: catalog: %v", err))
	}
	// 计价的注册面（StaticCatalog 的 Pricing 查找读的是这一张表——
	// ModelEntry.Pricing 字段不进它；ADR-0029 的单一来源）。
	if err := cat.AddPricing("deepseek-flash",
		wire.Pricing{InPerMTok: 1.0, CachedInPerMTok: 0.25, OutPerMTok: 2.0, ReasoningPerMTok: 2.0, Currency: "CNY"}); err != nil {
		panic(fmt.Sprintf("marl start: pricing: %v", err))
	}
	if err := cat.AddEndpoint(wire.EndpointConfig{
		Name: "deepseek-main", BaseURL: "https://api.deepseek.com/v1",
		KeyRef: "env:DEEPSEEK_API_KEY", MaxInflight: 4, RPM: 60,
	}); err != nil {
		panic(fmt.Sprintf("marl start: endpoint: %v", err))
	}
	return cat
}

// orchRules 是 attached 运行的默认规则表（llm_call 维度缺省由 LLMLimits
// 承担——见 #5 的"limits 即缺省政策"；编排分级 + shell 白名单在此；
// grants/ 落盘承接 always）。
func orchRules() []gate.Rule {
	return []gate.Rule{
		{ID: "allow-low-destruction", Match: map[string]string{"kind": "orchestration", "cache_destroyed_pct": "<10"}, Action: gate.ActionAllow},
		{ID: "review-destructive", Match: map[string]string{"kind": "orchestration"}, Action: gate.ActionNeedHuman, Reason: "高破坏编排需人审阅上下文操作"},
		// shell 白名单：构建/测试工具链放行（Agent 自验证编译的前提），
		// 其余命令问人——fail-closed 的白名单，不是黑名单。
		{ID: "allow-go-toolchain", Match: map[string]string{"kind": "shell", "command_prefix": "go"}, Action: gate.ActionAllow},
		{ID: "review-shell", Match: map[string]string{"kind": "shell"}, Action: gate.ActionNeedHuman, Reason: "命令不在白名单（默认只放行 go 工具链）——需要人类审批"},
	}
}

// frontmatterStripped 剥掉 frontmatter（人类收件箱的正文显示形态）。
func frontmatterStripped(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
		return strings.TrimSpace(s[4+i+5:])
	}
	return s
}

// ---- 包内共享小工具 ----

// controlRootOf 是控制面根的项目推导（Part 11.5/14.3 的路径约定；
// <project-id> 取目录名——多项目共存的最小形态）。
func controlRootOf(root string) string {
	base := filepath.Base(root)
	if base == "/" || base == "." || base == "" {
		base = "default"
	}
	return filepath.Join(stateHome(), "marl", base)
}

// stateHome 是 XDG_STATE_HOME 的收口（缺省 ~/.local/state）。
func stateHome() string {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return s
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// osUID 取 OS 用户标识（From 的推导源——能写控制面文件的进程就是同
// UID 的人类侧入口，Part 14.5；$UID 缺失时用 $USER 兜底）。
func osUID() string {
	if u := os.Getenv("UID"); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// actorIDOf 解析 -to（空 = 项目 Agent 的约定 id）。
// [推断: 约定 id 的解析在 daemon 阶段接入进程表查询；当前形态下未知 id
// 的信封由收件箱泵的 SendTo 报错可见。]
func actorIDOf(s string) actor.ActorID {
	if s == "" {
		s = "project"
	}
	return actor.ActorID(s)
}
