package discuss

// Manager 全流程的真机测试（真 fossil 二进制；Part 11.2 流程 1–6 的
// 每一步都打。交付判据：fossil timeline 上有 author=human 的最终 commit）。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"marl/internal/fossil"
)

// setupFossilRoot 建一个真 fossil 项目（init + open + 骨架 + 首提交）。
func setupFossilRoot(t *testing.T) (root, control string, cli *fossil.CLI) {
	t.Helper()
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil binary not available")
	}
	root = t.TempDir()
	control = t.TempDir()
	cli, err := fossil.NewCLI("")
	if err != nil {
		t.Skipf("fossil CLI: %v", err)
	}
	repo := filepath.Join(root, ".marl")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := cli.InitRepo(ctx, filepath.Join(repo, "project.fossil"), "tester"); err != nil {
		t.Fatal(err)
	}
	if err := cli.OpenRepo(ctx, filepath.Join(repo, "project.fossil"), root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".marl", "config.yaml"), []byte("config: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cli.Add(ctx, root, ".marl/config.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Commit(ctx, root, fossil.UserSystem, "init"); err != nil {
		t.Fatal(err)
	}
	return root, control, cli
}

// TestFullDiscussionLoop 真机闭环：
//
//	Agent 建草稿 v1 →（人类）批注 → Agent 修订 v2 →（人类）@approve
//	→ Finalize 落地 author=human → trunk 上能看到结论文件。
func TestFullDiscussionLoop(t *testing.T) {
	root, control, cli := setupFossilRoot(t)
	m, err := NewManager(Config{
		Root:             root,
		ControlDir:       control,
		DefaultTargetDir: ".marl/knowledge/contracts",
		VCS:              cli,
		PollInterval:     10 * time.Millisecond,
		Quiescence:       60 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := m.Open(ctx, OpenRequest{
		AgentID: "root",
		Topic:   "OAuth interface contract",
		Draft:   "Provider 接口：Google/GitHub 两份实现。",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".marl", "discussions", sess.ID, "draft.md")); err != nil {
		t.Fatalf("draft on disk: %v", err)
	}
	verdictPath := filepath.Join(sess.Dir, "verdict.md")
	vb, err := os.ReadFile(verdictPath)
	if err != nil || !strings.Contains(string(vb), "nonce: ") {
		t.Fatalf("verdict template missing: %v", err)
	}
	// 人类批注（无裁决命令）→ Wait 返回 Annotation。
	if err := os.WriteFile(verdictPath, []byte(humanEdit(string(vb), "接口命名为 Provider，加一个 Close 方法。")), 0o644); err != nil {
		t.Fatal(err)
	}
	o1, err := m.Wait(ctx, sess)
	if err != nil {
		t.Fatalf("wait1: %v", err)
	}
	if o1.Kind != OutcomeAnnotation {
		t.Fatalf("outcome1 = %+v", o1)
	}
	// Agent 修订草稿（v2）+ verdict 换轮（旧 nonce 失效）。
	if err := m.UpdateDraft(ctx, sess, "Provider 接口 v2：Google/GitHub + Provider.Close()。"); err != nil {
		t.Fatal(err)
	}
	if err := m.RotateVerdict(sess); err != nil {
		t.Fatal(err)
	}
	vb2, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verdictPath, []byte(humanEdit(string(vb2), "同意；采纳 v2。@approve")), 0o644); err != nil {
		t.Fatal(err)
	}
	o2, err := m.Wait(ctx, sess)
	if err != nil {
		t.Fatalf("wait2: %v", err)
	}
	if o2.Kind != OutcomeApproved {
		t.Fatalf("outcome2 = %+v", o2)
	}
	if _, err := m.Finalize(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// 结论在 trunk 的 contracts/ 目录（topic slug 文件名）。
	contracts := filepath.Join(root, ".marl", "knowledge", "contracts")
	entries, _ := os.ReadDir(contracts)
	var found string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "oauth") {
			found = filepath.Join(contracts, e.Name())
		}
	}
	if found == "" {
		t.Fatal("conclusion file not landed")
	}
	b, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Provider.Close()") {
		t.Fatalf("final draft content not landed: %q", b)
	}
	// timeline：author=human 的 Finalize commit + author=agent 的草稿 commit。
	tl, err := cli.Timeline(ctx, filepath.Join(root, ".marl", "project.fossil"), 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawHuman, sawAgent bool
	for _, e := range tl {
		if strings.Contains(e.Comment, "Finalize") {
			sawHuman = strings.Contains(e.Author, "human")
		}
		if strings.Contains(e.Comment, "讨论草稿") {
			sawAgent = strings.Contains(e.Author, "agent")
		}
		if sawHuman && sawAgent {
			break
		}
	}
	if !sawHuman {
		t.Fatalf("finalize commit not from author=human: %+v", tl)
	}
	if !sawAgent {
		t.Fatalf("draft revisions not from author=agent: %+v", tl)
	}
}

// humanEdit 是测试的"人类编辑"动作：保持 frontmatter（含 nonce）不动、
// 正文换成 body。模拟的是人打开 verdict 编辑器收笔后的文件形态。
func humanEdit(content, body string) string {
	if i := strings.Index(content, "## 批注"); i >= 0 {
		return content[:i+len("## 批注")] + "\n\n" + body + "\n"
	}
	return content + "\n" + body + "\n"
}
