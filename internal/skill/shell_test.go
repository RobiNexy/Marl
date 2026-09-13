package skill

// shell_exec 的契约测试（阶段 13 的移植验收）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

func shellEnvForTest(t *testing.T) *SkillEnv {
	t.Helper()
	env, _ := newTestEnv(t)
	return env
}

func TestShellExecBasic(t *testing.T) {
	env := shellEnvForTest(t)
	res, err := ShellExec.Execute(context.Background(), map[string]any{
		"command": "echo hello-shell",
	}, env)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.OK || res.Data["stdout"] != "hello-shell" {
		t.Fatalf("res: %+v", res)
	}
}

func TestShellExecNonZeroExitIsBusinessFailure(t *testing.T) {
	env := shellEnvForTest(t)
	res, err := ShellExec.Execute(context.Background(), map[string]any{
		"command": "exit 3",
	}, env)
	if err != nil {
		t.Fatalf("非零退出不是技能失败: %v", err)
	}
	if res.OK || res.Data["exit_code"] != 3 {
		t.Fatalf("res: %+v", res)
	}
}

func TestShellExecForbidden(t *testing.T) {
	env := shellEnvForTest(t)
	for _, cmd := range []string{"sudo rm -rf /", "su nobody", "echo ok && doas id"} {
		res, err := ShellExec.Execute(context.Background(), map[string]any{"command": cmd}, env)
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if res.ErrorType != "FORBIDDEN_COMMAND" {
			t.Fatalf("%q: %+v", cmd, res)
		}
	}
}

func TestShellExecTimeout(t *testing.T) {
	env := shellEnvForTest(t)
	start := time.Now()
	res, err := ShellExec.Execute(context.Background(), map[string]any{
		"command": "sleep 5", "timeout_ms": 300,
	}, env)
	if err != nil {
		t.Fatalf("timeout: %v", err)
	}
	if res.OK || res.Data["timed_out"] != true {
		t.Fatalf("res: %+v", res)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("超时未生效（耗时 %v）", time.Since(start))
	}
}

func TestShellExecOutputTruncation(t *testing.T) {
	env := shellEnvForTest(t)
	res, err := ShellExec.Execute(context.Background(), map[string]any{
		"command": "seq 1 500", "output_mode": "tail",
	}, env)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Data["stdout"].(string)
	if len(out) > 2000 || !strings.Contains(out, "500") {
		t.Fatalf("tail 截断异常: %d bytes", len(out))
	}
}
