package server

// 环境自检（GUI 的诊断页 + CLI 的 marl doctor 共用单一实现）。
//
// 检查项按"缺了它任务必然失败"排序；每项 ✓/✗ + 一行修复提示——
// 自检的价值在提示，不在状态本身。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/RobiNexy/Marl/internal/contract"
)

// CheckResult 是 CheckResult 的别名（契约 DTO 的单一来源在 contract）。
type CheckResult = contract.CheckResult

// RunChecks 对项目目录跑全部检查（root 缺骨架也照跑——前四项环境项
// 与项目无关）。
func RunChecks(root string) []CheckResult {
	marlDir := filepath.Join(root, ".marl")
	out := []CheckResult{}

	out = append(out, func() CheckResult {
		if _, err := exec.LookPath("fossil"); err != nil {
			return CheckResult{Name: "fossil", OK: false, Note: "not found — install from https://fossil-scm.org (2.20+)"}
		}
		out2, _ := exec.Command("fossil", "version").Output()
		return CheckResult{Name: "fossil", OK: true, Note: strings.SplitN(string(out2), "\n", 2)[0]}
	}())

	out = append(out, func() CheckResult {
		if os.Getenv("DEEPSEEK_API_KEY") == "" {
			return CheckResult{Name: "api_key", OK: false, Note: "DEEPSEEK_API_KEY is empty — export it (real runs need it; offline replay does not)"}
		}
		k := os.Getenv("DEEPSEEK_API_KEY")
		if len(k) > 8 {
			k = k[:8] + "…"
		}
		return CheckResult{Name: "api_key", OK: true, Note: "set (" + k + ")"}
	}())

	out = append(out, func() CheckResult {
		if _, err := os.Stat(filepath.Join(marlDir, "project.fossil")); err != nil {
			return CheckResult{Name: "project", OK: false, Note: "not a Marl project (missing project.fossil) — run: marl init " + root}
		}
		for _, sub := range []string{"config.yaml", "profiles", "prompts", "knowledge"} {
			if _, err := os.Stat(filepath.Join(marlDir, sub)); err != nil {
				return CheckResult{Name: "project", OK: false, Note: "skeleton incomplete (missing " + sub + ")"}
			}
		}
		return CheckResult{Name: "project", OK: true, Note: marlDir}
	}())

	out = append(out, func() CheckResult {
		db := filepath.Join(marlDir, "store.db")
		f, err := os.OpenFile(db, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return CheckResult{Name: "store", OK: false, Note: "cannot read/write " + db + ": " + err.Error()}
		}
		f.Close()
		return CheckResult{Name: "store", OK: true, Note: db}
	}())

	out = append(out, func() CheckResult {
		dir := ControlRootOf(root)
		if err := os.MkdirAll(filepath.Join(dir, "inbox"), 0o755); err != nil {
			return CheckResult{Name: "control_plane", OK: false, Note: "cannot create control plane: " + err.Error()}
		}
		return CheckResult{Name: "control_plane", OK: true, Note: "inbox " + dir + "/inbox/"}
	}())

	return out
}
