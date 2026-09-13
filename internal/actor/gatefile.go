package actor

// 收件箱的文件编码与解析（Part 14.4 / 14.7：Gate 审批文件从
// gate.FileApprover 重构而来——"Gate 投递"特例的消除点）。
//
// 形态沿袭（与 discuss verdict / escalate 求助同一防线）：
//   - frontmatter 携带 nonce（当轮凭据——人类不编辑 frontmatter 区域，
//     Agent 伪造不出；回执必须原样带回，陈旧回放被拒——原则 4）；
//   - 解析是**回执**方向：带当轮 nonce 的 @ 命令行（gate）或正文
//     （direct）才会变成信封；纯投递形态（report / escalation / 未裁决
//     的 gate）永远不进读端。

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"marl/internal/proto"
	"marl/internal/types"
)

// newNonce 生成 8 字节 hex 当轮凭据（与 discuss / 旧 FileApprover 同源）。
func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// gateFileOf 渲染 Gate 审批文件（frontmatter + 属性摘要 + @ 命令模板）。
func gateFileOf(human ActorID, req *proto.GateRequest) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "id: %s\n", req.RequestID)
	fmt.Fprintf(&sb, "type: gate\n")
	fmt.Fprintf(&sb, "nonce: %s\n", req.Nonce)
	fmt.Fprintf(&sb, "from: %s\n", req.AgentID)
	fmt.Fprintf(&sb, "to: %s\n", human)
	fmt.Fprintf(&sb, "kind: %s\n", req.Kind)
	fmt.Fprintf(&sb, "rule: %s\n", req.RuleID)
	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb, "## 请求（%s 命中规则 %s）\n\n", req.Kind, req.RuleID)
	if req.Reason != "" {
		fmt.Fprintf(&sb, "%s\n\n", req.Reason)
	}
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
// 人也要看到数字，与 Part 11.4 §4.2 的白盒约定同一方向）。
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

// directFileOf 渲染人类直接消息文件（marl say 的文件形态）。
func directFileOf(env Envelope, msg *proto.DirectMessage) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "type: direct\n")
	fmt.Fprintf(&sb, "from: %s\n", env.From)
	fmt.Fprintf(&sb, "to: %s\n", env.To)
	if env.TraceID != "" {
		fmt.Fprintf(&sb, "trace: %s\n", env.TraceID)
	}
	sb.WriteString("---\n\n")
	sb.WriteString(msg.Text)
	if !strings.HasSuffix(msg.Text, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

// reportFileOf 渲染子 report 文件（纯投递，无回执路径）。
func reportFileOf(env Envelope, report *proto.ChildReport) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "type: report\n")
	fmt.Fprintf(&sb, "from: %s\n", env.From)
	fmt.Fprintf(&sb, "to: %s\n", env.To)
	fmt.Fprintf(&sb, "child: %s\n", report.ChildID)
	fmt.Fprintf(&sb, "status: %s\n", report.Status)
	sb.WriteString("---\n\n")
	sb.WriteString(report.Report)
	if !strings.HasSuffix(report.Report, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

// escalationFileOf 渲染 escalation 兜底投递文件（主流路径走 escalate 包
// 自己的 requests/ 信箱；这里只保证"发到人类后端的消息不静默丢失"）。
func escalationFileOf(env Envelope, req *proto.EscalationRequest) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "type: escalation\n")
	fmt.Fprintf(&sb, "from: %s\n", env.From)
	fmt.Fprintf(&sb, "to: %s\n", env.To)
	fmt.Fprintf(&sb, "trace: %s\n", req.TraceID)
	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb, "## 求助来源（from %s）\n\n%s\n\nquestion: %s\n",
		req.From, req.Reason, req.Question)
	return sb.String()
}

// inboxFile 是收件箱文件的解析中间形态（frontmatter 字段 + 正文）。
type inboxFile struct {
	typ    string
	id     string
	nonce  string
	from   string
	to     string
	trace  string
	body   string
	closed bool // frontmatter 完整（--- 成对出现）
}

// parseInboxText 做 frontmatter 剥离（不校验语义——校验在 parseInboxFile）。
func parseInboxText(content string) (inboxFile, bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	f := inboxFile{}
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return f, false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return f, false
	}
	for i := 1; i < end; i++ {
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "type":
			f.typ = v
		case "id":
			f.id = v
		case "nonce":
			f.nonce = v
		case "from":
			f.from = v
		case "to":
			f.to = v
		case "trace":
			f.trace = v
		}
	}
	f.body = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	f.closed = true
	return f, true
}

// parseInboxFile 把一份收件箱文件解析回信封（人类 → 框架方向的唯一入口）。
//
// 返回 (envelope, consumed)：consumed=false 表示"不是回执"（纯投递形态 /
// 未裁决 / 凭据不匹配）——文件留在 inbox 继续留观，**不是错误**。
//
// nonceOf(requestID, nonce) 是 gate 回执的对账谓词（Deliver 时登记的
// 当轮凭据——陈旧回放与伪造凭据被拒；原则 4 的验证面）。
//
// 凭据规则（原则 4）：gate 回执必须带当轮 nonce 且 RequestID 一致；
// direct 的 From 必须是收件箱主人本人（文件是控制面，能写它的进程就是
// 同 UID 的人类侧入口——Part 14.5 的 From 推导，文件内容里的 from 字段
// 只作交叉核对，不作授权依据）。
func parseInboxFile(content string, human ActorID, nonceOf func(requestID, nonce string) bool) (Envelope, bool) {
	f, ok := parseInboxText(content)
	if !ok {
		return Envelope{}, false
	}
	switch f.typ {
	case "gate":
		return parseGateReply(f, human, nonceOf)
	case "direct":
		if f.body == "" {
			return Envelope{}, false
		}
		return Envelope{
			From:    human, // 通道属性：控制面写者 = 同 UID 的人类（原则 4）
			To:      ActorID(f.to),
			TraceID: types.TraceID(f.trace),
			Type:    proto.MsgDirect,
			Payload: &proto.DirectMessage{Text: f.body},
		}, true
	default:
		return Envelope{}, false // report / escalation / 未知：纯投递
	}
}

// parseGateReply 解析 gate 回执（nonce 对账 + 首个 @ 命令行生效；人类
// 手写宽容度：未裁决 = 只写了批注 → false 继续等）。
func parseGateReply(f inboxFile, human ActorID, nonceOf func(requestID, nonce string) bool) (Envelope, bool) {
	if f.nonce == "" || f.id == "" || f.from == "" {
		return Envelope{}, false
	}
	// 对账：回执的 nonce 必须与 Deliver 登记的当轮凭据一致（陈旧回放
	// 在结构上不可能——凭据登记是后端的私有记忆）。
	if nonceOf != nil && !nonceOf(f.id, f.nonce) {
		return Envelope{}, false
	}
	reply := proto.GateReply{RequestID: f.id, Nonce: f.nonce}
	found := false
	for _, ln := range strings.Split(f.body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "##") || !strings.HasPrefix(trimmed, "@") {
			continue
		}
		if g, ok := gateDirectiveOf(trimmed); ok {
			reply.Action = g.action
			reply.GrantMode = g.mode
			reply.Count = g.count
			reply.Tokens = g.tokens
			reply.Reason = g.reason
			found = true
		}
		break // 首个 @ 命令行生效（与旧 FileApprover 的语义一致）
	}
	if !found {
		return Envelope{}, false
	}
	return Envelope{
		From:    human,
		To:      ActorID(f.from),
		TraceID: types.TraceID(f.id),
		Type:    proto.MsgGateReply,
		Payload: &reply,
	}, true
}

// gateDirective 是一行 @ 命令的解析产物（proto.GateReply 的散字段形态）。
type gateDirective struct {
	action string // "allow" | "deny"
	mode   string // once / count / tokens / always
	count  int
	tokens int64
	reason string
}

// gateDirectiveOf 是一行 @ 命令 → 指令（形态支持与旧 FileApprover 一致）：
//
//	@grant once | @grant next N | @grant tokens N | @always-grant | @deny
//
// 解析失败（别扭数值）→ (zero, false)（继续等人类改到合法形态）。
func gateDirectiveOf(line string) (gateDirective, bool) {
	fields := strings.Fields(strings.TrimPrefix(line, "@"))
	if len(fields) == 0 {
		return gateDirective{}, false
	}
	switch fields[0] {
	case "deny":
		return gateDirective{action: "deny", reason: "人类拒绝（审批文件）"}, true
	case "always-grant":
		return gateDirective{action: "allow", mode: "always", reason: "人类永久放行（动态 allow 规则）"}, true
	case "grant":
		if len(fields) >= 2 && fields[1] == "once" {
			return gateDirective{action: "allow", mode: "once", reason: "人类放行（一次）"}, true
		}
		if len(fields) >= 3 && fields[1] == "next" {
			n, err := strconv.Atoi(fields[2])
			if err == nil && n > 0 {
				return gateDirective{action: "allow", mode: "count", count: n,
					reason: fmt.Sprintf("人类放行接下来 %d 次", n)}, true
			}
			return gateDirective{}, false
		}
		if len(fields) >= 3 && fields[1] == "tokens" {
			n, err := strconv.ParseInt(fields[2], 10, 64)
			if err == nil && n > 0 {
				return gateDirective{action: "allow", mode: "tokens", tokens: n,
					reason: fmt.Sprintf("追加 %d token 额度", n)}, true
			}
			return gateDirective{}, false
		}
	}
	return gateDirective{}, false
}
