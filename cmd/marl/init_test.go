package main

// marl init 的测试（13.8 交付判据）：临时目录 init → fossil timeline 有
// author=system 的首次 commit → 骨架文件全部入库 → 二次 init 拒绝。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"marl/internal/fossil"
)

func needFossil(t *testing.T) *fossil.CLI {
	t.Helper()
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not in PATH")
	}
	c, err := fossil.NewCLI("")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInitEndToEnd(t *testing.T) {
	ctx := context.Background()
	c := needFossil(t)
	dir := t.TempDir()

	if err := run(dir); err != nil {
		t.Fatalf("init: %v", err)
	}

	// timeline 有 author=system 的首次 commit。
	entries, err := c.Timeline(ctx, filepath.Join(dir, ".marl", "project.fossil"), 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Author == fossil.UserSystem && strings.Contains(e.Comment, "marl init") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no system init commit: %+v", entries)
	}

	// 骨架文件全部入库（fossil ls）。
	res, err := c.RunLS(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".marl/config.yaml",
		".marl/profiles/_default.yaml",
		".marl/prompts/_index.yaml",
		".marl/knowledge/contracts/README.md",
	} {
		if !strings.Contains(res, want) {
			t.Fatalf("skeleton file %q not tracked:\n%s", want, res)
		}
	}

	// 二次 init 拒绝（覆盖历史不是可重试操作）。
	if err := run(dir); err == nil {
		t.Fatal("re-init must fail")
	}
}

func TestInitSkeletonNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	// 用户已定制 config.yaml。
	if err := os.MkdirAll(filepath.Join(dir, ".marl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".marl", "config.yaml"), []byte("custom"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(dir); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".marl", "config.yaml"))
	if err != nil || string(raw) != "custom" {
		t.Fatalf("user config overwritten: %q %v", raw, err)
	}
}
