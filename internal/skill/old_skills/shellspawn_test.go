//go:build ignore

package skill

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func spawnTestCtx(t *testing.T) *ExecContext {
	t.Helper()
	root := t.TempDir()
	return &ExecContext{
		WorkspaceRoot: root,
		SnapshotDir:   filepath.Join(root, ".agents", "snapshots"),
		LogDir:        filepath.Join(root, ".agents", "logs"),
	}
}

// 契约：shell_spawn 后台起进程（不阻塞）、返回 handle、写 meta/log；
// shell_kill 按 handle 杀进程组；观察者写退出事件。
func TestShellSpawnAndKill(t *testing.T) {
	if os.Getenv("SKIP_PROC") != "" {
		t.Skip("process tests disabled")
	}
	r := NewRegistry()
	ec := spawnTestCtx(t)
	events := make(chan map[string]any, 8)
	ec.ProcessEvent = func(ev map[string]any) { events <- ev }

	res := mustExec(t, r, ec, "shell_spawn", map[string]any{
		"command": "sleep 30",
	})
	if res["ok"] != true {
		t.Fatalf("shell_spawn failed: %v", res)
	}
	handle, _ := res["handle"].(string)
	if handle == "" {
		t.Fatal("no handle returned")
	}
	// meta 文件已写
	if _, err := os.Stat(filepath.Join(ec.LogDir, "procs", handle+".json")); err != nil {
		t.Errorf("meta file missing: %v", err)
	}

	kill := mustExec(t, r, ec, "shell_kill", map[string]any{"handle": handle})
	if kill["ok"] != true {
		t.Fatalf("shell_kill failed: %v", kill)
	}

	// 观察者应推送 process_exit 事件
	select {
	case ev := <-events:
		if ev["event"] != "process_exit" || ev["handle"] != handle {
			t.Errorf("event = %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no process_exit event within deadline")
	}
	// 退出事件落盘
	if _, err := os.Stat(filepath.Join(ec.LogDir, "procs", handle+".exit.json")); err != nil {
		t.Errorf("exit event file missing: %v", err)
	}
}

// 契约：shell_spawn 自然退出（短命令）也推事件、exit_code=0。
func TestShellSpawnNaturalExit(t *testing.T) {
	if os.Getenv("SKIP_PROC") != "" {
		t.Skip("process tests disabled")
	}
	r := NewRegistry()
	ec := spawnTestCtx(t)
	events := make(chan map[string]any, 8)
	ec.ProcessEvent = func(ev map[string]any) { events <- ev }

	mustExec(t, r, ec, "shell_spawn", map[string]any{"command": "exit 3"})
	select {
	case ev := <-events:
		if ev["exit_code"] != float64(3) && ev["exit_code"] != 3 {
			t.Errorf("exit_code = %v (%T), want 3", ev["exit_code"], ev["exit_code"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit event for natural exit")
	}
}

// 契约：shell_spawn 无 command 报 BAD_ARGS；禁词 sudo 拦截。
func TestShellSpawnValidation(t *testing.T) {
	r := NewRegistry()
	ec := spawnTestCtx(t)

	res := mustExec(t, r, ec, "shell_spawn", map[string]any{})
	if res["ok"] != false || res["error_type"] != "BAD_ARGS" {
		t.Errorf("empty command: %v", res)
	}
	res = mustExec(t, r, ec, "shell_spawn", map[string]any{"command": "sudo rm -rf /"})
	if res["ok"] != false || res["error_type"] != "FORBIDDEN_COMMAND" {
		t.Errorf("sudo: %v", res)
	}
}
