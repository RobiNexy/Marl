//go:build ignore

package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDirExcludes(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	os.MkdirAll(filepath.Join(root, "node_modules", "deep"), 0o755)
	os.WriteFile(filepath.Join(root, "node_modules", "deep", "x.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644)

	res := mustExec(t, r, ec, "list_dir", map[string]any{"path": "."})
	if res["ok"] != true {
		t.Fatalf("list_dir failed: %v", res)
	}
	if strings.Contains(fmt.Sprint(res["tree"]), "node_modules") {
		t.Errorf("default excludes not applied: %v", res["tree"])
	}
}

func TestFileSearchFilesMode(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	os.MkdirAll(filepath.Join(root, "src"), 0o755)
	os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("package src\n// TODO fix me\nfunc f() {}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "src", "b.go"), []byte("// TODO another\n"), 0o644)
	os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644) // 二进制跳过

	res := mustExec(t, r, ec, "file_search", map[string]any{"pattern": "TODO", "output_mode": "files"})
	if res["ok"] != true || res["total_matches"] != 2 {
		t.Fatalf("files mode: %v", res)
	}
	res = mustExec(t, r, ec, "file_search", map[string]any{
		"pattern": "TODO", "output_mode": "snippets", "context_after": 1,
	})
	if !strings.Contains(fmt.Sprint(res["results"]), "fix me") {
		t.Errorf("snippet content missing: %v", res["results"])
	}
}

func TestFileReadPagination(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	var lines []string
	for i := 1; i <= 500; i++ {
		lines = append(lines, fmt.Sprintf("line-%03d", i))
	}
	os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Join(lines, "\n")), 0o644)

	res := mustExec(t, r, ec, "file_read", map[string]any{"path": "big.txt", "mode": "content", "offset": 490, "limit": 20})
	if res["ok"] != true {
		t.Fatalf("read failed: %v", res)
	}
	if res["total_lines"] != 500 || res["eof"] != true {
		t.Errorf("pagination flags wrong: %v", res)
	}
	if !strings.Contains(fmt.Sprint(res["content"]), "line-490") {
		t.Errorf("offset wrong: %v", res["content"])
	}
	res = mustExec(t, r, ec, "file_read", map[string]any{"path": "big.txt", "mode": "content", "limit": 9999})
	if res["limit"] != 200 {
		t.Errorf("hard limit not enforced: %v", res["limit"])
	}
}

func TestFileReadBinary(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0x89, 'P', 'N', 'G', 0x00, 0x00}, 0o644)
	res := mustExec(t, r, ec, "file_read", map[string]any{"path": "blob.bin"})
	if res["type"] != "binary" {
		t.Errorf("binary detection failed: %v", res)
	}
}
