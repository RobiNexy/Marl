package gate

// Manager：规则表评估 + grant 记账 + 人类审批（Part 11.3 的 PDP 完整形态）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"marl/internal/store"
	"marl/internal/types"
)

// KindAll 是规则表通配（Part 11.3 §3.5 建议规则表带一条 kind="*" 的
// 兜底 allow/deny；通配规则须显式书写）。
const KindAll = Kind("*")

// Approver 是 need_human 时的裁决面（审批文件 + nonce 的实现见 file.go；
// 测试用假实现）。裁决的输出是 Grant（额度 / 永久放行规则）——Part 11.3
// §3.3"审批的输出是 grant，不是 yes"。
type Approver interface {
	Ask(ctx context.Context, req *Request, rule Rule) (*Decision, Grant, error)
}

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
	// Approver 非 nil 才支持 need_human；nil 时 need_human 兑现为 deny
	//（显式拒绝：没有人类的部署里"问人"必须显式失败，不能永久挂起）。
	Approver Approver
	// Audit 非 nil 时记决策与 grant（原则 1 副产品）。
	Audit store.AuditStore
	// Now 注入（测试确定性）。
	Now func() time.Time
	// PersistGrant 是 GrantAlways 规则的落盘钩子（控制面；nil = 不落盘，
	// 重启丢失——装配处/note 的失败面在 Approver 侧）。
	PersistGrant func(ctx context.Context, rule Rule) error
}

// Manager 是 Gate 的 PDP。
//
// 并发：Evaluate 多 Agent 并发；状态（session grant）经 mu。
type Manager struct {
	mu    sync.Mutex
	rules []Rule
	// sessionGrants 是运行中的额度（AgentID × Kind → 剩余次数/token）。
	// 装配生命周期 = 进程生命周期；"永久"级在 Approver 的落盘里。
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

// Evaluate 是 PEP 的唯一问询入口。
//
// 顺序：
//  1. session grant（先享后减——批准过的额度不需要过规则表）；
//  2. 规则表顺序匹配（首中生效）；
//  3. need_human → Approver；人类的裁决按 grant 形态兑现。
//
// 决策必过审计（AgentID/Kind/rule_id/grant 形态）。
func (m *Manager) Evaluate(ctx context.Context, req *Request) Decision {
	if req == nil || !req.Kind.Valid() {
		return Decision{Action: ActionDeny, RuleID: "engine", Reason: "gate: request kind 未分类（未分类即拒绝）"}
	}
	if g, allowed := m.consumeGrant(req); allowed {
		d := Decision{Action: ActionAllow, RuleID: g.addedByRule, Reason: "grant 额度内（不再打扰人类）"}
		m.auditev(ctx, req, d, nil)
		return d
	}
	for _, r := range m.rules {
		if !r.Matches(req) {
			continue
		}
		if r.Action == ActionNeedHuman {
			if m.cfg.Approver == nil {
				d := Decision{Action: ActionDeny, RuleID: r.ID,
					Reason: ruleReason(r, "need_human 但未装配人类裁决面（Approver）——显式拒绝而非永久挂起")}
				m.auditev(ctx, req, d, nil)
				return d
			}
			d2, g, err := m.cfg.Approver.Ask(ctx, req, r)
			if err != nil {
				// 审批通道故障是框架级错误（上抛给 PEP，不算"业务拒绝"）。
				dd := Decision{Action: ActionDeny, RuleID: r.ID,
					Reason: fmt.Sprintf("gate: approver failure: %v", err)}
				m.auditev(ctx, req, dd, nil)
				return dd
			}
			// Approver 的裁决：allow（附 grant）或 deny。grant 记账当面
			// 由 Approver 返回值承载；Always 模式在 Approver 内部落动态
			// 规则（AddGrant 的调用)——manager 只负责审计与透传。
			d2.RuleID = r.ID
			// grant 记账（Part 11.3 §3.3）：allow + 非空 grant → 落账
			// （once/count/tokens 进 session；always 插动态规则 + 落盘钩子）。
			if g.Mode != "" && d2.Action == ActionAllow {
				if gerr := m.AddGrant(ctx, req, g); gerr != nil {
					fmt.Printf("marl: gate grant record failed: %v\n", gerr)
				}
			}
			m.auditev(ctx, req, *d2, &g)
			return *d2
		}
		d := Decision{Action: r.Action, RuleID: r.ID, Reason: r.Reason}
		m.auditev(ctx, req, d, nil)
		return d
	}
	// 规则表全部未命中：拒绝（默认拒绝是"未分类"操作面最诚实的形态——
	// 规则表里永远该有一条 * 兜底，漏了也不会静默放行）。
	d := Decision{Action: ActionDeny, RuleID: "default-deny",
		Reason: "没有任何规则覆盖本操作（规则表应有 kind=\"*\" 的兜底规则——漏配仍按拒绝处理）"}
	m.auditev(ctx, req, d, nil)
	return d
}

// consumeGrant 报告额度面是否允许。（Approver 的授权形态走独立通道 —
// GrantCount / GrantTokens 的快速通道在本判据里显式对账。）
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

// AddGrant 是 Approver 的授权记账入口（file.go 的裁决载体；session 级）。
//
// GrantAlways 的语义：往规则表顶部插入一条动态 allow 规则（+ 可选落盘）。
func (m *Manager) AddGrant(ctx context.Context, req *Request, g Grant) error {
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
		if m.cfg.PersistGrant != nil {
			if err := m.cfg.PersistGrant(ctx, rule); err != nil {
				return fmt.Errorf("gate: persist grant rule: %w", err)
			}
		}
	default:
		return fmt.Errorf("gate: unknown grant mode %q", g.Mode)
	}
	m.auditev(ctx, req, Decision{Action: ActionAllow, RuleID: "grant", Reason: g.Reason}, &g)
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
// 审计痕迹，写失败不改变决策）。
func (m *Manager) auditev(ctx context.Context, req *Request, d Decision, g *Grant) {
	if m.cfg.Audit == nil {
		return
	}
	payload := map[string]any{"kind": string(req.Kind), "rule": d.RuleID,
		"action": string(d.Action), "reason": d.Reason}
	if g != nil {
		payload["grant"] = string(g.Mode)
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

// Decider 是消费侧的窄面（Agent 对 PDP 的需求：非阻塞问询 + 挂起恢复
// 时的全套评估）。
type Decider interface {
	// Decide 非阻塞（need_human 时不触发 Approver——agent 先回填
	// GATE_PENDING 给模型、挂起）。
	Decide(ctx context.Context, req *Request) Decision
	// Evaluate 全套（挂起恢复路径： وذلك Approver 在这里启动）。
	Evaluate(ctx context.Context, req *Request) Decision
}

// Decide 是 PEP 的**非阻塞**问询：与 Evaluate 同一枚规则表，但命中
// need_human 时不触发 Approver——调用方把决策回填到模型（GATE_PENDING）
// 并挂起（Blocked(AwaitingGate)），真正的"问人"发生在挂起恢复路径的
// Evaluate（FileApprover 的文件轮询在那里运行）。
//
// 与 Evaluate 的分工：Evaluate 也给 PEP 用（当调用方愿意同步等裁决时）；
// 能"先回模型再恢复"的执行点（llm_call / 编排）走 Decide——避免在任何
// tool_call 执行的中途阻塞 eventLoop。
func (m *Manager) Decide(ctx context.Context, req *Request) Decision {
	if req == nil || !req.Kind.Valid() {
		return Decision{Action: ActionDeny, RuleID: "engine", Reason: "gate: request kind 未分类"}
	}
	if g, allowed := m.consumeGrant(req); allowed {
		d := Decision{Action: ActionAllow, RuleID: g.addedByRule, Reason: "grant 额度内"}
		m.auditev(ctx, req, d, nil)
		return d
	}
	for _, r := range m.rules {
		if !r.Matches(req) {
			continue
		}
		if r.Action == ActionNeedHuman && m.cfg.Approver != nil {
			// 非阻塞面：need_human 由调用方决定何时挂起（先回填 GATE_PENDING）。
			d := Decision{Action: ActionNeedHuman, RuleID: r.ID, Reason: ruleReason(r, "需要人类裁决")}
			m.auditev(ctx, req, d, nil)
			return d
		}
		d := Decision{Action: r.Action, RuleID: r.ID, Reason: r.Reason}
		m.auditev(ctx, req, d, nil)
		return d
	}
	d := Decision{Action: ActionDeny, RuleID: "default-deny",
		Reason: "没有任何规则覆盖本操作（规则表应有 kind=\"*\" 的兜底规则——漏配仍按拒绝处理）"}
	m.auditev(ctx, req, d, nil)
	return d
}
