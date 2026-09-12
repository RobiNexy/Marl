package gate

// Gate 的契约测试（Part 11.3）。
//
// 规则匹配 / 首中生效 / grant 记账 / 审批文件（FileApprover——轮询参数
// 缩小到毫秒级，契约测"静默窗口 + nonce 凭据"不测具体数值）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ruleOf(id string, kind Kind, action Action) Rule {
	return Rule{ID: id, Match: map[string]string{"kind": string(kind)}, Action: action}
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

// TestEvaluateFirstMatch：顺序匹配、首中生效 + 默认拒绝。
func TestEvaluateFirstMatch(t *testing.T) {
	m, err := NewManager(ManagerConfig{Rules: []Rule{
		ruleOf("deny-big", KindOrchestration, ActionDeny),
		ruleOf("allow-rest", KindOrchestration, ActionAllow),
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := m.Evaluate(context.Background(), &Request{Kind: KindOrchestration, AgentID: "a1"})
	if d.RuleID != "deny-big" {
		t.Fatalf("first hit: %+v", d)
	}
	// 未命中 → 默认拒绝（漏配兜底规则不会静默放行）。
	d2 := m.Evaluate(context.Background(), &Request{Kind: KindLLMCall, AgentID: "a1"})
	if d2.Action != ActionDeny || d2.RuleID != "default-deny" {
		t.Fatalf("default-deny: %+v", d2)
	}
}

// TestGrantCount：审批的 grant（count）在额度内不再打扰人类。
func TestGrantCount(t *testing.T) {
	asks := 0
	impl := &fnApp{fn: func(req *Request, rule Rule) (*Decision, Grant, error) {
		asks++
		return &Decision{Action: ActionAllow, Reason: "granted"}, Grant{Mode: GrantCount, Count: 2}, nil
	}}
	m, err := NewManager(ManagerConfig{
		Rules:    []Rule{{ID: "review-all", Match: map[string]string{"kind": "llm_call"}, Action: ActionNeedHuman}},
		Approver: impl,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 第一次：问人（asks=1）+ 记 count=2 的 grant。
	m.Evaluate(ctx, &Request{Kind: KindLLMCall, AgentID: "a1", Attributes: map[string]any{"task_call_count": 0.0}})
	// 第二次：额度内直行（asks 不变）。
	for i := 0; i < 2; i++ {
		d2 := m.Evaluate(ctx, &Request{Kind: KindLLMCall, AgentID: "a1", Attributes: map[string]any{"task_call_count": 1.0}})
		if d2.RuleID != "approver" || d2.Action != ActionAllow {
			t.Fatalf("grant path: %+v", d2)
		}
	}
	// 额度上：再问人（asks=2）。
	m.Evaluate(ctx, &Request{Kind: KindLLMCall, AgentID: "a1", Attributes: map[string]any{"task_call_count": 9.0}})
	if asks != 2 {
		t.Fatalf("approver asks = %d, want 2（额度内不再打扰）", asks)
	}
}

// fnApp 是 Approver 的函数形态替身。
type fnApp struct {
	fn func(*Request, Rule) (*Decision, Grant, error)
}

func (f *fnApp) Ask(ctx context.Context, req *Request, rule Rule) (*Decision, Grant, error) {
	return f.fn(req, rule)
}

// TestNeedHumanWithoutApproverDenies：无 Approver 时 need_human 兑现为
// 显式拒绝（"没有人类可问"不能变成永久挂起）。
func TestNeedHumanWithoutApproverDenies(t *testing.T) {
	m, _ := NewManager(ManagerConfig{Rules: []Rule{{
		ID:     "review",
		Match:  map[string]string{"kind": "llm_call"},
		Action: ActionNeedHuman,
		Reason: "基本盘组的门槛",
	}}})
	d := m.Evaluate(context.Background(), &Request{Kind: KindLLMCall, AgentID: "a"})
	if d.Action != ActionDeny {
		t.Fatalf("explicit deny: %+v", d)
	}
}

// TestNeedHumanWithoutApproverDenies 的基线确认：Audit nil 也不会崩溃。
func TestGateNoAuditOK(t *testing.T) {
	m, _ := NewManager(ManagerConfig{Rules: []Rule{
		ruleOf("allow", KindLLMCall, ActionAllow),
	}})
	d := m.Evaluate(context.Background(), &Request{Kind: KindLLMCall, AgentID: "z", Attributes: map[string]any{}})
	if d.Action != ActionAllow {
		t.Fatalf("no-attr allow: %+v", d)
	}
}

// TestFileApproverGrantNext：人类在审批文件写 @grant next 2 → Ask 返回
// count 型 grant 与 allow（Part 11.3 §3.3 的 grant 形态全走过这里）。
func TestFileApproverGrantNext(t *testing.T) {
	root := t.TempDir()
	fa, err := NewFileApprover(root, 10*time.Millisecond, 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Kind: KindLLMCall, AgentID: "a1",
		Attributes: map[string]any{"task_call_count": 21, "task_tokens": 9000.0}}
	r := Rule{ID: "review-llm-overage", Match: map[string]string{"kind": "llm_call", "task_call_count": ">=20"}, Action: ActionNeedHuman}
	type res struct {
		dec *Decision
		g   Grant
	}
	resCh := make(chan res, 1)
	go func() {
		dec, g, err := fa.Ask(context.Background(), req, r)
		if err != nil {
			t.Errorf("ask: %v", err)
			resCh <- res{}
			return
		}
		resCh <- res{dec, g}
	}()
	// 等审批文件出现（Ask 写入）→ 人类编辑（写裁决行）。
	budget := 3 * time.Second
	has := false
	var md string
	for start := time.Now(); time.Since(start) < budget; {
		entries, _ := os.ReadDir(filepath.Join(root, "approvals"))
		if len(entries) > 0 {
			md = filepath.Join(root, "approvals", entries[0].Name())
			has = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !has {
		t.Fatal("approval file not created")
	}
	// 把裁决写成 @grant next 2（frontmatter/nonce 不动）。
	body, _ := os.ReadFile(md)
	edited := strings.Replace(string(body), "@grant once", "@grant next 2", 1)
	if err := os.WriteFile(md, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resCh:
		if got.dec == nil || got.dec.Action != ActionAllow {
			t.Fatalf("decision: %+v", got.dec)
		}
		if got.g.Mode != GrantCount || got.g.Count != 2 {
			t.Fatalf("grant: %+v", got.g)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ask did not resolve")
	}
}
