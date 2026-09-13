package knowledge

// vendor / promote 的真机契约测试（真 fossil；Part 12.7/12.8 的语义）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RobiNexy/Marl/internal/fossil"
)

// setupGlobalRepo 建一个真 fossil 全局库（含一条知识）。
func setupGlobalRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "global.fossil")
	cli, err := fossil.NewCLI("")
	if err != nil {
		t.Skipf("fossil CLI: %v", err)
	}
	ctx := context.Background()
	if err := cli.InitRepo(ctx, repo, "tester"); err != nil {
		t.Skipf("init: %v", err)
	}
	return repo
}

// TestPullThenPromoteRoundtrip：promote 项目文件 → 全局库有条目 → 另一个
// 项目 pull → 内容逐字节一致 + lock 记录 artifact（Part 12.7 的关键语义）。
func TestPullThenPromoteRoundtrip(t *testing.T) {
	cli, err := fossil.NewCLI("")
	if err != nil {
		t.Skipf("fossil: %v", err)
	}
	globalRepo := setupGlobalRepo(t)
	// 项目 A：promote 一份契约知识。
	projA := filepath.Join(t.TempDir(), ".marl")
	if err := os.MkdirAll(projA, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# 契约模板\n\npromote 的语义是：验证过、然后跨项目复用。\n"
	if err := os.MkdirAll(filepath.Join(projA, "contracts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projA, "contracts", "http-handler.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := Promote(context.Background(), cli, projA, globalRepo, "contracts/http-handler.md")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if hash == "" {
		t.Fatal("promote must return commit hash")
	}
	// 项目 B：pull → vendor/ 下内容逐字节一致 + lock 记录。
	projB := filepath.Join(t.TempDir(), ".marl")
	if err := os.MkdirAll(projB, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := Pull(context.Background(), cli, projB, globalRepo)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "contracts/http-handler.md" {
		t.Fatalf("pull entries: %+v", entries)
	}
	if entries[0].Artifact == "" || entries[0].Source != "global" {
		t.Fatalf("lock entry incomplete: %+v", entries[0])
	}
	got, err := os.ReadFile(filepath.Join(projB, "knowledge", "vendor", "contracts", "http-handler.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("vendored content drift:\n%s\n--- want ---\n%s", got, content)
	}
	// lock 文件可再解析（幂等 Pull 的形态：第二次 pull 内容不变）。
	lock, err := LoadVendorLock(projB)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Entries) != 1 {
		t.Fatalf("lock after pull: %+v", lock)
	}
}

// TestPromoteToNonEmptyGlobalRepo：第二次 promote（全局库已非空）必须成功。
//
// 回归面：OpenRepo 始终带 -k（不物化已跟踪文件），temp checkout 里缺文件
// 时 fossil commit 报 "not found: no such file"——修复是 Promote 前先
// Update 物化 tip（真机实测记录：fossil 2.26）。
func TestPromoteToNonEmptyGlobalRepo(t *testing.T) {
	cli, err := fossil.NewCLI("")
	if err != nil {
		t.Skipf("fossil: %v", err)
	}
	global := setupGlobalRepo(t)
	mkProj := func(name, rel, body string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), ".marl")
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	ctx := context.Background()
	// 第一次 promote：全局库空 → 直接入库（既有通过面，不许回归）。
	if _, err := Promote(ctx, cli, mkProj("a", "knowledge/contracts/first.md", "first\n"), global, "knowledge/contracts/first.md"); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	// 第二次 promote：全局库非空 → 必须成功（物化修复的验证点）。
	hash, err := Promote(ctx, cli, mkProj("b", "knowledge/contracts/second.md", "second\n"), global, "knowledge/contracts/second.md")
	if err != nil {
		t.Fatalf("second promote (non-empty global): %v", err)
	}
	if hash == "" {
		t.Fatal("second promote must return commit hash")
	}
	// 全局库两条目 → pull 全量登记。
	proj := filepath.Join(t.TempDir(), ".marl")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := Pull(ctx, cli, proj, global)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("pull entries: %+v", entries)
	}
}

// TestPullEmptyGlobal：全局库空 → 显式报错（"全局无知识"是配置问题，
// 不是静默空集）。
func TestPullEmptyGlobal(t *testing.T) {
	cli, _ := fossil.NewCLI("")
	global := setupGlobalRepo(t)
	projB := filepath.Join(t.TempDir(), ".marl")
	if err := os.MkdirAll(projB, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Pull(context.Background(), cli, projB, global)
	if err == nil || !strings.Contains(err.Error(), "no knowledge entries") {
		t.Fatalf("empty global: %v", err)
	}
}
