package gate

// Gate 的契约测试（Part 11.3 + Part 14.7 的重新定性）。
//
// 规则匹配 / 首中生效 / grant 记账 / ResolveGate 的裁决兑现。审批文件的
// 编码与解析（往返、nonce、静默窗）在 actor 包测试——Gate 投递的特例
// 已消除，Manager 只产出决策与兑现 grant（"审批者必须是规则不是 Actor"）。

import (
	"context"
	"testing"

	"github.com/RobiNexy/Marl/internal/proto"
)

func ruleOf(id string, kind Kind, action Action) Rule {
	return Rule{ID: id, Match: map[string]string{"kind": string(kind)}, Action: action}
}

// gateReplyOf 构造 GateReply 载荷（allow/deny 两面的测试形态）。
func gateReplyOf(action, mode string, count int, tokens int64, reason string) *proto.GateReply {
	return &proto.GateReply{Action: action, GrantMode: mode, Count: count, Tokens: tokens, Reason: reason}
}

// TestRuleMatch：字符串/布尔/数值前缀比较（"<10" / ">=20"）。
func TestRuleMatch(t *testing.T) {
	r := Rule{ID: "r", Match: map[string]string{"kind": "llm_call", "task_call_count": ">=20"}, Action: ActionNeedHuman}
	over := &Request{Kind: KindLLMCall, Attributes: map[string]any{"task_call_count": float64(25)}}
	if !r.Matches(over) {
		t.Fatal(">=20 should match 25")
	}
	tooSoon := &Request{Kind: KindLLMCall, Attributes: map[string]any{"task_call_count": float64(5)}}
	if r.Matches(tooSoon) {
		t.Fatal(">=20 must not match 5")
	}
	pct := Rule{ID: "p", Match: map[string]string{"kind": "orchestration", "cache_destroyed_pct": "<10"}, Action: ActionAllow}
	if !pct.Matches(&Request{Kind: KindOrchestration, Attributes: map[string]any{"cache_destroyed_pct": 3.2}}) {
		t.Fatal("<10 should match 3.2")
	}
	if pct.Matches(&Request{Kind: KindOrchestration, Attributes: map[string]any{"cache_destroyed_pct": 43.1}}) {
		t.Fatal("<10 must not match 43.1")
	}
	missing := &Request{Kind: KindLLMCall, Attributes: map[string]any{}}
	if r.Matches(missing) {
		t.Fatal("missing attribute must not match (не default-guess)")
	}
}

// TestDecideFirstMatch：顺序匹配、首中生效 + 默认拒绝。
func TestDecideFirstMatch(t *testing.T) {
	m, err := NewManager(ManagerConfig{Rules: []Rule{
		ruleOf("deny-big", KindOrchestration, ActionDeny),
		ruleOf("allow-rest", KindOrchestration, ActionAllow),
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := m.Decide(context.Background(), &Request{Kind: KindOrchestration, AgentID: "a1"})
	if d.RuleID != "deny-big" {
		t.Fatalf("first hit: %+v", d)
	}
	// 未命中 → 默认拒绝（漏配兜底规则不会静默放行）。
	d2 := m.Decide(context.Background(), &Request{Kind: KindLLMCall, AgentID: "a1"})
	if d2.Action != ActionDeny || d2.RuleID != "default-deny" {
		t.Fatalf("default-deny: %+v", d2)
	}
}

// TestNeedHumanDecision：need_human 是**决策**不是阻塞——PEP 产出后
// 由调用方折算成 MsgGateRequest（Part 14.7 的"审批者必须是规则"）。
func TestNeedHumanDecision(t *testing.T) {
	m, _ := NewManager(ManagerConfig{Rules: []Rule{{
		ID:     "review",
		Match:  map[string]string{"kind": "llm_call"},
		Action: ActionNeedHuman,
		Reason: "基本盘组的门槛",
	}}})
	d := m.Decide(context.Background(), &Request{Kind: KindLLMCall, AgentID: "a"})
	if d.Action != ActionNeedHuman || d.RuleID != "review" {
		t.Fatalf("need_human decision: %+v", d)
	}
}

// TestResolveGateGrantCount：人类的 count 型裁决记账后，额度内直行
// （不再产生 need_human）；额度耗尽回到 need_human。
func TestResolveGateGrantCount(t *testing.T) {
	m, err := NewManager(ManagerConfig{Rules: []Rule{{
		ID: "review-all", Match: map[string]string{"kind": "llm_call"}, Action: ActionNeedHuman,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := &Request{Kind: KindLLMCall, AgentID: "a1", Attributes: map[string]any{"task_call_count": 0.0}}
	if d := m.Decide(ctx, req); d.Action != ActionNeedHuman {
		t.Fatalf("pre-grant: %+v", d)
	}
	// 人类批 "接下来 2 次" → ResolveGate 兑现。
	dec, rerr := m.ResolveGate(ctx, req, gateReplyOf("allow", "count", 2, 0, ""), "human:u1000")
	if rerr != nil || dec.Action != ActionAllow {
		t.Fatalf("resolve: %v %+v", rerr, dec)
	}
	// 两次额度内直行（need_human 不再出现）。
	for i := 0; i < 2; i++ {
		d2 := m.Decide(ctx, req)
		if d2.Action != ActionAllow || d2.RuleID != "approver" {
			t.Fatalf("grant path %d: %+v", i, d2)
		}
	}
	// 额度上：回到 need_human（"不再打扰"的边界要闭环）。
	if d3 := m.Decide(ctx, req); d3.Action != ActionNeedHuman {
		t.Fatalf("over-grant: %+v", d3)
	}
}

// TestResolveGateAlwaysPersisted + GrantStore：永久放行 → 动态 allow 规则
// 插顶 + grants/ 落盘；重启（新 Manager + LoadGrants 回插）后仍然直行。
func TestResolveGateAlwaysPersisted(t *testing.T) {
	ctx := context.Background()
	controlRoot := t.TempDir()
	gs, err := NewGrantStore(controlRoot)
	if err != nil {
		t.Fatal(err)
	}
	rules := []Rule{{ID: "review-all", Match: map[string]string{"kind": "llm_call"}, Action: ActionNeedHuman}}
	m, err := NewManager(ManagerConfig{Rules: rules, Grants: gs})
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Kind: KindLLMCall, AgentID: "a1", Attributes: map[string]any{}}
	dec, rerr := m.ResolveGate(ctx, req, gateReplyOf("allow", "always", 0, 0, ""), "human:u1000")
	if rerr != nil || dec.Action != ActionAllow {
		t.Fatalf("resolve: %v %+v", rerr, dec)
	}
	if d := m.Decide(ctx, req); d.Action != ActionAllow {
		t.Fatalf("always in-process: %+v", d)
	}
	// 重启形态：新 Manager，LoadGrants 回插动态规则。
	saved, lerr := gs.LoadGrants(ctx)
	if lerr != nil || len(saved) != 1 {
		t.Fatalf("load grants: %v %d", lerr, len(saved))
	}
	if saved[0].GrantedBy != "human:u1000" {
		t.Fatalf("granted_by 链断裂: %+v", saved[0])
	}
	if saved[0].Rule.Match["kind"] != string(KindLLMCall) || saved[0].Rule.Action != ActionAllow {
		t.Fatalf("persisted rule shape: %+v", saved[0].Rule)
	}
	m2, err := NewManager(ManagerConfig{Rules: rules, Grants: gs})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range saved {
		if err := m2.AddGrant(ctx, req, Grant{Mode: GrantAlways, Reason: g.Rule.Reason}, g.GrantedBy); err != nil {
			t.Fatal(err)
		}
	}
	if d := m2.Decide(ctx, req); d.Action != ActionAllow {
		t.Fatalf("always after restart: %+v", d)
	}
}

// TestResolveGateDeny：deny 的兑现（原因透传，不记账）。
func TestResolveGateDeny(t *testing.T) {
	m, _ := NewManager(ManagerConfig{Rules: []Rule{
		ruleOf("review", KindLLMCall, ActionNeedHuman),
	}})
	req := &Request{Kind: KindLLMCall, AgentID: "a", Attributes: map[string]any{}}
	dec, err := m.ResolveGate(context.Background(), req,
		gateReplyOf("deny", "", 0, 0, "别烧钱了"), "human:u1")
	if err != nil || dec.Action != ActionDeny || dec.Reason != "别烧钱了" {
		t.Fatalf("deny resolve: %v %+v", err, dec)
	}
	// deny 不记账：再问还是 need_human。
	if d := m.Decide(context.Background(), req); d.Action != ActionNeedHuman {
		t.Fatalf("deny must not record grant: %+v", d)
	}
}

// TestResolveGateUnknownAction：未知 action 显式报错（协议 bug 可见）。
func TestResolveGateUnknownAction(t *testing.T) {
	m, _ := NewManager(ManagerConfig{Rules: []Rule{ruleOf("r", KindLLMCall, ActionNeedHuman)}})
	if _, err := m.ResolveGate(context.Background(),
		&Request{Kind: KindLLMCall, AgentID: "a", Attributes: map[string]any{}},
		gateReplyOf("maybe", "", 0, 0, ""), ""); err == nil {
		t.Fatal("unknown action must error")
	}
}
