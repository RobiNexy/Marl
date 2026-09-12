//go:build ignore

package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// ── file_edit：锚点匹配契约（NO_MATCH / MULTIPLE_MATCHES / occurrence）──

func TestFileEditNoMatchWithCandidates(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	os.WriteFile(filepath.Join(root, "code.go"), []byte("func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n"), 0o644)
	res := mustExec(t, r, ec, "file_edit", map[string]any{
		"path": "code.go", "old_string": "return 3", "new_string": "return 4",
	})
	if res["ok"] != false || res["error_type"] != "NO_MATCH" {
		t.Fatalf("want NO_MATCH, got %v", res)
	}
	if _, has := res["candidates_snippet"]; !has {
		t.Error("NO_MATCH must carry candidates_snippet (token economy)")
	}
}

func TestFileEditMultipleMatches(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	os.WriteFile(filepath.Join(root, "dup.go"), []byte("x := 1\nx := 2\nx := 3\n"), 0o644)
	res := mustExec(t, r, ec, "file_edit", map[string]any{
		"path": "dup.go", "old_string": "x := ", "new_string": "y := ",
	})
	if res["ok"] != false || res["error_type"] != "MULTIPLE_MATCHES" {
		t.Fatalf("want MULTIPLE_MATCHES, got %v", res)
	}
	locs, ok := res["locations"].([]int)
	if !ok || len(locs) != 3 {
		t.Errorf("locations = %v, want 3 line numbers", res["locations"])
	}
	// occurrence=2 定位第二处（LLM 看过 locations 后的明确选择）
	res = mustExec(t, r, ec, "file_edit", map[string]any{
		"path": "dup.go", "old_string": "x := ", "new_string": "y := ", "occurrence": float64(2),
	})
	if res["ok"] != true {
		t.Fatalf("occurrence=2 failed: %v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "dup.go"))
	if string(data) != "x := 1\ny := 2\nx := 3\n" {
		t.Errorf("occurrence edit wrong: %q", data)
	}
}

func TestFileEditCRLFPreserved(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	p := filepath.Join(root, "win.txt")
	os.WriteFile(p, []byte("line1\r\nline2\r\n"), 0o644)
	res := mustExec(t, r, ec, "file_edit", map[string]any{
		"path": "win.txt", "old_string": "line2", "new_string": "LINE2",
	})
	if res["ok"] != true {
		t.Fatalf("edit failed: %v", res)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "line1\r\nLINE2\r\n" {
		t.Errorf("CRLF style not preserved: %q", data)
	}
}

// ── 快照恢复契约：undo 且 undo 可再 undo ────────────────────────

func TestSnapshotRestoreRoundtrip(t *testing.T) {
	ec, root := newTestEC(t, nil)
	r := reg()
	p := filepath.Join(root, "v.txt")
	os.WriteFile(p, []byte("v1"), 0o644)

	mustExec(t, r, ec, "file_write", map[string]any{"path": "v.txt", "content": "v2"})
	if data, _ := os.ReadFile(p); string(data) != "v2" {
		t.Fatalf("write failed: %q", data)
	}
	res := mustExec(t, r, ec, "restore_snapshot", map[string]any{"path": "v.txt"})
	if res["ok"] != true || res["restored"] != true {
		t.Fatalf("restore failed: %v", res)
	}
	if data, _ := os.ReadFile(p); string(data) != "v1" {
		t.Errorf("restore wrong content: %q", data)
	}
	// 恢复前当前内容（v2）也被快照 → 可再撤销回 v2
	res = mustExec(t, r, ec, "restore_snapshot", map[string]any{"path": "v.txt"})
	if res["ok"] != true {
		t.Fatalf("second restore failed: %v", res)
	}
	if data, _ := os.ReadFile(p); string(data) != "v2" {
		t.Errorf("double-undo broken: %q", data)
	}
}

// ── shell_exec：超时击杀、禁 sudo ───────────────────────────────

func TestShellExecBasic(t *testing.T) {
	ec, _ := newTestEC(t, nil)
	r := reg()
	res := mustExec(t, r, ec, "shell_exec", map[string]any{"command": "echo hello"})
	if res["ok"] != true || res["exit_code"] != 0 || res["stdout"] != "hello" {
		t.Errorf("basic exec broken: %v", res)
	}
	// 超时：sleep 5 但 200ms 超时
	res = mustExec(t, r, ec, "shell_exec", map[string]any{"command": "sleep 5", "timeout_ms": 200})
	if res["timed_out"] != true {
		t.Errorf("timeout not detected: %v", res)
	}
}

func TestShellExecForbidden(t *testing.T) {
	ec, _ := newTestEC(t, nil)
	r := reg()
	for _, cmd := range []string{"sudo rm x", "sudo -u root ls", "su root -c ls"} {
		res := mustExec(t, r, ec, "shell_exec", map[string]any{"command": cmd})
		if res["ok"] != false || res["error_type"] != "FORBIDDEN_COMMAND" {
			t.Errorf("forbidden %q not blocked: %v", cmd, res)
		}
	}
}
