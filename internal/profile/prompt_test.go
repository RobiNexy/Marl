package profile

// PromptIndex 的契约测试（Part 6.2 / 6.12）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePromptFixture(t *testing.T) string {
	t.Helper()
	marl := t.TempDir()
	if err := os.MkdirAll(filepath.Join(marl, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	idx := `prompts:
  - id: "go-engineer"
    path: "prompts/go-engineer.md"
    description: "Go 工程师"
  - id: "code-reviewer"
    path: "prompts/code-reviewer.md"
    description: "代码审查"
`
	if err := os.WriteFile(filepath.Join(marl, "prompts", "_index.yaml"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marl, "prompts", "go-engineer.md"), []byte("# 激活\n\ndata-flow modeling\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marl, "prompts", "code-reviewer.md"), []byte("# 审查\ncontract-first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return marl
}

// TestPromptIndexWholeFile：ReadPrompt 返回整个 md 文件的字节（框架不解析，
// Part 6.1）。
func TestPromptIndexWholeFile(t *testing.T) {
	marl := writePromptFixture(t)
	p, err := LoadPromptIndex(marl)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadPrompt("go-engineer")
	if err != nil {
		t.Fatal(err)
	}
	if got != "# 激活\n\ndata-flow modeling\n" {
		t.Fatalf("prompt content mangled: %q", got)
	}
	// List 按 ID 排序。
	ls := p.List()
	if len(ls) != 2 || ls[0].ID != "code-reviewer" || ls[1].ID != "go-engineer" {
		t.Fatalf("list = %+v", ls)
	}
}

// TestPromptIndexUnknown / 越界路径 / 双 id。
func TestPromptIndexValidation(t *testing.T) {
	marl := writePromptFixture(t)
	p, _ := LoadPromptIndex(marl)
	if _, err := p.ReadPrompt("missing"); err == nil {
		t.Fatal("unknown id must error")
	}
	// 越界 path（../）加载期报错。
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, want string }{
		{"traversal", `prompts:
  - id: "x"
    path: "../outside.md"`, "must be relative"},
		{"absolute", `prompts:
  - id: "x"
    path: "/etc/passwd"`, "must be relative"},
		{"missing path", `prompts:
  - id: "x"`, "path is required"},
		{"duplicate id", `prompts:
  - id: "x"
    path: "prompts/a.md"
  - id: "x"
    path: "prompts/b.md"`, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "prompts", "_index.yaml"), []byte(tc.path), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadPromptIndex(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention %q", err, tc.want)
			}
		})
	}
}
