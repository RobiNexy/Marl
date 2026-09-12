package fossil

// 工作区操作：Status / Add / Commit / Timeline / Diff（Part 8.4 的提交通路）。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Change 是 `fossil changes` 的一行（工作区相对路径 + 状态标记）。
type Change struct {
	Path string // 工作区相对路径
	Kind string // fossil 的状态标记（ADDED / EDITED / DELETED / MISSING / RENAMED / ...）
}

// Status 返回工作区改动（已跟踪文件的 `fossil changes` + 未跟踪文件的
// `fossil extra`；读命令，可并发）。
//
// 两命令合并的理由（实测记录见 doc.go）：`changes` 只列**已跟踪**文件的
// 变更（ADDED/EDITED/...），新文件在 add 之前只出现在 `extra` 里——单写者
// 提交流程"子写文件 → 父 Add → Commit"的第一步（发现有哪些文件要 add）
// 必须看到两者。
//
// 输出解析：changes 每行 "<KIND><变长空白><path>"；extra 每行一个路径
// （Kind=EXTRA，语义 = "未跟踪"——add 后变 ADDED）。
//
// 失败：未 open → ErrNotOpen。
func (c *CLI) Status(ctx context.Context, workdir string) ([]Change, error) {
	res, err := c.runRead(ctx, workdir, "changes")
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, ln := range strings.Split(res.stdout, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		parts := strings.SplitN(ln, " ", 2)
		if len(parts) != 2 {
			continue // 头部行跳过
		}
		kind := strings.TrimSpace(parts[0])
		path := strings.TrimSpace(parts[1])
		if kind == "" || path == "" {
			continue
		}
		out = append(out, Change{Path: path, Kind: kind})
	}
	// extra：未跟踪文件（ignore-glob 在这一层也生效——SKIP 的文件不出现）。
	extra, err := c.runRead(ctx, workdir, "extra")
	if err != nil {
		return nil, fmt.Errorf("fossil extra: %w", err)
	}
	for _, ln := range strings.Split(extra.stdout, "\n") {
		path := strings.TrimRight(strings.TrimSpace(ln), "\r")
		if path == "" {
			continue
		}
		out = append(out, Change{Path: path, Kind: "EXTRA"})
	}
	return out, nil
}

// Add 把路径加入本次提交（`fossil add`；目录递归，. 开头文件默认跳过——
// fossil 默认行为，ignore-glob 在这一层生效：SKIP 而非 ADDED）。
//
// relPaths 必须是工作区相对路径（调用方来自框架内部路径清单，不是模型输入；
// 绝对路径与 .. 由调用方杜绝——本方法不重复校验，因为 add 的目标是"框架
// 自己知道要提交什么"，与技能层的沙箱是两个信任域）。
func (c *CLI) Add(ctx context.Context, workdir string, relPaths ...string) error {
	if len(relPaths) == 0 {
		return nil
	}
	args := append([]string{"add"}, relPaths...)
	if _, err := c.runWrite(ctx, workdir, args...); err != nil {
		return fmt.Errorf("fossil add: %w", err)
	}
	return nil
}

// Commit 提交当前 checkout 的全部已登记改动（单写者模型的唯一提交点）。
//
// author 必须是三个框架用户之一（UserHuman/UserAgent/UserSystem）——
// Part 8.4 的 "author 字段按调用路径填" 的落点；Agent 无法影响它，因为
// 调用方（agent.doCommit）是框架代码，不是模型输出。
//
// --no-verify-comment：fossil 默认对 comment 做 fossil-wiki 格式检查
// （<tag>/&/[links]/_下划线_ 都是触发词）——commit message 是机器生成的
// 结构化摘要（子 report 的文本片段），wiki 误报是常态（实测记录见测试
// 报告阶段 6）；timeline 的可读性不依赖 wiki 渲染。
//
// 失败：无改动 → ErrNothingToCommit（正常空转，调用方按需跳过）；
// 未 open → ErrNotOpen；冲突/其它 → 底层错误。
func (c *CLI) Commit(ctx context.Context, workdir, author, message string) (string, error) {
	if author != UserHuman && author != UserAgent && author != UserSystem {
		return "", fmt.Errorf("fossil commit: author %q is not a framework user (part 8.4: author is decided by the call path)", author)
	}
	res, err := c.runWrite(ctx, workdir, "commit", "-m", message,
		"--no-prompt", "--no-verify-comment", "-U", author)
	if err != nil {
		return "", err
	}
	// 新版本 hash 从 stdout 取（"New_Version: <64hex>"）。
	for _, ln := range strings.Split(res.stdout, "\n") {
		if hash, ok := strings.CutPrefix(strings.TrimSpace(ln), "New_Version: "); ok {
			return hash, nil
		}
	}
	return "", nil // 空 commit（--allow-empty 未用，理论上到不了这里）
}

// TimelineEntry 是 timeline 的一行（报表/验证用）。
type TimelineEntry struct {
	Time    time.Time
	Hash    string
	Comment string
	Author  string
}

// Timeline 返回最近 n 条提交（`fossil timeline -n <n>`；读命令）。
//
// 解析 fossil timeline 的默认形态：
//
//	=== 2026-09-12 ===
//	06:50:45 [e2442af0fb] *CURRENT* test commit (user: agent:root tags: trunk)
//
// 失败：仓库不存在 → ErrNotFound。
func (c *CLI) Timeline(ctx context.Context, repoPath string, n int) ([]TimelineEntry, error) {
	res, err := c.runRead(ctx, "", "timeline", "-R", repoPath, "-n", fmt.Sprint(n))
	if err != nil {
		return nil, err
	}
	return parseTimeline(res.stdout), nil
}

// parseTimeline 解析 timeline 输出（纯函数；错误行静默跳过——timeline 的
// 装饰行（=== 分隔、+++ no more data）不是错误，是格式）。
//
// 并发：纯函数。
func parseTimeline(out string) []TimelineEntry {
	var entries []TimelineEntry
	var day string // "=== 2026-09-12 ===" 携带日期，时间行只带时刻
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		trimmed := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(trimmed, "===") && strings.HasSuffix(trimmed, "==="):
			day = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "==="), "==="))
		case strings.HasPrefix(trimmed, "+++"):
			continue // 结束标记
		case trimmed == "":
			continue
		default:
			// 06:50:45 [hash] *CURRENT* comment (user: x tags: y)
			e, ok := parseTimelineLine(trimmed, day)
			if ok {
				entries = append(entries, e)
			}
		}
	}
	return entries
}

// parseTimelineLine 解析单行。
func parseTimelineLine(ln, day string) (TimelineEntry, bool) {
	// 时间 [hash] [flags] comment (user: ...)
	open := strings.Index(ln, "[")
	close := strings.Index(ln, "]")
	if open < 0 || close < open {
		return TimelineEntry{}, false
	}
	timeStr := strings.TrimSpace(ln[:open])
	hash := ln[open+1 : close]
	rest := strings.TrimSpace(ln[close+1:])
	rest = strings.TrimPrefix(rest, "*CURRENT* ")
	// comment 与 (user: ...) 分离。
	comment := rest
	author := ""
	if i := strings.LastIndex(rest, "(user: "); i >= 0 {
		comment = strings.TrimSpace(rest[:i])
		author = strings.TrimSpace(strings.TrimSuffix(rest[i+len("(user: "):], ")"))
		if j := strings.Index(author, " tags:"); j >= 0 {
			author = strings.TrimSpace(author[:j])
		}
	}
	ts, err := time.Parse("2006-01-02 15:04:05", day+" "+timeStr)
	if err != nil {
		ts = time.Time{}
	}
	return TimelineEntry{Time: ts, Hash: hash, Comment: comment, Author: author}, true
}

// Diff 返回工作区与 checkout 的差异（验证用：确认子写的文件在 diff 里）。
func (c *CLI) Diff(ctx context.Context, workdir string, relPaths ...string) (string, error) {
	args := append([]string{"diff"}, relPaths...)
	res, err := c.runRead(ctx, workdir, args...)
	if err != nil {
		if strings.Contains(err.Error(), ErrNothingToCommit.Error()) {
			return "", nil // 无差异是正常状态
		}
		return "", err
	}
	return res.stdout, nil
}

// RunLS 返回 checkout 的文件清单（`fossil ls`；验证用：确认骨架入库）。
func (c *CLI) RunLS(ctx context.Context, workdir string) (string, error) {
	res, err := c.runRead(ctx, workdir, "ls")
	if err != nil {
		return "", err
	}
	return res.stdout, nil
}

// Abs 把工作区相对路径转绝对（Commit 前的 Add 目标解析）。
func (c *CLI) Abs(workdir, rel string) string {
	return filepath.Join(workdir, rel)
}
