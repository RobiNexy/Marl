package discuss

// 讨论结论的落地（Part 11.2 流程 6，13.10 的 merge.go）。
//
// Finalize 的判定：
//
//	a. 参照草稿在讨论分支上的**最终形态**（分支最后一次 draft commit 的
//	   工作区内容——单写者纪律下工作区即真相，不重新从分支历史反演）；
//	b. checkout trunk（框架分支——项目默认分支。fossil 的 trunk 是 datetime
//	   语义的默认分支，全部项目初始化内容都在上面）；
//	c. 把草稿内容写到 Target 路径（结论文件的内容 = 草稿全文——契约文件
//	   的 canonical 内容就是讨论的最终版，落地的 transform 只有"写到契约路径"）；
//	d. Add + commit（author=human——原则 4 的通道属性：@approve 是人类
//	   的意志经由 verdict.md 表达的，落地通道只有人类能写）；
//	e. 讨论**分支保留**（Part 11.2 流程 6 d：历史不删，回溯讨论过程在
//	   fossil timeline 可查）。
//
// 失败：
//   - checkout 失败 / 写 Target 失败 / commit 失败——原样上抛（落地失败
//     意味着结论没进主干，绝不能告诉 Agent"讨论完成了"）。
//
// [权衡: 未实现 fossil merge（分支合并到主干）。理由：讨论的产物是"结论
// 文件写进 Target 路径"，不是"分支的全部改动"（分支上只有
// .marl/discussions/<id>/draft.md，合并没有语义增益却引入 base 冲突
// 处理面）；fos merge 的用武之地是"实验分支上的代码改动"，那是
// request_branch 的未来场景。]

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"marl/internal/fossil"
	"strings"
)

// trunkBranch 是主干分支名（fossil 的默认分支名固定为 trunk）。
const trunkBranch = "trunk"

// Finalize 把讨论结论落到 Target 路径并 commit（author=human）。
//
// 后置条件（成功时）：Target 文件存在且内容 = 草稿最终版；返回 commit
// hash（fossil timeline 的对账键——13.10 交付检查里"author=human 的
// commit 在 timeline 可见"的判据输入）。
func (m *Manager) Finalize(ctx context.Context, sess *Session) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("discuss: nil session")
	}
	// Draft 的当前内容 = 工作区文件（讨论分支 checkout 状态下，单写者
	// 纪律保证了"上次 UpdateDraft 之后没人碰过它"——Agent 不可写、人类
	// 编辑的产物在 verdict 而不是 draft）。
	draftPath := filepath.Join(m.cfg.Root, filepath.FromSlash(sess.Draft))
	content, err := os.ReadFile(draftPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("discuss: read draft for finalize: %w", err)
	}
	if os.IsNotExist(err) {
		// 首开即审批、从未产生的特殊状态（Review-only 草稿遗漏）按错误
		// 处理：落地"不存在"的草稿等于把"空文件"当结论。
		return "", fmt.Errorf("discuss: draft %s missing", sess.Draft)
	}
	if err := m.cfg.VCS.BranchSwitch(ctx, m.cfg.Root, trunkBranch); err != nil {
		return "", fmt.Errorf("discuss: checkout trunk: %w", err)
	}
	targetAbs := filepath.Join(m.cfg.Root, filepath.FromSlash(sess.Target))
	if err := os.MkdirAll(filepath.Dir(targetAbs), 0o755); err != nil {
		return "", fmt.Errorf("discuss: mkdir target dir: %w", err)
	}
	if err := os.WriteFile(targetAbs, appendTrailingNewline(content), 0o644); err != nil {
		return "", fmt.Errorf("discuss: write conclusion: %w", err)
	}
	if err := m.cfg.VCS.Add(ctx, m.cfg.Root, sess.Target); err != nil {
		return "", fmt.Errorf("discuss: add conclusion: %w", err)
	}
	msg := fmt.Sprintf("Finalize「%s」after discussion (%s, agent=%s): %s", sess.Topic, sess.ID, sess.AgentID, sess.Draft)
	hash, err := m.cfg.VCS.Commit(ctx, m.cfg.Root, fossil.UserHuman, msg)
	if err != nil {
		return "", fmt.Errorf("discuss: finalize commit: %w", err)
	}
	m.audit(ctx, sess.AgentID, "discussion_concluded", sess.ID, map[string]any{
		"topic": sess.Topic, "target_path": sess.Target, "commit": hash,
		"dir": sess.Dir,
	})
	return hash, nil
}

// targetAbs 的文件内容以单个 \n 结尾（md 文本的 POSIX 惯例，下游 diff 干净）。
func appendTrailingNewline(b []byte) []byte {
	s := strings.TrimRight(string(b), "\n")
	return []byte(s)
}
