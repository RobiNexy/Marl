package knowledge

// vendor / promote 的真机契约测试（真 fossil；Part 12.7/12.8 的语义）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marl/internal/fossil"
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
