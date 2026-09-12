package escalate

// 路由判断与人类文件信箱（Part 11.3 / 11.5，13.12 阶段 10）。

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"marl/internal/proto"
	"marl/internal/store"
	"marl/internal/types"
)

// Target 是路由结果（Route 的判定面）。
type Target int

const (
	TargetHuman  Target = iota // 人类文件信箱
	TargetParent               // 父的信箱（envelope）
)

// Route 实现 EscalationRule 的消费约定（Part 11.3 流程 2）：
//
//	有父且父未 Blocked → 发给父（TargetParent）；
//	否则              → 发给人类信箱（TargetHuman）。
//
// 失败：rule 无出口（Validate 不过——"没有出口而静默"的丢弃求助是最坏
// 失败形态：发起者永远阻塞在 Blocked(Escalating)，系统各处无报错）。
func Route(rule proto.EscalationRule, req *proto.EscalationRequest) (Target, error) {
	if err := rule.Validate(); err != nil {
		return TargetHuman, err
	}
	if req == nil {
		return TargetHuman, fmt.Errorf("escalate: nil request")
	}
	if rule.ParentID != "" && !rule.ParentBlocked {
		return TargetParent, nil
	}
	return TargetHuman, nil
}

// MailboxConfig 是人类文件信箱的装配参数。
type MailboxConfig struct {
	// ControlRoot 是 ~/.local/state/marl/<project-id>/（Agent 不可触——
	// "回复的种类由通道决定"的路径事实）。
	ControlRoot string
	// PollInterval / Quiescence：verdict 同款参数（真实 500ms / 10s；
	// 测试可缩小——契约测"静默窗口"的语义，不测具体数值）。
	PollInterval time.Duration
	Quiescence   time.Duration
	// Audit 非 nil 时记生命周期事件（原则 1 的副产品）。
	Audit store.AuditStore
}

// Mailbox 是人类文件信箱（pending → done 的写入与读取面）。
//
// archived/ 的调度（7 天归档、30 天删）是运行期维护（Part 11.5 的"周期
// 扫描"归宿主），不进本库的不变量面——Mailbox 保证的是 pending→done 的
// 搬运后可读；[偏离文档: archived 的 Go 端接不进 CLI/daemon 的当前形态。
type Mailbox struct {
	cfg MailboxConfig
}

// NewMailbox 构造（目录创建——缺目录是信箱断链的隐形失败，必须显式）。
func NewMailbox(cfg MailboxConfig) (*Mailbox, error) {
	if cfg.ControlRoot == "" {
		return nil, fmt.Errorf("escalate: ControlRoot is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.Quiescence <= 0 {
		cfg.Quiescence = 10 * time.Second
	}
	for _, sub := range []string{"pending", "done", "archived"} {
		if err := os.MkdirAll(filepath.Join(cfg.ControlRoot, "requests", sub), 0o755); err != nil {
			return nil, fmt.Errorf("escalate: mkdir %s: %w", sub, err)
		}
	}
	return &Mailbox{cfg: cfg}, nil
}

// EscalationFile 是一次 file-mailbox 求助的控制面事实。
type EscalationFile struct {
	ID    types.EscalationID
	Path  string // pending 文件的路径（人类移动后此处失效；回复在 done/）
	Nonce string
	Req   *proto.EscalationRequest
}

// Submit 创建一份求助文件（Part 11.3 流程 5 的框架形态）。
//
// 形态：pending/escalation_<ulid>.md，frontmatter 带 nonce（Agent 读不到
// 控制面 → 伪不出当轮凭据；resp 读侧校验这一对应关系）。
func (m *Mailbox) Submit(req *proto.EscalationRequest) (*EscalationFile, error) {
	if req == nil || req.Question == "" || req.From == "" {
		return nil, fmt.Errorf("escalate: submit requires from/question")
	}
	ulid, err := store.NewMessageID()
	if err != nil {
		return nil, fmt.Errorf("escalate: ulid: %w", err)
	}
	id := types.EscalationID("escalation_" + strings.ToLower(ulid))
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("escalate: nonce: %w", err)
	}
	f := &EscalationFile{
		ID:    id,
		Path:  filepath.Join(m.cfg.ControlRoot, "requests", "pending", string(id)+".md"),
		Nonce: fmt.Sprintf("%x", nonce),
		Req:   req,
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "escalation_id: %s\n", id)
	fmt.Fprintf(&sb, "nonce: %s\n", f.Nonce)
	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb, "## 求助来源\n\nfrom: %s\nquestion: %s\n\n%s\n",
		req.From, req.Question, req.Reason)
	if req.ContextSummary != "" {
		fmt.Fprintf(&sb, "context: %s\n", req.ContextSummary)
	}
	sb.WriteString("\n## 回复\n\n（回复写在这里，然后把整个文件移到 requests/done/）\n")
	if err := os.WriteFile(f.Path, []byte(sb.String()), 0o644); err != nil {
		return nil, fmt.Errorf("escalate: write request: %w", err)
	}
	m.audit(req.From, "escalation_opened", string(id), map[string]any{
		"question": req.Question, "path": f.Path,
	})
	return f, nil
}

// WaitReply 轮询等待回复（done/ 出现本求助的文件 → 静默窗口 → 读正文）。
//
// [偏离文档: fsnotify → mtime 轮询（与 discuss 的 watcher 同一取舍，
// 参数语义不变面——PollInterval / Quiescence 两个配置）。]
func (m *Mailbox) WaitReply(ctx context.Context, f *EscalationFile) (string, error) {
	doneDir := filepath.Join(m.cfg.ControlRoot, "requests", "done")
	name := string(f.ID) + ".md"
	path := filepath.Join(doneDir, name)
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	var modSeen time.Time
	var firstSeen time.Time
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
		st, err := os.Stat(path)
		if err != nil {
			continue // 未移动 / 权限未恢复：继续等（控制面在人类手上，自愈）
		}
		if mt := st.ModTime(); mt != modSeen {
			modSeen = mt
			firstSeen = time.Now()
			continue
		}
		if firstSeen.IsZero() || time.Since(firstSeen) < m.cfg.Quiescence {
			continue
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		reply, ok := replyOf(string(content), f.Nonce)
		if !ok {
			continue // 编辑中间态 / 凭据不匹配（原则 4）
		}
		m.audit(f.Req.From, "escalation_reply_received", string(f.ID), nil)
		return reply, nil
	}
}

// replyOf 是 frontmatter 校验（nonce）+ 正文剥离（人类的回复不强制格式——
// "## 回复"之后的一切都是回复；标题缺失时整段正文）。
func replyOf(content, wantNonce string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return "", false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", false
	}
	nonce := ""
	for i := 1; i < end; i++ {
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "nonce" {
			nonce = strings.TrimSpace(v)
		}
	}
	if nonce != wantNonce {
		return "", false
	}
	body := strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	if i := strings.Index(body, "## 回复"); i >= 0 {
		body = strings.TrimSpace(body[i+len("## 回复"):])
	}
	return body, true
}

// audit 记生命周期事件（Audit nil 跳过；写失败打 stderr——求助不因
// 审计写入失败而不确死业务（[权衡: 与 agent.auditf 同纪律]）。
func (m *Mailbox) audit(agentID types.AgentID, action, target string, payload map[string]any) {
	if m.cfg.Audit == nil {
		return
	}
	ev := &store.AuditEvent{AgentID: agentID, Action: action, Target: target, Payload: payload}
	if err := m.cfg.Audit.Append(context.Background(), ev); err != nil {
		fmt.Printf("marl: escalate audit failed (%s): %v\n", action, err)
	}
}
