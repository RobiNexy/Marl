//go:build ignore

package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestEC 构造临时 workspace 的执行环境。
func newTestEC(t *testing.T, writable []string) (*ExecContext, string) {
	t.Helper()
	root := t.TempDir()
	ec := &ExecContext{
		WorkspaceRoot: root,
		SnapshotDir:   filepath.Join(root, ".agents", "snapshots"),
		LogDir:        filepath.Join(root, ".agents", "logs"),
		WritablePaths: writable,
	}
	return ec, root
}

func mustExec(t *testing.T, r *Registry, ec *ExecContext, name string, args map[string]any) map[string]any {
	t.Helper()
	res := r.Execute(context.Background(), name, ec, args)
	if res == nil {
		t.Fatalf("skill %s: nil result", name)
	}
	return res
}

func reg() *Registry { return NewRegistry() }

// ── 沙箱契约 ──────────────────────────────────────────────────────

func TestSandboxEscapeBlocked(t *testing.T) {
	ec, _ := newTestEC(t, nil)
	r := reg()
	// 绝对路径逃逸
	res := mustExec(t, r, ec, "file_read", map[string]any{"path": "/etc/passwd"})
	if res["ok"] != false || res["error_type"] != "PATH_OUTSIDE_WORKSPACE" {
		t.Errorf("absolute escape not blocked: %v", res)
	}
	// 相对路径 .. 逃逸
	res = mustExec(t, r, ec, "file_read", map[string]any{"path": "../../etc/passwd"})
	if res["ok"] != false || res["error_type"] != "PATH_OUTSIDE_WORKSPACE" {
		t.Errorf("dotdot escape not blocked: %v", res)
	}
	// 前缀陷阱：/tmp-xxx 不在 /tmp/xxx 内
	sibling := filepath.Join(filepath.Dir(ec.WorkspaceRoot), filepath.Base(ec.WorkspaceRoot)+"-evil")
	os.WriteFile(sibling, []byte("x"), 0o644)
	t.Cleanup(func() { os.Remove(sibling) })
	res = mustExec(t, r, ec, "file_read", map[string]any{"path": sibling})
	if res["ok"] != false || res["error_type"] != "PATH_OUTSIDE_WORKSPACE" {
		t.Errorf("prefix-trick escape not blocked: %v", res)
	}
}

func TestSandboxSymlinkEscapeBlocked(t *testing.T) {
	ec, root := newTestEC(t, nil)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s3cret"), 0o644)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt"))
	r := reg()
	res := mustExec(t, r, ec, "file_read", map[string]any{"path": "link.txt"})
	if res["ok"] != false || res["error_type"] != "PATH_OUTSIDE_WORKSPACE" {
		t.Errorf("symlink escape not blocked: %v", res)
	}
}

// ── writable_paths 所有权契约（能力约束代替惩罚，硬拦截） ─────────

func TestWritablePathsOwnership(t *testing.T) {
	ec, root := newTestEC(t, []string{"src/**"})
	os.MkdirAll(filepath.Join(root, "src"), 0o755)
	os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("package src"), 0o644)
	os.WriteFile(filepath.Join(root, "README.md"), []byte("readme"), 0o644)
	r := reg()

	res := mustExec(t, r, ec, "file_write", map[string]any{"path": "src/a.go", "content": "package src // edited"})
	if res["ok"] != true {
		t.Errorf("owned path write failed: %v", res)
	}
	res = mustExec(t, r, ec, "file_write", map[string]any{"path": "README.md", "content": "hack"})
	if res["ok"] != false || res["error_type"] != "PATH_NOT_OWNED" {
		t.Errorf("unowned write not blocked: %v", res)
	}
	// 空 writable_paths = 根 Agent 无限制
	ec2, _ := newTestEC(t, nil)
	res = mustExec(t, r, ec2, "file_write", map[string]any{"path": "README.md", "content": "root can"})
	if res["ok"] != true {
		t.Errorf("root agent write failed: %v", res)
	}
}

// ── file_write：幂等写、create_new ───────────────────────────────

func TestFileWriteIdempotent(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	p := filepath.Join(root, "f.txt")
	os.WriteFile(p, []byte("same"), 0o644)
	res := mustExec(t, r, ec, "file_write", map[string]any{"path": "f.txt", "content": "same"})
	if res["changed"] != false || res["bytes_written"] != 0 {
		t.Errorf("same content must not rewrite: %v", res)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp residue: %s", e.Name())
		}
	}
}

func TestFileWriteCreateNew(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old"), 0o644)
	res := mustExec(t, r, ec, "file_write", map[string]any{"path": "exists.txt", "mode": "create_new", "content": "new"})
	if res["ok"] != false || res["error_type"] != "FILE_ALREADY_EXISTS" {
		t.Errorf("create_new overwrite not blocked: %v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "exists.txt"))
	if string(data) != "old" {
		t.Errorf("original content corrupted: %q", data)
	}
}
