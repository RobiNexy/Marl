package main

// doctor 子命令：环境自检（用户跑不起来之前的"为什么"清单）。
//
// 检查项按"缺了它任务必然失败"排序：fossil 可执行、API key、
// .marl 骨架完整性、store 可写、控制面可写。每项 ✓/✗ + 一行修复提示
// ——自检的价值在提示，不在状态本身。

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("marl doctor", flag.ContinueOnError)
	dir := fs.String("dir", ".", "项目目录（检查骨架与 store；缺省当前目录）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := filepathAbs(*dir)

	check("fossil 可执行", func() (string, string) {
		_, err := exec.LookPath("fossil")
		if err != nil {
			return "✗", "未找到 fossil 命令——从 https://fossil-scm.org 安装（2.20+）"
		}
		out, _ := exec.Command("fossil", "version").Output()
		line := strings.SplitN(string(out), "\n", 2)[0]
		return "✓", line
	})
	check("API key（DEEPSEEK_API_KEY）", func() (string, string) {
		if os.Getenv("DEEPSEEK_API_KEY") == "" {
			return "✗", "环境变量为空——export DEEPSEEK_API_KEY=sk-... （真跑必需；离线回放不需要）"
		}
		return "✓", "已设置（前 8 位 " + maskKey(os.Getenv("DEEPSEEK_API_KEY")) + "）"
	})
	marlDir := filepath.Join(root, ".marl")
	check(".marl 骨架（"+marlDir+"）", func() (string, string) {
		if _, err := os.Stat(filepath.Join(marlDir, "project.fossil")); err != nil {
			return "✗", "不是 Marl 项目（缺 project.fossil）——先 marl init " + root
		}
		for _, sub := range []string{"config.yaml", "profiles", "prompts", "knowledge"} {
			if _, err := os.Stat(filepath.Join(marlDir, sub)); err != nil {
				return "✗", "骨架不完整（缺 " + sub + "）——重新 init 到新目录后对照补齐"
			}
		}
		return "✓", "完整"
	})
	check("store 可读写", func() (string, string) {
		db := filepath.Join(marlDir, "store.db")
		f, err := os.OpenFile(db, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return "✗", "无法读写 " + db + "（权限？）：" + err.Error()
		}
		f.Close()
		return "✓", db
	})
	check("控制面可写（"+controlRootOf(root)+"）", func() (string, string) {
		dir2 := controlRootOf(root)
		if err := os.MkdirAll(filepath.Join(dir2, "inbox"), 0o755); err != nil {
			return "✗", "无法创建控制面目录（XDG_STATE_HOME？）：" + err.Error()
		}
		return "✓", "收件箱 " + dir2 + "/inbox/"
	})
	return nil
}

// check 是单项检查的渲染（mark/status 两段；出错不中断整体——doctor
// 的价值是全貌）。
func check(name string, fn func() (mark, note string)) {
	mark, note := fn()
	fmt.Printf(" %s %-28s %s\n", mark, name, note)
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:8] + "…"
}
