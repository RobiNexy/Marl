package agent

// 单写者提交（Part 8.4，13.8 阶段 6）。
//
// 子 Agent 只写文件、不碰 VCS；提交由**阻塞恢复后的父 Agent**做，一次
// commit = 一轮分治的完整产出。三个收益（Part 8.4）：VCS 无并发、commit
// 粒度对齐语义、父有机会在 commit 前拦截（回滚发生在 commit 之前）。
//
// 触发点（Part 8.8 + 13.8）：父在 awaitChildren 恢复（全部子 report 落库）
// 之后、下一轮编排之前——见 Run 的等待循环。

import (
	"context"
	"fmt"
	"strings"

	"github.com/RobiNexy/Marl/internal/fossil"
	"github.com/RobiNexy/Marl/internal/proto"
)

// CommitConfig 是单写者提交的装配参数（Config.Committer 非 nil 即启用）。
//
// 零值契约：零值不可用（VCS 为 nil）；由 New 校验拒绝。
type CommitConfig struct {
	// VCS 是 fossil 工作区操作（internal/fossil 的 CLI）。
	VCS Committer
	// RepoPath 是仓库文件（timeline/diff 的 -R 目标）。
	RepoPath string
}

// Committer 是提交所需的最窄接口（消费侧定义；internal/fossil.CLI 满足它）。
// 收窄到三个动作：发现改动、登记、提交——timeline/diff 是验证与报表的
// 需求，不属于 Agent 的提交通路。
type Committer interface {
	// Status 返回工作区改动（发现要提交的文件）。
	Status(ctx context.Context, workdir string) ([]fossil.Change, error)
	// Add 登记路径（ignore-glob 在这一层生效）。
	Add(ctx context.Context, workdir string, relPaths ...string) error
	// Commit 提交并返回新版本 hash。
	Commit(ctx context.Context, workdir, author, message string) (string, error)
}

// doCommit 执行一次单写者提交：Status → Add（全部改动）→ Commit
// （author=agent——调用路径是父 Agent 的恢复点，Part 8.4）。
//
// 只在父阻塞恢复后调用（Run 的等待循环里）——这是单写者纪律的结构保证：
// 子 Agent 的 Config 没有 Committer（装配层不给），提交点在父的唯一代码
// 路径上。
//
// 失败：
//   - 无改动 → 跳过（返回 nil，不报错——"全部子都是纯阅读任务"是合法
//     状态，空 commit 没有意义）；
//   - fossil 故障 → 错误上抛（提交失败必须可见：Log 里没有这条 commit
//     而 report 声称成功，是下次任务的对账线索）。
//
// commit message 是结构化摘要（子 report 的状态行），不是自由文本——
// timeline 的可读性由它保证。
func (a *Agent) doCommit(ctx context.Context) error {
	if a.commit == nil || a.commit.VCS == nil {
		return nil
	}
	changes, err := a.commit.VCS.Status(ctx, a.env.ProjectRoot)
	if err != nil {
		return fmt.Errorf("agent: commit status: %w", err)
	}
	if len(changes) == 0 {
		return nil // 无改动：跳过（不空转）
	}
	paths := make([]string, 0, len(changes))
	for _, ch := range changes {
		paths = append(paths, ch.Path)
	}
	if err := a.commit.VCS.Add(ctx, a.env.ProjectRoot, paths...); err != nil {
		return fmt.Errorf("agent: commit add: %w", err)
	}
	message := a.commitMessage()
	hash, err := a.commit.VCS.Commit(ctx, a.env.ProjectRoot, fossil.UserAgent, message)
	if err != nil {
		return fmt.Errorf("agent: commit: %w", err)
	}
	// 提交事实进审计（可追溯：hash ↔ 任务 ↔ 子 report 的对账键）。
	a.auditf(ctx, "commit", hash, map[string]any{
		"task":    string(a.taskID),
		"files":   paths,
		"message": message,
	})
	return nil
}

// commitMessage 构造提交说明：任务 + 各子的 report 状态（一行一个）。
//
// 单写者模型下"一次 commit = 一轮分治的完整产出"，message 就是这轮的
// 账目——timeline 上能看出这轮谁干了什么、结论是什么。
func (a *Agent) commitMessage() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "marl[%s] 任务完成提交", a.taskID)
	a.mu.Lock()
	reports := append([]*proto.ChildReport(nil), a.lastReports...)
	a.mu.Unlock()
	for _, r := range reports {
		fmt.Fprintf(&sb, "\n  %s: %s (%s)", r.ChildID, r.Status, firstLineOf(r.Report))
	}
	if len(reports) == 0 {
		sb.WriteString("\n  （无子任务；父直接完成）")
	}
	return sb.String()
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
