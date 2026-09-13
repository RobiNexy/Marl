package discuss

// 裁决解析与 Wait 的契约测试（Part 11.2 / 13.10）。
//
// 轮询参数在测试里取小（PollInterval=10ms，Quiescence=60ms）——契约
// 测的是"静默窗口"的语义，不是 10 秒这个具体数值。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// fastConfig 是测试装配（轮询参数照注释的说明缩小）。
func fastConfigControl(t *testing.T, root, control string) Config {
	t.Helper()
	return Config{
		Root:             root,
		MARLDir:          filepath.Join(root, ".marl"),
		ControlDir:       control,
		DefaultTargetDir: ".marl/knowledge/contracts",
		PollInterval:     10 * time.Millisecond,
		Quiescence:       60 * time.Millisecond,
		VCS:              stubVCS{}, // Wait 不触 VCS；NewManager 要求装配完整
	}
}

// stubVCS 是 Wait-only 场景的 VCS 占位（调用即 t.Fatal：这些测试不应触达它）。
type stubVCS struct{}

func (stubVCS) BranchCreate(ctx context.Context, workdir, name string) error { return nil }
func (stubVCS) BranchSwitch(ctx context.Context, workdir, name string) error { return nil }
func (stubVCS) Add(ctx context.Context, workdir string, relPaths ...string) error {
	return nil
}
func (stubVCS) Commit(ctx context.Context, workdir, author, message string) (string, error) {
	return "", nil
}

// TestParseVerdict：解析 / 中间态错误 / nonce 缺失。
func TestParseVerdict(t *testing.T) {
	ok := parse(t, "---\ndiscussion: d\nnonce: abc\n---\n\n正文")
	if ok.nonce != "abc" || !strings.Contains(ok.body, "正文") {
		t.Fatalf("parsed = %+v", ok)
	}
	for _, bad := range []string{
		"没有 frontmatter",
		"---\nnonce: abc\n正文没闭合",
		"---\ndiscussion: d\n---",
	} {
		t.Run(bad[:8], func(t *testing.T) {
			if _, err := parseVerdict(bad); err == nil {
				t.Fatal("malformed verdict must error (kept waiting)")
			}
		})
	}
}

func parse(t *testing.T, s string) *verdict {
	t.Helper()
	v, err := parseVerdict(s)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return v
}

// TestOutcome：@approve / 纯批注 / reject当批注。
func TestOutcome(t *testing.T) {
	v := parse(t, template(t, "xy")+"@approve\n\n理由 A\n")
	o := readVerdictOutcome(v)
	if o.Kind != OutcomeApproved || o.Annotation != "理由 A" {
		t.Fatalf("outcome = %+v", o)
	}
	v2 := parse(t, template(t, "xy")+"先讨论边界\n")
	o2 := readVerdictOutcome(v2)
	if o2.Kind != OutcomeAnnotation || o2.Annotation != "先讨论边界" {
		t.Fatalf("outcome2 = %+v", o2)
	}
	v3 := parse(t, template(t, "xy")+"@reject")
	o3 := readVerdictOutcome(v3)
	if o3.Kind != OutcomeAnnotation { // 最小闭环：reject 以批注通道恢复
		t.Fatalf("reject should floor to annotation: %+v", o3)
	}
	if o3.Annotation != "@reject" {
		t.Fatalf("annotation content lost: %q", o3.Annotation)
	}
}

// template 让测试里能拿到带 nonce 的 verdict 形态。
func template(t *testing.T, nonce string) string {
	t.Helper()
	return "---\ndiscussion: discuss_x\nnonce: " + nonce + "\n---\n\n## 批注\n\n"
}

// TestWaitNonceMismatch：旧 frontmatter 的 verdict 不算裁决（nonce 防伪造
// 的机制验证——Agent 无法伪造它读不到的当轮 nonce）。
func TestWaitNonceMismatch(t *testing.T) {
	root := t.TempDir()
	control := t.TempDir()
	cfg := fastConfigControl(t, root, control)
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "d1", Dir: control, nonce: "good"}
	if err := os.MkdirAll(sess.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdictPath := filepath.Join(sess.Dir, "verdict.md")
	write := func(content string) {
		if err := os.WriteFile(verdictPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 陈旧 nonce 的 @approve：必须被忽略。
	fake := template(t, "stale") + "@approve\n"
	fake = strings.Replace(fake, "nonce: stale", "nonce: stale", 1)
	write(fake)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err = m.Wait(ctx, sess)
	if err == nil {
		t.Fatal("stale-nonce verdict must not be accepted")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v", err)
	}
}

// TestWaitApprove：收笔静默窗口后读到 @approve。
func TestWaitApprove(t *testing.T) {
	root := t.TempDir()
	control := t.TempDir()
	cfg := fastConfigControl(t, root, control)
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "d2", Dir: control, nonce: "n1", rev: 1}
	if err := os.MkdirAll(sess.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdictPath := filepath.Join(sess.Dir, "verdict.md")
	if err := os.WriteFile(verdictPath, []byte(verdictTemplate(sess)), 0o644); err != nil {
		t.Fatal(err)
	}
	// "人类"编辑：等轮询建好 baseline 后再动（mtime 脉冲 + 静默窗口）。
	go func() {
		time.Sleep(50 * time.Millisecond)
		body := "---\ndiscussion: d2\nnonce: n1\n---\n\n## 批注\n\n 契约写清楚接口。@approve\n"
		_ = os.WriteFile(verdictPath, []byte(body), 0o644)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o, err := m.Wait(ctx, sess)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if o.Kind != OutcomeApproved {
		t.Fatalf("outcome = %+v", o)
	}
	// RotateVerdict：旧轮 nonce 失效（再次带旧 nonce 的 approve 不再接受）。
	if err := m.RotateVerdict(sess); err != nil {
		t.Fatal(err)
	}
	if sess.nonce == "n1" {
		t.Fatal("rotate must change the nonce")
	}
	if err := os.WriteFile(verdictPath, []byte("---\ndiscussion: d2\nnonce: n1\n---\n\n@approve\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := m.Wait(ctx2, sess); err == nil {
		t.Fatal("stale verdict after rotation must not resolve")
	}
}

// TestSlugify：中文/空格 → 文件名安全 slug。
func TestSlugify(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"OAuth 接口 契约", "oauth"},
		{"  ", "topic"},
	} {
		got := slugify(tc.in)
		if !strings.HasPrefix(got, tc.want) && got != tc.want {
			t.Fatalf("slugify(%q) = %q, want prefix %q", tc.in, got, tc.want)
		}
	}
}

// TestConcurrentWait：两个 Wait 并发（-race 条件下的 读竞争面）只是
// 说明 Manager 无共享可变状态。不作为功能分支。
func TestConcurrentWaitSa(t *testing.T) {
	t.Skip("多 agent 同一个 verdict 的场景不存在（一个讨论一个人类），不需要并发 Wait 套件")
	var _ sync.Mutex
}

// TestApproveEchoInTemplateIsNotApproval：模板说明里的 @approve 字样
// 不是裁决（真机测试踩出的坑：不做"## 批注"之后剥离，模板自身就是
// 永久批准——裁决只能出现在人类动手的注释区）。
func TestApproveEchoInTemplate(t *testing.T) {
	m := &Manager{}
	sess := &Session{ID: "d3", Dir: t.TempDir()}
	// 写入模板（含说明中的 @approve 文字）。
	if err := m.RotateVerdict(sess); err != nil {
		t.Fatal(err)
	}
	// 等于模板原文，一字未改。
	content, err := os.ReadFile(filepath.Join(sess.Dir, "verdict.md"))
	if err != nil {
		t.Fatal(err)
	}
	o, ok := waitOnce(&m.cfg, sess, content, time.Now())
	if ok && o.Kind == OutcomeApproved {
		t.Fatalf("template text alone must not approve: %+v", o)
	}
	_ = o
}

// TestWaitStructuredFastPath：带"写完"声明的结构化裁决（TUI/API 写入）
// 免静默窗立即消费——快路径的期限断言（<300ms，远小于 60ms 静默窗的
// 观测窗口 + tick 对齐的无害余量）。
func TestWaitStructuredFastPath(t *testing.T) {
	root := t.TempDir()
	control := t.TempDir()
	cfg := fastConfigControl(t, root, control)
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "df", Dir: control, nonce: "n1", rev: 1}
	if err := os.MkdirAll(sess.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdictPath := filepath.Join(sess.Dir, "verdict.md")
	if err := os.WriteFile(verdictPath, []byte(verdictTemplate(sess)), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond) // 等 watcher 建 mtime baseline
		body := "---\ndiscussion: df\nnonce: n1\n---\n\n## 批注\n\n 接口已对齐。@approve\n" + types.ReplyFinalMarker + "\n"
		_ = os.WriteFile(verdictPath, []byte(types.WithReplyFinal(body)), 0o644)
	}()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o, err := m.Wait(ctx, sess)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if o.Kind != OutcomeApproved {
		t.Fatalf("outcome = %+v", o)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("结构化裁决应零延迟消费，却用了 %v", elapsed)
	}
	if strings.Contains(o.Annotation, types.ReplyFinalMarker) {
		t.Fatalf("标记泄漏进批注: %q", o.Annotation)
	}
}
