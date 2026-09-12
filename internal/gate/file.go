package gate

// FileApprover：审批文件 + nonce（与讨论 verdict 同构的裁决文件，Part 11.3
// §3.2 的挂起/恢复；prd 的 grant 形态由文件内容约定承载——见审批模板）。

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"marl/internal/store"
)

// FileApprover 是人类审批文件面的实现。
//
// 形态：<ControlRoot>/approvals/approval_<ulid>.md：
//
//	---
//	approval_id: approval_<ulid>
//	nonce: <hex>
//	kind: llm_call|orchestration|...
//	---
//
//	## 注意事项（来自规则的 Reason/属性摘要）
//	（人类不编辑上面）
//
//	## 裁决
//
//	@grant once            / @grant next 20       / @grant tokens 50000
//	@always-grant          （永久放行 → 动态 allow 规则，落盘）
//	@deny
//	（批注写在裁决行之后；与批准值同时生效）
//
// 种类与审批的写权限：控制面目录（Agent 不可写、事实上不可见——verdict
// 语义的同一防线）；nonce 防陈旧裁决回放。裁决是人类的**通道**属性，还是
// 原则 4：写权限是唯一的信任边界。
type FileApprover struct {
	// ControlRoot 是 ~/.local/state/marl/<project-id>/（与 escalate 的
	// requests 同侧的 approvals 子目录）。
	ControlRoot string
	// PollInterval / Quiescence 是 mtime 轮询参数（与 discuss/escalate
	// 同一参数面；测试缩小）。
	PollInterval time.Duration
	Quiescence   time.Duration
	// PersistAlways 是 GrantAlways 的落盘钩子（直接中转 Manager 的钩子
	// ——审批文件自身不创作规则）。
	PersistAlways func(ctx context.Context, rule Rule) error
}

// NewFileApprover 构造（目录创建——approvals 目录缺失是审批断链的隐形失败）。
func NewFileApprover(controlRoot string, poll, quiescence time.Duration) (*FileApprover, error) {
	if controlRoot == "" {
		return nil, fmt.Errorf("gate: ControlRoot is required")
	}
	dir := filepath.Join(controlRoot, "approvals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("gate: mkdir approvals: %w", err)
	}
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	if quiescence <= 0 {
		quiescence = 10 * time.Second
	}
	return &FileApprover{ControlRoot: controlRoot, PollInterval: poll, Quiescence: quiescence}, nil
}

// Ask 实现 Approver：写审批文件 → 轮询 → 解析裁决。
//
// 超时/取消语义：ctx 取消即返回错误——Agent 侧的 Blocked(AwaitingGate)
// 消费者会把错误转回 "审批还在等" 的挂起；永久挂起不可能（宿主 Stop
// 的 ctx 总在，Part 11.3 的"挂起是便宜的"——不再计费）。
func (fa *FileApprover) Ask(ctx context.Context, req *Request, rule Rule) (*Decision, Grant, error) {
	ulid, err := store.NewMessageID()
	if err != nil {
		return nil, Grant{}, fmt.Errorf("gate: ulid: %w", err)
	}
	id := fmt.Sprintf("approval_%s", strings.ToLower(ulid))
	nonce := make([]byte, 8)
	if _, nerr := rand.Read(nonce); nerr != nil {
		return nil, Grant{}, fmt.Errorf("gate: nonce: %w", nerr)
	}
	nonceStr := fmt.Sprintf("%x", nonce)
	path := filepath.Join(fa.ControlRoot, "approvals", id+".md")
	body := templateOf(id, nonceStr, req, rule)
	if werr := os.WriteFile(path, []byte(body), 0o644); werr != nil {
		return nil, Grant{}, fmt.Errorf("gate: write approval file: %w", werr)
	}
	// 轮询面：文件出现"裁决面"（nonce 校验 + 决策行解析）→ 返回。
	dec, g, ok, perr := fa.wait(ctx, path, nonceStr)
	if perr != nil {
		return nil, Grant{}, perr
	}
	if !ok {
		return nil, Grant{}, fmt.Errorf("gate: approval %s 未见裁决（ctx 取消会返回挂起错误；审批文件保留在 approvals/）", id)
	}
	dec.RuleID = rule.ID
	return dec, g, nil
}

// templateOf 渲染审批文件（frontmatter + 触发摘要 + 裁决模板）。
func templateOf(id, nonce string, req *Request, rule Rule) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "approval_id: %s\n", id)
	fmt.Fprintf(&sb, "nonce: %s\n", nonce)
	fmt.Fprintf(&sb, "kind: %s\n", req.Kind)
	fmt.Fprintf(&sb, "agent: %s\n", req.AgentID)
	fmt.Fprintf(&sb, "rule: %s\n", rule.ID)
	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb, "属性：%s\n\n", AttributesSummary(req.Attributes))
	sb.WriteString("裁决（修改下面一行 @ 命令；命令后的行都是批注面）：\n\n")
	sb.WriteString("@grant once         # 放行这一次\n")
	sb.WriteString("@grant next 20      # 放行接下来 20 次（额度内不打扰人类）\n")
	sb.WriteString("@grant tokens 50000 # 追加 50K token 额度\n")
	sb.WriteString("@always-grant       # 永久放行此类操作（= 动态 allow 规则，落盘）\n")
	sb.WriteString("@deny\n")
	return sb.String()
}

// AttributesSummary 把 request 属性折成人类可读的一行（数字进消息——
// 人也要看到数字，与 Part 11.4 §4.2 的白盒约定同一个方向）。
func AttributesSummary(attrs map[string]any) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, attrs[k]))
	}
	return strings.Join(parts, ", ")
}

// wait：mtime 轮询 → 收笔 → parseApproval（verdict 同款语义；两个参数
// PollInterval/Quiescence 与 discuss/escalate 的配置面同形）。
func (fa *FileApprover) wait(ctx context.Context, path, nonce string) (*Decision, Grant, bool, error) {
	ticker := time.NewTicker(fa.PollInterval)
	defer ticker.Stop()
	var modSeen time.Time
	var firstSeen time.Time
	for {
		select {
		case <-ctx.Done():
			return nil, Grant{}, false, ctx.Err()
		case <-ticker.C:
		}
		_, err := os.Stat(path)
		if err != nil {
			continue
		}
		if _, s2 := os.Stat(path); s2 == nil {
			// mtime 静默窗（人类多击键编辑的脉冲抑制）。
		}
		st, _ := os.Stat(path)
		if mt := st.ModTime(); mt != modSeen {
			modSeen = mt
			firstSeen = time.Now()
			continue
		}
		if firstSeen.IsZero() || time.Since(firstSeen) < fa.Quiescence {
			continue
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		dec, g, valid, perr := parseApproval(string(content), nonce)
		if perr != nil || !valid {
			continue // 凭据不匹配 / 编辑中间态：继续等
		}
		return dec, g, true, nil
	}
}

// parseApproval：frontmatter nonce 校验 + 首个 @ 命令行生效（人类手写
// 宽容度：未裁决 = 只写了批注 → false 继续等）。
func parseApproval(content, wantNonce string) (*Decision, Grant, bool, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return nil, Grant{}, false, fmt.Errorf("gate: approval frontmatter missing")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, Grant{}, false, fmt.Errorf("gate: approval frontmatter not closed")
	}
	nonce := ""
	for i := 1; i < end; i++ {
		k, v, ok := strings.Cut(lines[i], ":")
		if ok && strings.TrimSpace(k) == "nonce" {
			nonce = strings.TrimSpace(v)
		}
	}
	if nonce != wantNonce {
		return nil, Grant{}, false, fmt.Errorf("gate: approval nonce mismatch")
	}
	for _, ln := range lines[end+1:] {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "##") || !strings.HasPrefix(trimmed, "@") {
			continue
		}
		return directiveOf(trimmed)
	}
	return nil, Grant{}, false, nil
}

// directiveOf 是一行 @ 命令 → (Decision, Grant)。
//
// 形态支持（Part 11.3 §3.3 的三类 grant + deny）：
//
//	@grant once | @grant next N | @grant tokens N | @always-grant | @deny
//
// 解析失败（别扭数值）→ 显式 false=继续等（人类改完再至同一 nonce）。
func directiveOf(line string) (*Decision, Grant, bool, error) {
	fields := strings.Fields(strings.TrimPrefix(line, "@"))
	if len(fields) == 0 {
		return nil, Grant{}, false, nil
	}
	switch fields[0] {
	case "deny":
		return &Decision{Action: ActionDeny, Reason: "人类拒绝（审批文件）"}, Grant{}, true, nil
	case "always-grant":
		return &Decision{Action: ActionAllow,
			Reason: "人类永久放行（动态 allow 规则）"}, Grant{Mode: GrantAlways}, true, nil
	case "grant":
		if len(fields) >= 2 && fields[1] == "once" {
			return &Decision{Action: ActionAllow, Reason: "人类放行（一次）"},
				Grant{Mode: GrantOnce}, true, nil
		}
		if len(fields) >= 3 && fields[1] == "next" {
			n, aerr := strconv.Atoi(fields[2])
			if aerr == nil && n > 0 {
				return &Decision{Action: ActionAllow, Reason: fmt.Sprintf("人类放行接下来 %d 次", n)},
					Grant{Mode: GrantCount, Count: n}, true, nil
			}
			return nil, Grant{}, false, nil
		}
		if len(fields) >= 3 && fields[1] == "tokens" {
			n, terr := strconv.ParseInt(fields[2], 10, 64)
			if terr == nil && n > 0 {
				return &Decision{Action: ActionAllow, Reason: fmt.Sprintf("追加 %d token 额度", n)},
					Grant{Mode: GrantTokens, Tokens: n}, true, nil
			}
			return nil, Grant{}, false, nil
		}
	}
	return nil, Grant{}, false, nil
}
