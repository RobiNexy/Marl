package spawner

// Spawner 测试：裁决闸（深度/权限/上限/子集/扇出/注入）、生命周期与
// 框架代报、命名空间构建、report 机械检查；Part 14 的统一 Actor 面
// （人类 requester → 正常裁决 / report 到人类收件箱）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/actor/actortest"
	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// ---------------------------------------------------------------------------
// 测试基建
// ---------------------------------------------------------------------------

// fakeRunner 记录 Run 是否被调用，可阻塞模拟子运行。
type fakeRunner struct {
	mu        sync.Mutex
	block     chan struct{} // 非 nil 时 Run 阻塞直到关闭
	runCalled bool
}

func (f *fakeRunner) Run(ctx context.Context) error {
	f.mu.Lock()
	f.runCalled = true
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return nil
}

func (f *fakeRunner) called() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCalled
}

// fakeFactory 记录计划并返回预设 runner（或错误）。
type fakeFactory struct {
	mu      sync.Mutex
	plans   []*ChildPlan
	runners []*fakeRunner
	err     error
}

func (f *fakeFactory) BuildChild(_ context.Context, plan *ChildPlan, _ *proto.SpawnRequest) (ChildRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	r := &fakeRunner{block: make(chan struct{})}
	f.plans = append(f.plans, plan)
	f.runners = append(f.runners, r)
	return r, nil
}

func (f *fakeFactory) lastRunner() *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runners) == 0 {
		return nil
	}
	return f.runners[len(f.runners)-1]
}

func testConfig(t *testing.T) (Config, *store.SQLiteStore) {
	t.Helper()
	lg, err := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	return Config{
		// Part 14.6 深度语义：max_depth 只约束 AI→AI fork；项目 Agent 是
		// AI 第 1 层（人类 = depth 0 的根）。"只有项目 Agent 能 fork" =
		// depth==1 的谓词。
		MaxDepth:        2,
		MaxActive:       8,
		MaxForkRounds:   3,
		CanSpawnAtDepth: func(depth int) bool { return depth == 1 },
		Log:             lg,
	}, lg
}

// registerHuman 是测试的人类登记（actortest.ScriptedHuman 的 channel
// 后端——框架代报给人类的信封落在这里，断言用）。
func registerHuman(t *testing.T, spw *Spawner) *actortest.ScriptedHuman {
	t.Helper()
	sh := actortest.New("tester")
	if err := spw.RegisterHuman(context.Background(), sh.Actor()); err != nil {
		t.Fatalf("RegisterHuman: %v", err)
	}
	return sh
}

// spawnRoot 以统一路径建"项目 Agent"根（人类 requester → 正常裁决——
// Part 14.6；旧 Bootstrap 特例已删除）。返回根 ID 与计划的镜像 ns
// （human 基准 nil = 请求即授权，读写挂载由请求折算）。
func spawnRoot(t *testing.T, spw *Spawner, task string, writable, readable []string) types.AgentID {
	t.Helper()
	dec, err := spw.Adjudicate(context.Background(), &proto.SpawnRequest{
		RequesterID:     "human:tester",
		ProfileID:       "coder",
		TaskDescription: task,
		WritablePaths:   writable,
		ReadablePaths:   readable,
	})
	if err != nil {
		t.Fatalf("root Adjudicate: %v", err)
	}
	if dec.Status != proto.SpawnApproved {
		t.Fatalf("root spawn rejected: %+v", dec)
	}
	return dec.ChildAgentID
}

// parentSpawnReq 构造"根 Agent 发起"的 fork 请求（rootID 由统一路径取）。
func spawnReqFrom(rootID types.AgentID, task string, writable ...string) *proto.SpawnRequest {
	return &proto.SpawnRequest{
		RequesterID:     rootID,
		ProfileID:       "coder",
		TaskDescription: task,
		WritablePaths:   writable,
	}
}

func newTestSpawner(t *testing.T, mutate func(*Config)) (*Spawner, *fakeFactory, *store.SQLiteStore) {
	t.Helper()
	cfg, lg := testConfig(t)
	factory := &fakeFactory{}
	cfg.Factory = factory
	if mutate != nil {
		mutate(&cfg)
	}
	spw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return spw, factory, lg
}

func parentNS() *types.Namespace {
	return &types.Namespace{AgentID: "root", Mounts: []types.Mount{
		{Pattern: "src/**", Mode: types.PathRead},
		{Pattern: "src/auth/**", Mode: types.PathWrite},
		{Pattern: "out/**", Mode: types.PathWrite},
	}}
}

func spawnReq(task string, writable ...string) *proto.SpawnRequest {
	return &proto.SpawnRequest{
		RequesterID:     "root",
		ProfileID:       "coder",
		TaskDescription: task,
		WritablePaths:   writable,
	}
}

// rootNamespace 的镜像（旧 parentNS 的 spawn 请求形态；human 基准 nil =
// 请求即授权——读挂载来自 ReadablePaths、写挂载来自 WritablePaths）。
func rootSpawnPaths() (writable, readable []string) {
	return []string{"src/auth/**", "out/**"}, []string{"src/**"}
}

// ---------------------------------------------------------------------------
// 裁决闸
// ---------------------------------------------------------------------------

func TestAdjudicateApprove(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, nil)
	registerHuman(t, spw)
	w, r := rootSpawnPaths()
	rootID := spawnRoot(t, spw, "读 src/auth/oauth.go", w, r)

	dec, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "读 src/auth/oauth.go 内文", "src/auth/oauth/**"))
	if err != nil {
		t.Fatalf("Adjudicate: %v", err)
	}
	if dec.Status != proto.SpawnApproved || dec.ChildAgentID == "" {
		t.Fatalf("decision: %+v", dec)
	}
	// 工厂收到的计划：深度 2（human d0 → root d1 → child d2）、
	// 命名空间是根的子集、注入为空。
	plan := factory.plans[len(factory.plans)-1]
	if plan.Depth != 2 || plan.ParentID != rootID {
		t.Fatalf("plan: %+v", plan)
	}
	spw.mu.Lock()
	rootNS := spw.nsOf[rootID]
	spw.mu.Unlock()
	if !plan.Namespace.Subset(rootNS) {
		t.Fatalf("child ns not subset: %+v", plan.Namespace)
	}
	// 子的 write 挂载是请求的路径；父的 write 面对子降级为 read。
	foundWrite := false
	for _, m := range plan.Namespace.Mounts {
		if m.Pattern == "src/auth/oauth/**" && m.Mode == types.PathWrite {
			foundWrite = true
		}
		if m.Pattern == "out/**" && m.Mode == types.PathWrite {
			t.Fatal("parent write must degrade to read for child")
		}
	}
	if !foundWrite {
		t.Fatalf("requested writable missing: %+v", plan.Namespace.Mounts)
	}
	// 子已启动。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !factory.lastRunner().called() {
		time.Sleep(time.Millisecond)
	}
	if !factory.lastRunner().called() {
		t.Fatal("child not started")
	}
}

func TestAdjudicateRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		req    func(rootID types.AgentID) *proto.SpawnRequest
		want   proto.SpawnErrorCode
	}{
		{"requester not found", nil, func(rootID types.AgentID) *proto.SpawnRequest {
			r := spawnReqFrom(rootID, "x")
			r.RequesterID = "ghost"
			return r
		}, proto.SpawnErrRequesterNotFound},
		{"namespace exceeded", nil, func(rootID types.AgentID) *proto.SpawnRequest {
			return spawnReqFrom(rootID, "x", "etc/**")
		}, proto.SpawnErrNamespaceExceeded},
		{"empty profile", nil, func(rootID types.AgentID) *proto.SpawnRequest {
			r := spawnReqFrom(rootID, "x", "src/auth/**")
			r.ProfileID = ""
			return r
		}, proto.SpawnErrProfileNotFound},
		{"fork rounds", func(c *Config) { c.MaxForkRounds = 1 }, func(rootID types.AgentID) *proto.SpawnRequest {
			return spawnReqFrom(rootID, "x", "src/auth/**")
		}, proto.SpawnErrForkRounds},
		{"inject out of range", nil, func(rootID types.AgentID) *proto.SpawnRequest {
			r := spawnReqFrom(rootID, "x", "src/auth/**")
			r.InjectMessages = []int64{99}
			return r
		}, proto.SpawnErrInvalidInject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spw, _, lg := newTestSpawner(t, tc.mutate)
			registerHuman(t, spw)
			w, r := rootSpawnPaths()
			rootID := spawnRoot(t, spw, "root task", w, r)
			// 预置 requester log（inject 校验的基准）。
			e := types.NewLogEntry(rootID, types.RoleUserInput, "seed")
			if _, err := lg.Append(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if tc.name == "fork rounds" {
				// 先用掉唯一的轮次。
				if _, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "first", "src/auth/**")); err != nil {
					t.Fatal(err)
				}
			}
			dec, err := spw.Adjudicate(context.Background(), tc.req(rootID))
			if err != nil {
				t.Fatalf("Adjudicate: %v", err)
			}
			if dec.Status != proto.SpawnRejected || dec.Code != tc.want {
				t.Fatalf("decision: %+v, want %s", dec, tc.want)
			}
			if dec.Reason == "" {
				t.Fatal("rejection requires a readable reason (Part 9.4)")
			}
		})
	}
}

// TestAdjudicateDepthGate：AI 递归到顶被拒（Part 14.6：max_depth 只约束
// AI→AI fork）；人类行不受深度闸（走 caps）。
func TestAdjudicateDepthGate(t *testing.T) {
	spw, _, _ := newTestSpawner(t, func(c *Config) { c.MaxDepth = 2 }) // human d0 → root d1 → child d2 顶格
	registerHuman(t, spw)
	w, r := rootSpawnPaths()
	rootID := spawnRoot(t, spw, "root", w, r)
	dec, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "x", "src/auth/**"))
	if err != nil || dec.Status != proto.SpawnApproved {
		t.Fatalf("first AI fork: %+v %v", dec, err)
	}
}

// TestHumanCapsAuthority：人类的 spawn 权威在 CapSet（被收窄的替身人类
// 被拒——纪律 1 的反例验证：权威不来自"它是人类"这个事实本身）。
func TestHumanCapsAuthority(t *testing.T) {
	spw, _, _ := newTestSpawner(t, nil)
	b := actor.NewChannelBackend(8)
	narrow, err := actor.NewHuman(actor.HumanID("tester"), b, actor.CapSet{Kind: actor.HumanKind}) // CanSpawn=false
	if err != nil {
		t.Fatal(err)
	}
	if err := spw.RegisterHuman(context.Background(), narrow); err != nil {
		t.Fatal(err)
	}
	dec, aerr := spw.Adjudicate(context.Background(), &proto.SpawnRequest{
		RequesterID: "human:tester", ProfileID: "coder", TaskDescription: "x",
	})
	if aerr != nil {
		t.Fatal(aerr)
	}
	if dec.Status != proto.SpawnRejected || dec.Code != proto.SpawnErrNotPermitted {
		t.Fatalf("narrowed human: %+v", dec)
	}
}

func TestAdjudicateGlobalLimit(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, func(c *Config) { c.MaxActive = 3 }) // human + root + 1 子
	registerHuman(t, spw)
	w, r := rootSpawnPaths()
	rootID := spawnRoot(t, spw, "root", w, r)
	// root + 1 个子 = 3，第二个 fork 被拒。
	if _, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "a", "src/auth/**")); err != nil {
		t.Fatal(err)
	}
	factory.lastRunner().mu.Lock()
	close(factory.lastRunner().block) // 子结束（未 report → 框架代报）
	factory.lastRunner().mu.Unlock()
	time.Sleep(50 * time.Millisecond) // 等代报落袋
	dec, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "b", "src/auth/**"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Status != proto.SpawnRejected || dec.Code != proto.SpawnErrGlobalAgentLimit {
		t.Fatalf("decision: %+v", dec)
	}
}

// ---------------------------------------------------------------------------
// 生命周期：report 投递与框架代报
// ---------------------------------------------------------------------------

func TestChildReportDelivery(t *testing.T) {
	spw, _, _ := newTestSpawner(t, nil)
	human := registerHuman(t, spw)
	w, r := rootSpawnPaths()
	rootID := spawnRoot(t, spw, "root", w, r)

	dec, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "x", "src/auth/**"))
	if err != nil {
		t.Fatal(err)
	}
	childID := dec.ChildAgentID
	// 子 report（通过 ReportSink 通道）。
	report := &proto.ChildReport{ChildID: childID, Status: proto.ReportSuccess, Report: "done"}
	if err := spw.ReportToParent(context.Background(), report); err != nil {
		t.Fatalf("ReportToParent: %v", err)
	}
	// 父信箱收到信封，From = 子（原则 4：框架填写）。
	select {
	case env := <-spw.backendOf(t, rootID):
		if env.Type != proto.MsgChildReport || env.From != childID {
			t.Fatalf("envelope: %+v", env)
		}
		rp, ok := env.Payload.(*proto.ChildReport)
		if !ok || rp.Status != proto.ReportSuccess {
			t.Fatalf("payload: %+v", env.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("report not delivered")
	}
	// 重复 report 被拒（协议 bug 的可见形态）。
	if err := spw.ReportToParent(context.Background(), report); err == nil {
		t.Fatal("duplicate report must fail")
	}
	_ = human
}

// TestChildReportToHuman：项目 Agent（根）的 report 投给人类 Actor——
// 文件后端把它编码成人类收件箱里的一条（Part 14.5"MsgChildReport 到
// 人类"；根有父，"无父收 report"的特例消失）。
func TestChildReportToHuman(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, nil)
	human := registerHuman(t, spw)
	w, r := rootSpawnPaths()
	rootID := spawnRoot(t, spw, "root", w, r)

	// 根"退出未 report"→ 框架代报 failed → 人类收件箱。
	factory.lastRunner().mu.Lock()
	close(factory.lastRunner().block)
	factory.lastRunner().mu.Unlock()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("human inbox got no framework report")
		default:
		}
		for _, env := range human.Inbox() {
			if env.Type != proto.MsgChildReport {
				continue
			}
			rp, ok := env.Payload.(*proto.ChildReport)
			if !ok || rp.Status != proto.ReportFailed || rp.ChildID != rootID {
				t.Fatalf("framework report to human: %+v", env.Payload)
			}
			if !strings.Contains(rp.Report, "框架代报") {
				t.Fatalf("framework report text: %q", rp.Report)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFrameworkReportOnSilentExit(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, nil)
	registerHuman(t, spw)
	w, r := rootSpawnPaths()
	rootID := spawnRoot(t, spw, "root", w, r)

	dec, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "x", "src/auth/**"))
	if err != nil {
		t.Fatal(err)
	}
	// 子未 report 直接退出 → 框架代报进根的信箱。
	factory.lastRunner().mu.Lock()
	close(factory.lastRunner().block)
	factory.lastRunner().mu.Unlock()
	select {
	case env := <-spw.backendOf(t, rootID):
		rp := env.Payload.(*proto.ChildReport)
		if rp.Status != proto.ReportFailed || rp.ChildID != dec.ChildAgentID {
			t.Fatalf("framework report: %+v", rp)
		}
		if !strings.Contains(rp.Report, "框架代报") {
			t.Fatalf("framework report text: %q", rp.Report)
		}
	case <-time.After(time.Second):
		t.Fatal("no framework report for silent exit")
	}
}

// backendOf 取进程行的后端读端（断言投递形态；同包测试的观察面）。
func (s *Spawner) backendOf(t *testing.T, id types.AgentID) <-chan proto.Envelope {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[id]
	if p == nil {
		t.Fatalf("%s not registered", id)
	}
	return p.Backend.Receive()
}

func TestBuildChildFailureRollsBack(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, nil)
	registerHuman(t, spw)
	w, r := rootSpawnPaths()
	rootID := spawnRoot(t, spw, "root", w, r)
	factory.mu.Lock()
	factory.err = errors.New("boom")
	factory.mu.Unlock()
	_, err := spw.Adjudicate(context.Background(), spawnReqFrom(rootID, "x", "src/auth/**"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	// 回滚：进程表无残留（人类 + 根）、扇出计数还原。
	spw.mu.Lock()
	n := len(spw.procs)
	rounds := spw.procs[rootID].ForkRounds
	spw.mu.Unlock()
	if n != 2 || rounds != 0 {
		t.Fatalf("rollback incomplete: procs=%d rounds=%d", n, rounds)
	}
}

// ---------------------------------------------------------------------------
// report 机械检查
// ---------------------------------------------------------------------------

func TestReportCheckerTODO(t *testing.T) {
	root := t.TempDir()
	writeFile2(t, root, "src/auth/a.go", "package a\n// TODO(agent): finish\n")
	writeFile2(t, root, "src/auth/b.go", "package b\n")
	chk, err := NewReportChecker(ReportCheckerConfig{Root: root, MaxScanFiles: 10})
	if err != nil {
		t.Fatal(err)
	}
	report := &proto.ChildReport{ChildID: "c1", Status: proto.ReportSuccess, Report: "done"}
	out, err := chk.Check(report, []string{"src/auth/**"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != proto.ReportPartial {
		t.Fatalf("status = %s, want partial (TODO found)", out.Status)
	}
	if len(out.Blockers) == 0 || !strings.Contains(out.Blockers[0], "a.go") {
		t.Fatalf("blockers: %+v", out.Blockers)
	}
	// 原 report 不被修改。
	if report.Status != proto.ReportSuccess {
		t.Fatal("input report mutated")
	}
}

func TestReportCheckerNoChangeClaim(t *testing.T) {
	root := t.TempDir()
	chk, _ := NewReportChecker(ReportCheckerConfig{Root: root, MaxScanFiles: 10})
	t.Run("claims changes with none -> failed", func(t *testing.T) {
		report := &proto.ChildReport{ChildID: "c1", Status: proto.ReportSuccess,
			Report: "我写入了 src/auth/a.go 并修改了配置。"}
		out, err := chk.Check(report, []string{"src/**"})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != proto.ReportFailed {
			t.Fatalf("status = %s, want failed", out.Status)
		}
	})
	t.Run("pure read task passes", func(t *testing.T) {
		report := &proto.ChildReport{ChildID: "c1", Status: proto.ReportSuccess,
			Report: "读取了 src/main.go 的前 10 行：package main 与 import 块。"}
		out, err := chk.Check(report, []string{"src/**"})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != proto.ReportSuccess {
			t.Fatalf("status = %s, want success (read-only task)", out.Status)
		}
	})
	t.Run("negated change claim is not a claim", func(t *testing.T) {
		// 真机实测的教训：'nothing modified' 被动词表误判 → 纯阅读任务
		// 被错降为 failed。否定语境不算声明。
		report := &proto.ChildReport{ChildID: "c1", Status: proto.ReportSuccess,
			Report: "Read src/auth/oauth.go (read-only; nothing modified).\nPackage: auth."}
		out, err := chk.Check(report, []string{"src/**"})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != proto.ReportSuccess {
			t.Fatalf("status = %s, want success (negation is not a claim)", out.Status)
		}
	})
	t.Run("partial is not upgraded", func(t *testing.T) {
		report := &proto.ChildReport{ChildID: "c1", Status: proto.ReportPartial, Report: "half done"}
		out, err := chk.Check(report, []string{"src/**"})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != proto.ReportPartial {
			t.Fatalf("status = %s", out.Status)
		}
	})
	t.Run("invalid status rejected", func(t *testing.T) {
		report := &proto.ChildReport{ChildID: "c1", Status: "", Report: "x"}
		if _, err := chk.Check(report, nil); err == nil {
			t.Fatal("invalid status must be rejected (zero value is not success)")
		}
	})
}

func writeFile2(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 编译期：Spawner 满足 proto.Spawner。
var _ proto.Spawner = (*Spawner)(nil)

func init() { _ = fmt.Sprint }
