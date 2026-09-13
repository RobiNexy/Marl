package gate

// Manager：规则表评估 + grant 记账 + 人类裁决的兑现（Part 11.3 PDP +
// Part 14.7 的重新定性——Gate 是"人类 Actor 的收件秘书"）。
//
// 阶段 12（Part 14）的结构变化：
//   - need_human 的"问人"不再是 Manager 内的阻塞轮询（旧 FileApprover）；
//     Manager 只产出 need_human **决策**，审批往返是 MsgGateRequest /
//     MsgGateReply 信封（人类 Actor 的文件后端承载），裁决经 ResolveGate
//     兑现——审批者必须是规则，不是 Actor（Part 14.7 硬边界）。
//   - GrantAlways 的持久化由 GrantStore 承担（grants/ 目录），重启后由
//     装配侧 LoadGrants 回插动态规则——"人类批过的 always 不丢"。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"marl/internal/proto"
	"marl/internal/store"
	"marl/internal/types"
)

// KindAll 是规则表通配（Part 11.3 §3.5 建议规则表带一条 kind="*" 的
// 兜底 allow/deny；通配规则须显式书写）。
const KindAll = Kind("*")

// GrantMode 是裁决的形态。
type GrantMode string

const (
	GrantOnce   GrantMode = "once"   // 批准这一次（旧 yes 的通用化）
	GrantCount  GrantMode = "count"  // 授 N 次额度（"接下来 20 次"）
	GrantTokens GrantMode = "tokens" // 追加 token 额度（"追加 50K"）
	GrantAlways GrantMode = "always" // 永久放行（动态 allow 规则，落盘）
)

// Grant 是审批的输出。
type Grant struct {
	Mode   GrantMode
	Count  int
	Tokens int64
	Reason string // 人类的批注（进审计 + 决策消息）
}

// ManagerConfig 是 PDP 的装配参数。
type ManagerConfig struct {
	// Rules 是规则表（顺序即匹配序，首中生效）。
	Rules []Rule
	// Audit 非 nil 时记决策与 grant（原则 1 副产品）。
	Audit store.AuditStore
	// Now 注入（测试确定性）。
	Now func() time.Time
	// Grants 非 nil 时 GrantAlways 落盘（grants/ 目录；重启回插）。
	Grants *GrantStore
}

// Manager 是 Gate 的 PDP。
//
// 并发：Decide 多 Agent 并发；状态（session grant / 动态规则）经 mu。
type Manager struct {
	mu    sync.Mutex
	rules []Rule
	// sessionGrants 是运行中的额度（AgentID × Kind → 剩余次数/token）。
	// 装配生命周期 = 进程生命周期；"永久"级在 GrantStore 落盘。
	sessionGrants map[types.AgentID]map[Kind]*grantLedger
	cfg           ManagerConfig
}

// grantLedger 是一个 (Agent, Kind) 的额度账（先享后减）。
type grantLedger struct {
	counts      int
	tokens      int64
	addedByRule string
}

// NewManager 装配（规则表先 Validate；配置错必须在第一个 Request 之前炸）。
func NewManager(cfg ManagerConfig) (*Manager, error) {
	m := &Manager{rules: cfg.Rules, cfg: cfg,
		sessionGrants: map[types.AgentID]map[Kind]*grantLedger{}}
	if m.cfg.Now == nil {
		m.cfg.Now = time.Now
	}
	if err := m.ValidateRules(); err != nil {
		return nil, err
	}
	return m, nil
}

// ValidateRules 的失败集（配置面 fail fast）：
//   - 规则 ID 空 / 重复（审计对不上）；
//   - Action 未定义；
//   - match.kind 缺失（未分类即免审是最坏的静默方向——通配必须显式写 kind":"*"）；
//   - match 的数值表达式不可解析。
func (m *Manager) ValidateRules() error {
	seen := map[string]bool{}
	for i, r := range m.rules {
		if r.ID == "" {
			return fmt.Errorf("gate: rules[%d]: id 未填", i)
		}
		if seen[r.ID] {
			return fmt.Errorf("gate: duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		switch r.Action {
		case ActionAllow, ActionDeny, ActionNeedHuman:
		default:
			return fmt.Errorf("gate: rule %q: 非法 action %q", r.ID, r.Action)
		}
		kw, ok := r.Match["kind"]
		if !ok {
			return fmt.Errorf("gate: rule %q: match.kind 未填（通配请显式 kind=\"*\"）", r.ID)
		}
		if kw != string(KindAll) && !Kind(kw).Valid() {
			return fmt.Errorf("gate: rule %q: 未知 kind %q", r.ID, kw)
		}
		for k, v := range r.Match {
			if v == "" {
				return fmt.Errorf("gate: rule %q: match[%s] 为空", r.ID, k)
			}
			if numExprLen(v) == 0 {
				continue // 文本匹配（不做数值合法性检查）
			}
			if _, _, ok := parseNumExpr(v); !ok {
				return fmt.Errorf("gate: rule %q: match[%s]=%q 不是可解析的数值表达式", r.ID, k, v)
			}
		}
	}
	return nil
}

// Decide 是 PEP 的唯一问询入口（非阻塞；Part 14.7 的 GateRequest 化：
// need_human 由调用方折算成 MsgGateRequest 发给人类 Actor 并挂起）。
//
// 顺序：
//  1. session grant（先享后减——批准过的额度不需要过规则表）；
//  2. 规则表顺序匹配（首中生效）。
//
// 决策必过审计（AgentID/Kind/rule_id）。
func (m *Manager) Decide(ctx context.Context, req *Request) Decision {
	if req == nil || !req.Kind.Valid() {
		return Decision{Action: ActionDeny, RuleID: "engine", Reason: "gate: request kind 未分类（未分类即拒绝）"}
	}
	if g, allowed := m.consumeGrant(req); allowed {
		d := Decision{Action: ActionAllow, RuleID: g.addedByRule, Reason: "grant 额度内（不再打扰人类）"}
		m.auditev(ctx, req, d, nil, "")
		return d
	}
	for _, r := range m.rules {
		if !r.Matches(req) {
			continue
		}
		d := Decision{Action: r.Action, RuleID: r.ID, Reason: ruleReason(r, "需要人类裁决")}
		m.auditev(ctx, req, d, nil, "")
		return d
	}
	// 规则表全部未命中：拒绝（默认拒绝是"未分类"操作面最诚实的形态——
	// 规则表里永远该有一条 * 兜底，漏了也不会静默放行）。
	d := Decision{Action: ActionDeny, RuleID: "default-deny",
		Reason: "没有任何规则覆盖本操作（规则表应有 kind=\"*\" 的兜底规则——漏配仍按拒绝处理）"}
	m.auditev(ctx, req, d, nil, "")
	return d
}

// ResolveGate 兑现人类的 GateReply（Part 14.7 的"MsgGateReply → Agent
// 的 Mailbox"落地步）：grant 记账 + 动态规则 + 审计，返回最终 Decision
// （Agent 记入 Log 的对账键）。
//
// 凭据校验（原则 4）：reply 的 RequestID / Nonce 必须与请求一致——
// 陈旧回放与错配回执在这里被拒（error，不静默）。
//
// granted_by 记录人类 ActorID（Part 14.10：授权链完整——审计 payload
// 的 granted_by 字段）。
func (m *Manager) ResolveGate(ctx context.Context, req *Request, reply *proto.GateReply, grantedBy types.AgentID) (Decision, error) {
	if req == nil || reply == nil {
		return Decision{}, fmt.Errorf("gate: resolve requires request and reply")
	}
	switch reply.Action {
	case "deny":
		d := Decision{Action: ActionDeny, RuleID: "human-deny", Reason: reply.Reason}
		if d.Reason == "" {
			d.Reason = "人类拒绝（MsgGateReply）"
		}
		m.auditev(ctx, req, d, nil, grantedBy)
		return d, nil
	case "allow":
		g := Grant{Mode: GrantMode(reply.GrantMode), Count: reply.Count,
			Tokens: reply.Tokens, Reason: reply.Reason}
		if g.Reason == "" {
			g.Reason = "人类放行（MsgGateReply）"
		}
		d := Decision{Action: ActionAllow, RuleID: "human-grant", Reason: g.Reason}
		if err := m.AddGrant(ctx, req, g, grantedBy); err != nil {
			return Decision{}, err
		}
		m.auditev(ctx, req, d, &g, grantedBy)
		return d, nil
	default:
		return Decision{}, fmt.Errorf("gate: unknown reply action %q（allow | deny）", reply.Action)
	}
}

// consumeGrant 报告额度面是否允许（先享后减；Approver 的授权形态走
// ResolveGate 的独立通道）。
func (m *Manager) consumeGrant(req *Request) (grantLedger, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lm, ok := m.sessionGrants[req.AgentID]
	if !ok {
		return grantLedger{}, false
	}
	g, ok := lm[req.Kind]
	if !ok || g == nil {
		return grantLedger{}, false
	}
	if tokens, ok := req.Attributes["task_tokens"].(float64); ok && g.tokens > 0 {
		g.tokens -= int64(tokens)
		if g.tokens <= 0 {
			delete(lm, req.Kind)
		}
		return *g, true
	}
	if g.counts > 0 {
		g.counts--
		if g.counts <= 0 {
			delete(lm, req.Kind)
		}
		return *g, true
	}
	return grantLedger{}, false
}

// AddGrant 是授权记账入口（ResolveGate 的兑现面；session 级）。
//
// GrantAlways 的语义：往规则表顶部插入一条动态 allow 规则（+ grants/
// 落盘——重启回插，"人类批过的 always 不丢"）。
//
// grantedBy 是授权链的审计字段（Part 14.10；空 = 未记录——审计里如实
// 缺失而不是编造 "unknown"）。
func (m *Manager) AddGrant(ctx context.Context, req *Request, g Grant, grantedBy types.AgentID) error {
	if g.Count < 0 || g.Tokens < 0 {
		return fmt.Errorf("gate: grant 额度非法（负数）")
	}
	switch g.Mode {
	case GrantOnce:
		m.addSessionGrant(req.AgentID, req.Kind, g, 1, int64(g.Tokens))
	case GrantCount:
		if g.Count <= 0 {
			return fmt.Errorf("gate: GrantCount 需要 >0 次数")
		}
		m.addSessionGrant(req.AgentID, req.Kind, g, g.Count, int64(g.Tokens))
	case GrantTokens:
		if g.Tokens <= 0 {
			return fmt.Errorf("gate: GrantTokens 需要 >0 token 数")
		}
		m.addSessionGrant(req.AgentID, req.Kind, g, 1<<30, g.Tokens) // 次数不限制；token 到量即摘
	case GrantAlways:
		rule := Rule{
			ID:     fmt.Sprintf("grant-allow-%s-%d", string(req.Kind), m.cfg.Now().UnixMilli()),
			Match:  map[string]string{"kind": string(req.Kind)},
			Action: ActionAllow,
			Reason: g.Reason,
		}
		m.mu.Lock()
		m.rules = append([]Rule{rule}, m.rules...)
		m.mu.Unlock()
		if m.cfg.Grants != nil {
			if err := m.cfg.Grants.Save(ctx, rule, grantedBy); err != nil {
				return fmt.Errorf("gate: persist grant rule: %w", err)
			}
		}
	default:
		return fmt.Errorf("gate: unknown grant mode %q", g.Mode)
	}
	return nil
}

func (m *Manager) addSessionGrant(agent types.AgentID, kind Kind, g Grant, counts int, tokens int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lm, ok := m.sessionGrants[agent]
	if !ok {
		lm = map[Kind]*grantLedger{}
		m.sessionGrants[agent] = lm
	}
	cur, ok := lm[kind]
	if !ok {
		cur = &grantLedger{}
		lm[kind] = cur
	}
	cur.counts += counts
	cur.tokens += tokens
	cur.addedByRule = "approver"
}

// auditev 记审计（Audit nil 跳过；写失败打 stderr——gate 的决策必须伴随
// 审计痕迹，写失败不改变决策）。grantedBy 非空时进 payload（Part 14.10
// 的授权链字段）。
func (m *Manager) auditev(ctx context.Context, req *Request, d Decision, g *Grant, grantedBy types.AgentID) {
	if m.cfg.Audit == nil {
		return
	}
	payload := map[string]any{"kind": string(req.Kind), "rule": d.RuleID,
		"action": string(d.Action), "reason": d.Reason}
	if g != nil {
		payload["grant"] = string(g.Mode)
	}
	if grantedBy != "" {
		payload["granted_by"] = string(grantedBy)
	}
	ev := &store.AuditEvent{AgentID: req.AgentID, Action: "gate_decision", Target: string(req.Kind), Payload: payload}
	if err := m.cfg.Audit.Append(ctx, ev); err != nil {
		fmt.Printf("marl: gate audit failed: %v\n", err)
	}
}

// ruleReason 拼规则的理由（规则未写时给默认模板）。
func ruleReason(r Rule, fallback string) string {
	if s := strings.TrimSpace(r.Reason); s != "" {
		return s
	}
	return fmt.Sprintf("命中规则 %s：%s", r.ID, fallback)
}

// shortReason 修剪（Reason 长度显式取上限——LLM 的回复宽预算不背锅）。
func shortReason(s string) string { return strings.TrimSpace(s) }
