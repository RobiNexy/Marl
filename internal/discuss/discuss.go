package discuss

// 讨论的打开 / 草稿修订 / 等待逻辑（Part 11.2 流程 1-5）。
//
// Manager 是本包的唯一入口结构：它持有 fossil 操作与控制面路径，负责
// 讨论 Session 的完整生命周期。agent 包经 agent.DiscussionManager 的窄
// 接口消费（接口在消费侧定义：agent 只需要它列出的五个动作）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"marl/internal/fossil"
	"marl/internal/store"
	"marl/internal/types"
)

// OpenRequest 是一次"Agent 申请讨论"的请求。
//
// TargetPath 是结论落地的仓库相对路径（如 .marl/knowledge/contracts/x.md）；
// 空值由 Manager 用 DefaultTarget 给出。讨论里所有路径判定都过它——
// 结论落地是**框架**的动作（人类审过才算数），不属于任何 Agent 的
// 可写范围。
type OpenRequest struct {
	AgentID    types.AgentID
	Topic      string
	Draft      string
	TargetPath string
}

// DiscussionID 是一次讨论的稳定标识（<branch 名、目录名、审计 target
// 的共同引用键）。ulid 形态与 MessageID 同源（讨论按时间可排序）。
type DiscussionID = string

// Session 是一次进行中讨论的全部必要信息。
//
// 不变量：revision 由 Open=1 / UpdateDraft +1 维护（drift 判定的轮次号）；
// nonce 每轮重置（更新模板即刷新），Agent 永远拿不到它的值。
type Session struct {
	ID      DiscussionID
	Branch  string
	Dir     string // 控制面目录（verdict.md 所在）
	Draft   string // draft.md 的仓库路径（.marl/discussions/<id>/draft.md）
	Target  string // 结论落地路径（仓库相对）
	Topic   string
	AgentID types.AgentID
	// 运行态：
	rev   int    // 第几轮修订（1 = 首开）
	nonce string // 当前轮的裁决凭据（不出包）
}

// OutcomeKind 是等待的裁决形态。
type OutcomeKind int

const (
	// OutcomeAnnotation：人类写了批注（无裁决命令）——Agent 收到批注、
	// 恢复执行，去响应/修订。
	OutcomeAnnotation OutcomeKind = iota
	// OutcomeApproved：人类 @approve——框架接下来 Finalize。
	OutcomeApproved
)

// Outcome 是 Wait 的返回结果。
type Outcome struct {
	Kind OutcomeKind
	// Annotation 是批注文本（Approve 情况下也可能是"审批理由"，可能为空）。
	Annotation string
	// Round 是本轮的修订轮次号（血缘对得上第几轮的草稿——审计用）。
	Round int
}

// Config 是 Manager 的装配参数。
//
// 零值契约：零值不可用（VCS 为 nil / Root 与 ControlDir 为空）；由 New
// 显式拒绝。Quiescence 缺省 10s（设计文档 13.10 fsnotify 条目的静默期）。
type Config struct {
	// Root 是 fossil 工作区根（项目根）。
	Root string
	// MARLDir 是 <root>/.marl（骨架的仓库内配置区，draft.md 落这里）。
	MARLDir string
	// ControlDir 是 ~/.local/state/marl/<project-id>/discussions（verdict.md
	// 的家——在任何 Agent 命名空间之外）。
	ControlDir string
	// DefaultTarget 是 TargetPath 缺省时的落地目录（骨架：
	// .marl/knowledge/contracts）。落地文件名由 topic slug 化。
	DefaultTargetDir string
	// VCS 是 fossil 操作（BranchCreate/BranchSwitch/Add/Commit/Diff）。
	VCS DiscussionVCS
	// PollInterval 是 verdict.md 的扫描步长（轮询实现；[#待验证：fsnotify
	// 在 Linux 下的真实形态是否需要——轮询在本阶段的正确性不受影响]）。
	PollInterval time.Duration
	// Quiescence 是"文件停止变化"窗口（设计文档：10 秒静默期——编辑是
	// 中断式多击键动作，抖动期读取会拿到半截裁决）。
	Quiescence time.Duration
	// DraftRelDir 是 draft.md 的仓库内相对目录（默认 .marl/discussions）。
	DraftRelDir string
	// Audit 非 nil 时记讨论审计（原则 1 的副产品）。
	Audit store.AuditStore
}

// DiscussionVCS 是 Manager 需要的窄 VCS 面（消费侧收窄：讨论不做 diff
// / timeline / revert——那是观测与回滚的需求，不是讨论通路的需求）。
// 实现：internal/fossil.CLI。
type DiscussionVCS interface {
	BranchCreate(ctx context.Context, workdir, name string) error
	BranchSwitch(ctx context.Context, workdir, name string) error
	Add(ctx context.Context, workdir string, relPaths ...string) error
	Commit(ctx context.Context, workdir, author, message string) (string, error)
}

// Manager 讨论生命周期。
//
// 并发：Agent 的讨论通路单 goroutine（eventLoop 唯一调用点）；Wait 的
// watcher goroutine 由 Watch 拥有并在 Wait 返回时退出——Manager 本身
// 无共享可变状态（Session 由调用方持有）。
type Manager struct {
	cfg Config
	// slugCache 不是缓存：topic → 文件名是纯函数，缓存属于过早优化，
	// 这里显式不缓存。
}

// NewManager 构造 Manager。
func NewManager(cfg Config) (*Manager, error) {
	switch {
	case cfg.VCS == nil:
		return nil, fmt.Errorf("discuss: Config.VCS is required")
	case cfg.Root == "":
		return nil, fmt.Errorf("discuss: Config.Root is required")
	case cfg.ControlDir == "":
		return nil, fmt.Errorf("discuss: Config.ControlDir is required")
	case cfg.DefaultTargetDir == "":
		return nil, fmt.Errorf("discuss: Config.DefaultTargetDir is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.Quiescence <= 0 {
		cfg.Quiescence = 10 * time.Second
	}
	if cfg.MARLDir == "" {
		cfg.MARLDir = filepath.Join(cfg.Root, ".marl")
	}
	if cfg.DraftRelDir == "" {
		cfg.DraftRelDir = filepath.Join(".marl", "discussions")
	}
	return &Manager{cfg: cfg}, nil
}

// Open 开一次讨论（Part 11.2 入口 1 的框架处理全步）：
//
//  1. fossil branch discuss/<id>（创建后立即 checkout——draft commit 落
//     在讨论分支上）；
//  2. 控制面目录 <ControlDir>/<id>/（verdict.md 所在），draft.md 写入仓库
//     首次 commit（author=agent）；
//  3. verdict.md 模板（含本轮随机 nonce）；
//  4. 记审计 discussion_opened。
//
// 返回的 Session 由调用方持有（agent 侧在 eventLoop 单 goroutine 中保存
// 直至等待结束）。
func (m *Manager) Open(ctx context.Context, req OpenRequest) (*Session, error) {
	if req.AgentID == "" {
		return nil, fmt.Errorf("discuss: AgentID is required")
	}
	if req.Topic == "" {
		return nil, fmt.Errorf("discuss: topic is required")
	}
	// 讨论 ID：时间可排序 + 与分支名同干净——ulid 的 Crockford 字符集正好
	// 不含 fossil 的问题字符；前缀固定为 discuss_（分支名深查安全）。
	ulid, err := store.NewMessageID()
	if err != nil {
		return nil, fmt.Errorf("discuss: ulid: %w", err)
	}
	// ULID 首字符按规范恒为 0，"0<26 细 char>" 即"时间可排序"的起点，
	// 转小写以便 fst 的分支名一致可读。
	ulid = strings.ToLower(ulid)
	id := "discuss_" + ulid
	branch := id // 讨论；名字与 id 一致（timeline 可读性）
	sess := &Session{
		ID:      id,
		Branch:  branch,
		Dir:     filepath.Join(m.cfg.ControlDir, id),
		Draft:   filepath.Join(m.cfg.DraftRelDir, id, "draft.md"),
		Target:  req.TargetPath,
		Topic:   req.Topic,
		AgentID: req.AgentID,
		rev:     1,
	}
	if sess.Target == "" {
		sess.Target = filepath.Join(m.cfg.DefaultTargetDir, slugify(req.Topic)+".md")
	}

	// 1. 分支。
	if err := m.cfg.VCS.BranchCreate(ctx, m.cfg.Root, branch); err != nil {
		return nil, fmt.Errorf("discuss: branch create: %w", err)
	}
	if err := m.cfg.VCS.BranchSwitch(ctx, m.cfg.Root, branch); err != nil {
		return nil, fmt.Errorf("discuss: branch switch: %w", err)
	}
	// 2. draft.md（仓库内、讨论分支上的首 commit）。
	draftAbs := filepath.Join(m.cfg.Root, filepath.FromSlash(sess.Draft))
	if err := writeDraft(draftAbs, draftHeader(sess, req.Draft)); err != nil {
		return nil, fmt.Errorf("discuss: write draft: %w", err)
	}
	if err := m.cfg.VCS.Add(ctx, m.cfg.Root, sess.Draft); err != nil {
		return nil, fmt.Errorf("discuss: add draft: %w", err)
	}
	if _, err := m.cfg.VCS.Commit(ctx, m.cfg.Root, fossil.UserAgent,
		"marl: 讨论草稿 v1（discuss_<id>「"+req.Topic+"」）"); err != nil &&
		!m.isNothingToCommit(err) {
		return nil, fmt.Errorf("discuss: commit draft: %w", err)
	}
	// 3. 控制面目录 + verdict 模板（人类写；Agent 不可读——Session 不带裁决路径）。
	if err := os.MkdirAll(sess.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("discuss: mkdir control plane: %w", err)
	}
	n, err := newNonce()
	if err != nil {
		return nil, fmt.Errorf("discuss: nonce: %w", err)
	}
	sess.nonce = n
	if err := writeVerdict(filepath.Join(sess.Dir, "verdict.md"), verdictTemplate(sess)); err != nil {
		return nil, fmt.Errorf("discuss: write verdict template: %w", err)
	}
	m.audit(ctx, req.AgentID, "discussion_opened", id, map[string]any{
		"topic": req.Topic, "dir": sess.Dir, "target_path": sess.Target,
		"draft": sess.Draft,
	})
	return sess, nil
}

// UpdateDraft 记录一次草稿修订（Author=agent 的分支 commit）。
//
// 前置条件：sess 是 Open 的产出且未被 Finalize/Abort。
// 失败：写失败 / commit 失败——修版本回滚是 Agent 的职责，不是这里
// 静默假装成功（报告失败会把"我以为写进去改了它写真的写进去了"的混淆
// 又拉出到模型可见面——Part 9.4 的如实回填）。
func (m *Manager) UpdateDraft(ctx context.Context, sess *Session, draft string) error {
	if sess == nil {
		return fmt.Errorf("discuss: nil session")
	}
	sess.rev++
	draftAbs := filepath.Join(m.cfg.Root, filepath.FromSlash(sess.Draft))
	if err := writeDraft(draftAbs, draftHeader(sess, draft)); err != nil {
		return fmt.Errorf("discuss: write draft: %w", err)
	}
	if err := m.cfg.VCS.Add(ctx, m.cfg.Root, sess.Draft); err != nil {
		return fmt.Errorf("discuss: add draft: %w", err)
	}
	if _, err := m.cfg.VCS.Commit(ctx, m.cfg.Root, fossil.UserAgent,
		fmt.Sprintf("marl: 讨论草稿 v%d（%s「%s」）", sess.rev, sess.ID, sess.Topic)); err != nil &&
		!m.isNothingToCommit(err) {
		return fmt.Errorf("discuss: commit draft: %w", err)
	}
	m.audit(ctx, sess.AgentID, "discussion_revised", sess.ID, map[string]any{"round": sess.rev})
	return nil
}

// isNothingToCommit 是"草稿内容没变"时的正常路径（Add 之后的状态是
// 已提交过——同一份修订重复调用）。
func (m *Manager) isNothingToCommit(err error) bool {
	return err == fossil.ErrNothingToCommit ||
		(err != nil && strings.Contains(err.Error(), "nothing has changed"))
}

// audit 记讨论审计（Audit 为 nil 时跳过）。
func (m *Manager) audit(ctx context.Context, agentID types.AgentID, action, target string, payload map[string]any) {
	if m.cfg.Audit == nil {
		return
	}
	ev := &store.AuditEvent{AgentID: agentID, Action: action, Target: target, Payload: payload}
	if err := m.cfg.Audit.Append(ctx, ev); err != nil {
		// 审计写失败不能改讨论业务结论（Print 与 agent.auditf 同纪律）。
		fmt.Printf("marl: discuss audit failed (%s): %v\n", action, err)
	}
}

// draftHeader 渲染 draft.md 的完整文本（标题 + 正文）。
func draftHeader(s *Session, draft string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# 草稿：讨论「%s」\n\n", s.Topic)
	fmt.Fprintf(&sb, "（讨论分支 %s；由 Agent 修订，author=agent）\n\n", s.Branch)
	sb.WriteString(draft)
	return sb.String()
}

// writeDraft 原子写草稿（WriteFile + 新文件权限 0o644；草稿是仓库内容，
// 不需要 0o600——readable by human）。
func writeDraft(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// writeVerdict 模板写 verdict（每次 Open 与每轮 Annotation 后都重置为
// 新 nonce——注解轮结束后重置留白供人类下一轮裁决）。
func writeVerdict(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// verdictTemplate 渲染人类写的裁决文件。
//
// nonce 的位置：frontmatter（人类不编辑 frontmatter 区域的内容原则——
// 只要它不动 frontmatter，Agent 伪造不出当轮 nonce）。
func verdictTemplate(s *Session) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "discussion: %s\n", s.ID)
	fmt.Fprintf(&sb, "nonce: %s\n", s.nonce)
	sb.WriteString("---\n\n")
	sb.WriteString("下面写裁决或批注。命令词（单独成行）：\n")
	sb.WriteString("  @approve  通过——框架把草稿最终版落到目标路径（author=human 的 commit）\n")
	sb.WriteString("  @reject   拒绝——结束讨论，不落地，Agent 恢复执行\n\n")
	sb.WriteString("其余内容按批注处理（Agent 会读到并回应）。\n\n## 批注\n\n")
	return sb.String()
}

// slugify 把 topic 变成文件名安全的 slug（中文/空格 → 下划线；空 → untitled）。
func slugify(topic string) string {
	s := strings.ToLower(strings.TrimSpace(topic))
	re := regexp.MustCompile(`[^0-9a-z]+`)
	s = re.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "topic"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
