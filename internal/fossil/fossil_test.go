package fossil

// fossil 包测试：真实 fossil 二进制驱动的生命周期与提交流程。
//
// 这些测试需要 fossil 在 PATH（开发环境有；CI 无则跳过——fossil 集成的
// 全部价值在真机验证，fake 一个 CLI 没有意义）。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needFossil(t *testing.T) *CLI {
	t.Helper()
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not in PATH")
	}
	c, err := NewCLI("")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newRepo 建一套 仓库+checkout 的最小环境（InitRepo + OpenRepo + 首次提交）。
func newRepo(t *testing.T, c *CLI) (repo, workdir string) {
	t.Helper()
	base := t.TempDir()
	repo = filepath.Join(base, "test.fossil")
	workdir = filepath.Join(base, "ws")
	if err := c.InitRepo(context.Background(), repo, "marl-test"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	if err := c.OpenRepo(context.Background(), repo, workdir); err != nil {
		t.Fatalf("OpenRepo: %v", err)
	}
	return repo, workdir
}

func writeWS(t *testing.T, workdir, rel, content string) {
	t.Helper()
	abs := filepath.Join(workdir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInitAndOpen(t *testing.T) {
	ctx := context.Background()
	c := needFossil(t)
	repo, workdir := newRepo(t, c)

	// 幂等 open。
	if err := c.OpenRepo(ctx, repo, workdir); err != nil {
		t.Fatalf("re-open must be idempotent: %v", err)
	}
	// ignore-glob 已写。
	raw, err := os.ReadFile(filepath.Join(workdir, ".fossil-settings", "ignore-glob"))
	if err != nil || !strings.Contains(string(raw), ".marl/*.db") {
		t.Fatalf("ignore-glob: %v %q", err, raw)
	}
	// 重复 init 必须拒绝（防覆盖历史）。
	if err := c.InitRepo(ctx, repo, "marl-test"); err == nil {
		t.Fatal("re-init must fail")
	}
}

func TestCommitAuthorAndTimeline(t *testing.T) {
	ctx := context.Background()
	c := needFossil(t)
	repo, workdir := newRepo(t, c)

	writeWS(t, workdir, "src/main.go", "package main\n")
	if err := c.Add(ctx, workdir, "src/main.go"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	hash, err := c.Commit(ctx, workdir, UserAgent, "agent 的单写者提交")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if hash == "" {
		t.Fatal("commit must return the new version hash")
	}

	entries, err := c.Timeline(ctx, repo, 5)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("timeline empty")
	}
	// 最新一条是 agent 的提交，author 正确（Part 8.4：author 按调用路径）。
	top := entries[0]
	if top.Author != UserAgent {
		t.Fatalf("author = %q, want %q", top.Author, UserAgent)
	}
	if top.Comment != "agent 的单写者提交" || top.Hash == "" {
		t.Fatalf("entry: %+v", top)
	}
	// 人类提交。
	writeWS(t, workdir, "docs/note.md", "human note\n")
	if err := c.Add(ctx, workdir, "docs/note.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(ctx, workdir, UserHuman, "人类提交"); err != nil {
		t.Fatal(err)
	}
	entries, _ = c.Timeline(ctx, repo, 5)
	if entries[0].Author != UserHuman {
		t.Fatalf("author = %q, want human", entries[0].Author)
	}
}

func TestCommitValidation(t *testing.T) {
	ctx := context.Background()
	c := needFossil(t)
	_, workdir := newRepo(t, c)

	t.Run("non-framework author rejected", func(t *testing.T) {
		if _, err := c.Commit(ctx, workdir, "agent:root", "x"); err == nil {
			t.Fatal("part 8.4 的 user 形态是三个框架用户，不是 user:role 字符串")
		}
	})
	t.Run("nothing to commit is a sentinel", func(t *testing.T) {
		if _, err := c.Commit(ctx, workdir, UserAgent, "empty"); err != nil {
			if !strings.Contains(err.Error(), ErrNothingToCommit.Error()) {
				t.Fatalf("err = %v, want ErrNothingToCommit", err)
			}
		}
	})
	t.Run("outside checkout is ErrNotOpen", func(t *testing.T) {
		outside := t.TempDir()
		if _, err := c.Commit(ctx, outside, UserAgent, "x"); err == nil ||
			!strings.Contains(err.Error(), ErrNotOpen.Error()) {
			t.Fatalf("err = %v, want ErrNotOpen", err)
		}
	})
}

func TestStatusAndDiff(t *testing.T) {
	ctx := context.Background()
	c := needFossil(t)
	repo, workdir := newRepo(t, c)

	// 初始提交（建立基线）。
	writeWS(t, workdir, "base.txt", "v1\n")
	if err := c.Add(ctx, workdir, "base.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(ctx, workdir, UserSystem, "baseline"); err != nil {
		t.Fatal(err)
	}

	// 改动 + 新文件。
	writeWS(t, workdir, "base.txt", "v2\n")
	writeWS(t, workdir, "new.txt", "new\n")
	changes, err := c.Status(ctx, workdir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	kinds := map[string]string{}
	for _, ch := range changes {
		kinds[ch.Path] = ch.Kind
	}
	if kinds["base.txt"] != "EDITED" {
		t.Fatalf("base.txt: %+v", changes)
	}
	// 新文件在 add 之前是 EXTRA（fossil changes 不列未跟踪文件——实测）。
	if kinds["new.txt"] != "EXTRA" {
		t.Fatalf("new.txt: %+v (want EXTRA)", changes)
	}
	// diff 可见已跟踪文件的改动（未跟踪文件不在 diff 里——add 之后才进）。
	diff, err := c.Diff(ctx, workdir)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "+v2") {
		t.Fatalf("diff: %q", diff)
	}
	_ = repo
}

func TestIgnoreGlobEffective(t *testing.T) {
	ctx := context.Background()
	c := needFossil(t)
	repo, workdir := newRepo(t, c)

	// .marl/ 下的文件被 ignore（SKIP，不进 changes/add）。
	writeWS(t, workdir, ".marl/supervisor.db", "fake db")
	writeWS(t, workdir, "src/code.go", "package x\n")
	changes, err := c.Status(ctx, workdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range changes {
		if strings.HasPrefix(ch.Path, ".marl/") {
			t.Fatalf("ignored path leaked into changes: %+v", ch)
		}
	}
	// add .marl 路径 → SKIP（不报错也不入库）。
	if err := c.Add(ctx, workdir, ".marl/supervisor.db"); err != nil {
		t.Fatalf("add ignored path must be a silent skip: %v", err)
	}
	if err := c.Add(ctx, workdir, "src/code.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(ctx, workdir, UserSystem, "with ignore"); err != nil {
		t.Fatal(err)
	}
	entries, _ := c.Timeline(ctx, repo, 3)
	if len(entries) == 0 {
		t.Fatal("timeline empty")
	}
}

// TestConcurrentCommits 是 fossil 写互斥的实测记录（doc.go 的依据）：
// 两个 goroutine 并发 add+commit，writeMu 串行化后都成功。fossil 的 add 是
// **checkout 级暂存区**（不是 per-goroutine 的）——先 add 的文件会被先跑的
// commit 一并带走，后跑的 commit 可能 nothing-to-commit（ErrNothingToCommit
// 哨兵的正确消费场景）。本测试钉死这个语义：串行化不炸、文件最终都入库。
func TestConcurrentCommits(t *testing.T) {
	ctx := context.Background()
	c := needFossil(t)
	repo, workdir := newRepo(t, c)

	writeWS(t, workdir, "a.txt", "a\n")
	if err := c.Add(ctx, workdir, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(ctx, workdir, UserSystem, "baseline"); err != nil {
		t.Fatal(err)
	}
	// 两个 goroutine 各自写文件并 add+commit（writeMu 串行化）。
	done := make(chan error, 2)
	for i, user := range []string{UserAgent, UserHuman} {
		go func(idx int, u string) {
			writeWS(t, workdir, fmt.Sprintf("f%d.txt", idx), fmt.Sprintf("content %d\n", idx))
			if err := c.Add(ctx, workdir, fmt.Sprintf("f%d.txt", idx)); err != nil {
				done <- err
				return
			}
			_, err := c.Commit(ctx, workdir, u, fmt.Sprintf("concurrent %d", idx))
			done <- err
		}(i, user)
	}
	commits := 0
	for i := 0; i < 2; i++ {
		err := <-done
		if err == nil {
			commits++
			continue
		}
		// 后跑的 commit 可能空转（暂存区被前一个带走）——这是 fossil add
		// 语义的正确结果，不是故障。
		if !strings.Contains(err.Error(), ErrNothingToCommit.Error()) {
			t.Fatalf("concurrent commit: %v", err)
		}
	}
	if commits < 1 {
		t.Fatal("at least one commit must succeed")
	}
	// 两个文件最终都入库（先跑的 commit 带走了两个暂存）。
	ls, err := c.RunLS(ctx, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ls, "f0.txt") || !strings.Contains(ls, "f1.txt") {
		t.Fatalf("files lost: %q", ls)
	}
	_ = repo
}
