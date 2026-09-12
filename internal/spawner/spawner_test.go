package spawner

// Spawner 测试：裁决闸（深度/权限/上限/子集/扇出/注入）、生命周期与
// 框架代报、命名空间构建、report 机械检查。

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

	"marl/internal/proto"
	"marl/internal/store"
	"marl/internal/types"
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
		MaxDepth:        1, // 阶段 5：只有根能 fork
		MaxActive:       8,
		MaxForkRounds:   3,
		CanSpawnAtDepth: func(depth int) bool { return depth == 0 },
		Log:             lg,
	}, lg
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

// ---------------------------------------------------------------------------
// 裁决闸
// ---------------------------------------------------------------------------

func TestAdjudicateApprove(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, nil)
	mb := spw.Bootstrap("root", parentNS())
	_ = mb

	dec, err := spw.Adjudicate(context.Background(), spawnReq("读 src/auth/oauth.go", "src/auth/oauth/**"))
	if err != nil {
		t.Fatalf("Adjudicate: %v", err)
	}
	if dec.Status != proto.SpawnApproved || dec.ChildAgentID == "" {
		t.Fatalf("decision: %+v", dec)
	}
	// 工厂收到的计划：深度 1、命名空间是子集、注入为空。
	plan := factory.plans[0]
	if plan.Depth != 1 || plan.ParentID != "root" {
		t.Fatalf("plan: %+v", plan)
	}
	if !plan.Namespace.Subset(parentNS()) {
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
		req    func() *proto.SpawnRequest
		want   proto.SpawnErrorCode
	}{
		{"requester not found", nil, func() *proto.SpawnRequest {
			r := spawnReq("x")
			r.RequesterID = "ghost"
			return r
		}, proto.SpawnErrRequesterNotFound},
		{"depth gate (child cannot spawn)", nil, func() *proto.SpawnRequest {
			r := spawnReq("x")
			r.RequesterID = "sub" // 由前一个用例登记的子
			return r
		}, proto.SpawnErrNotPermitted},
		{"namespace exceeded", nil, func() *proto.SpawnRequest {
			return spawnReq("x", "etc/**")
		}, proto.SpawnErrNamespaceExceeded},
		{"empty profile", nil, func() *proto.SpawnRequest {
			r := spawnReq("x", "src/auth/**")
			r.ProfileID = ""
			return r
		}, proto.SpawnErrProfileNotFound},
		{"fork rounds", func(c *Config) { c.MaxForkRounds = 1 }, func() *proto.SpawnRequest {
			return spawnReq("x", "src/auth/**")
		}, proto.SpawnErrForkRounds},
		{"inject out of range", nil, func() *proto.SpawnRequest {
			r := spawnReq("x", "src/auth/**")
			r.InjectMessages = []int64{99}
			return r
		}, proto.SpawnErrInvalidInject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spw, _, lg := newTestSpawner(t, tc.mutate)
			spw.Bootstrap("root", parentNS())
			// 预置 requester log（inject 校验的基准）。
			e := types.NewLogEntry("root", types.RoleUserInput, "seed")
			if _, err := lg.Append(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if tc.name == "depth gate (child cannot spawn)" {
				// 先登记一个子进程（深度 1）。
				if _, err := spw.Adjudicate(context.Background(), spawnReq("x", "src/auth/**")); err != nil {
					t.Fatal(err)
				}
				var childID types.AgentID
				spw.mu.Lock()
				for id := range spw.procs {
					if id != "root" {
						childID = id
					}
				}
				spw.mu.Unlock()
				r := spawnReq("x")
				r.RequesterID = types.AgentID(childID)
				r.WritablePaths = []string{"src/auth/**"}
				dec, err := spw.Adjudicate(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				if dec.Code != tc.want {
					t.Fatalf("code = %s, want %s (reason=%s)", dec.Code, tc.want, dec.Reason)
				}
				return
			}
			if tc.name == "fork rounds" {
				// 先用掉唯一的轮次。
				if _, err := spw.Adjudicate(context.Background(), spawnReq("first", "src/auth/**")); err != nil {
					t.Fatal(err)
				}
			}
			dec, err := spw.Adjudicate(context.Background(), tc.req())
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

func TestAdjudicateMaxDepth(t *testing.T) {
	spw, _, _ := newTestSpawner(t, func(c *Config) { c.MaxDepth = 1 })
	spw.Bootstrap("root", parentNS())
	// MaxDepth=1：根 fork 子 OK（childDepth=1），子 fork 孙被拒（深度闸 +
	// 权限闸都会拦，权限闸先触发——depth 1 不可 spawn）。
	dec, err := spw.Adjudicate(context.Background(), spawnReq("x", "src/auth/**"))
	if err != nil || dec.Status != proto.SpawnApproved {
		t.Fatalf("first fork: %+v %v", dec, err)
	}
}

func TestAdjudicateGlobalLimit(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, func(c *Config) { c.MaxActive = 2 })
	spw.Bootstrap("root", parentNS())
	// root + 1 个子 = 2，第二个 fork 被拒。
	if _, err := spw.Adjudicate(context.Background(), spawnReq("a", "src/auth/**")); err != nil {
		t.Fatal(err)
	}
	factory.lastRunner().mu.Lock()
	close(factory.lastRunner().block) // 子结束（未 report → 框架代报）
	factory.lastRunner().mu.Unlock()
	time.Sleep(50 * time.Millisecond) // 等代报落袋
	dec, err := spw.Adjudicate(context.Background(), spawnReq("b", "src/auth/**"))
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
	spw, factory, _ := newTestSpawner(t, nil)
	mb := spw.Bootstrap("root", parentNS())

	dec, err := spw.Adjudicate(context.Background(), spawnReq("x", "src/auth/**"))
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
	case env := <-mb:
		if env.Type != proto.MsgChildReport || env.From != childID {
			t.Fatalf("envelope: %+v", env)
		}
		r, ok := env.Payload.(*proto.ChildReport)
		if !ok || r.Status != proto.ReportSuccess {
			t.Fatalf("payload: %+v", env.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("report not delivered")
	}
	// 重复 report 被拒（协议 bug 的可见形态）。
	if err := spw.ReportToParent(context.Background(), report); err == nil {
		t.Fatal("duplicate report must fail")
	}
	_ = factory
}

func TestFrameworkReportOnSilentExit(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, nil)
	mb := spw.Bootstrap("root", parentNS())

	dec, err := spw.Adjudicate(context.Background(), spawnReq("x", "src/auth/**"))
	if err != nil {
		t.Fatal(err)
	}
	// 子未 report 直接退出。
	factory.lastRunner().mu.Lock()
	close(factory.lastRunner().block)
	factory.lastRunner().mu.Unlock()
	select {
	case env := <-mb:
		r := env.Payload.(*proto.ChildReport)
		if r.Status != proto.ReportFailed || r.ChildID != dec.ChildAgentID {
			t.Fatalf("framework report: %+v", r)
		}
		if !strings.Contains(r.Report, "框架代报") {
			t.Fatalf("framework report text: %q", r.Report)
		}
	case <-time.After(time.Second):
		t.Fatal("no framework report for silent exit")
	}
}

func TestBuildChildFailureRollsBack(t *testing.T) {
	spw, factory, _ := newTestSpawner(t, nil)
	spw.Bootstrap("root", parentNS())
	factory.mu.Lock()
	factory.err = errors.New("boom")
	factory.mu.Unlock()
	_, err := spw.Adjudicate(context.Background(), spawnReq("x", "src/auth/**"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	// 回滚：进程表无残留、扇出计数还原。
	spw.mu.Lock()
	n := len(spw.procs)
	rounds := spw.procs["root"].ForkRounds
	spw.mu.Unlock()
	if n != 1 || rounds != 0 {
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
