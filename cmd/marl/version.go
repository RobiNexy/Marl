package main

// version 子命令：打印构建信息（git 提交 / 构建时间）。
//
// 两条信息来源：-ldflags 注入（release 构建的正式形态：go build
// -ldflags "-X main.version=v0.1.0"）或 Go 工具链的 VCS 戳记
//（go build 自带的 vcs.revision / vcs.time——本地构建的形态）。
// 都没有（如 go run）→ 显示 dev。

import (
	"fmt"
	"runtime/debug"
)

// version 是 release 构建的注入点（-ldflags "-X main.version=..."）。
// 空 = 未注入，回退到 VCS 戳记 / dev。
var version string

// cmdVersion 打印版本信息（无 flag 的简单出口）。
func cmdVersion(args []string) error {
	if version != "" {
		fmt.Println("marl " + version)
		return nil
	}
	rev, when := vcsInfo()
	if rev == "" {
		fmt.Println("marl dev（本地构建；release 版会带版本号与提交号）")
		return nil
	}
	fmt.Printf("marl dev (commit %s, built %s)\n", rev, when)
	return nil
}

// vcsInfo 从构建信息里读 VCS 戳记（go build 自动注入；go test/go run 无）。
func vcsInfo() (rev, when string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			when = s.Value
		}
	}
	if len(rev) > 10 {
		rev = rev[:10]
	}
	return rev, when
}
