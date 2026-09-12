package store

// SQLite Ledger 与 Audit 实现的测试（13.6）。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"marl/internal/types"
)

func newLedgerStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ledgerEntry(rung string, call CostCategory, usage [6]int, cost float64) *LedgerEntry {
	return &LedgerEntry{
		TaskID:  "t1",
		AgentID: "a1",
		Rung:    types.RungID(rung),
		CallType: call,
		TokenUsage: types.TokenUsage{
			PromptTokens: usage[0], CompletionTokens: usage[1], ReasoningTokens: usage[2],
			CacheWriteTokens: usage[3], CacheReadTokens: usage[4], ImageTokens: usage[5],
		},
		Cost:     cost,
		Currency: "CNY",
	}
}

func TestLedgerRecordAndSummary(t *testing.T) {
	ctx := context.Background()
	s := newLedgerStore(t)

	if err := s.Record(ctx, ledgerEntry("r0", CallMain, [6]int{100, 50, 0, 0, 80, 0}, 0.05)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.Record(ctx, ledgerEntry("r0", CallMain, [6]int{200, 60, 30, 0, 100, 0}, 0.20)); err != nil {
		t.Fatal(err)
	}
	// RecordOrchestration 强制覆盖类别。
	e := ledgerEntry("r0", CallMain, [6]int{50, 10, 0, 0, 0, 0}, 0.01)
	if err := s.RecordOrchestration(ctx, e); err != nil {
		t.Fatal(err)
	}
	if e.CallType != CallOrchestration {
		t.Fatalf("call type not overridden: %v", e.CallType)
	}
	// r1 的账。
	if err := s.Record(ctx, ledgerEntry("r1", CallMain, [6]int{300, 100, 60, 0, 150, 0}, 0.80)); err != nil {
		t.Fatal(err)
	}

	sum, err := s.TaskSummary(ctx, "t1")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.TotalTokens != int64(150+260+60+400) {
		t.Fatalf("total tokens = %d", sum.TotalTokens)
	}
	if lv := sum.ByLevel["r0"]; lv == nil || lv.Calls != 3 {
		t.Fatalf("r0 level: %+v", sum.ByLevel["r0"])
	}
	if lv := sum.ByLevel["r1"]; lv == nil || lv.Calls != 1 {
		t.Fatalf("r1 level: %+v", sum.ByLevel["r1"])
	}
	if oc := sum.ByCategory[CallOrchestration]; oc == nil || oc.Calls != 1 {
		t.Fatalf("orchestration category: %+v", sum.ByCategory[CallOrchestration])
	}
	// 总账 = 分项之和（自证不变量）。
	var levelCost, catCost float64
	var levelTokens, catTokens int64
	for _, lv := range sum.ByLevel {
		levelCost += lv.Cost
		levelTokens += lv.Tokens
	}
	for _, cv := range sum.ByCategory {
		catCost += cv.Cost
		catTokens += cv.Tokens
	}
	if diff := levelCost - sum.TotalCost; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("level cost mismatch: %v vs %v", levelCost, sum.TotalCost)
	}
	if catTokens != sum.TotalTokens {
		t.Fatalf("category token mismatch: %v vs %v", catTokens, sum.TotalTokens)
	}
	if sum.Currency != "CNY" || sum.Status != types.TaskRunning {
		t.Fatalf("summary meta: %+v", sum)
	}
	if sum.Duration <= 0 {
		t.Fatal("duration should be positive across entries")
	}
}

func TestLedgerValidation(t *testing.T) {
	ctx := context.Background()
	s := newLedgerStore(t)
	cases := []struct {
		name  string
		entry *LedgerEntry
	}{
		{"nil", nil},
		{"no task", &LedgerEntry{AgentID: "a", Rung: "r0", CallType: CallMain, Currency: "CNY"}},
		{"no agent", &LedgerEntry{TaskID: "t", Rung: "r0", CallType: CallMain, Currency: "CNY"}},
		{"no rung", &LedgerEntry{TaskID: "t", AgentID: "a", CallType: CallMain, Currency: "CNY"}},
		{"no currency", &LedgerEntry{TaskID: "t", AgentID: "a", Rung: "r0", CallType: CallMain}},
		{"zero call type", &LedgerEntry{TaskID: "t", AgentID: "a", Rung: "r0", Currency: "CNY"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Record(ctx, tc.entry)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
	t.Run("summary empty task is ErrNotFound", func(t *testing.T) {
		if _, err := s.TaskSummary(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestModelSwitchAudit(t *testing.T) {
	ctx := context.Background()
	s := newLedgerStore(t)
	ev := &ModelSwitchEvent{
		AgentID: "a1", TaskID: "t1", FromModel: "deepseek/chat", ToModel: "deepseek/v4-pro",
		Reason: "evidence", CacheHitsBefore: 768, CacheWritesBefore: 100,
	}
	if err := s.RecordModelSwitch(ctx, ev); err != nil {
		t.Fatalf("record: %v", err)
	}
	if ev.At.IsZero() {
		t.Fatal("timestamp must be backfilled")
	}
	got, err := s.QueryModelSwitch(ctx, "t1")
	if err != nil || len(got) != 1 {
		t.Fatalf("query: %v %v", got, err)
	}
	if got[0].ToModel != "deepseek/v4-pro" || got[0].CacheHitsBefore != 768 {
		t.Fatalf("roundtrip: %+v", got[0])
	}
	// 校验：缺字段拒绝。
	if err := s.RecordModelSwitch(ctx, &ModelSwitchEvent{AgentID: "a"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuditStore(t *testing.T) {
	ctx := context.Background()
	s := newLedgerStore(t)
	audit := AuditSQLite{SQLiteStore: s}
	for i, action := range []string{"spawn", "orchestrate", "model_upgrade"} {
		ev := &AuditEvent{AgentID: "a1", Action: action, Target: fmt.Sprintf("t%d", i), Payload: map[string]any{"n": i}}
		if err := audit.Append(ctx, ev); err != nil {
			t.Fatalf("append %s: %v", action, err)
		}
		if ev.Seq == 0 || ev.Timestamp.IsZero() {
			t.Fatalf("seq/timestamp not backfilled: %+v", ev)
		}
	}
	// 全量按 Seq 升序。
	all, err := audit.Query(ctx, AuditFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("query all: %v %v", all, err)
	}
	if all[0].Action != "spawn" || all[2].Action != "model_upgrade" {
		t.Fatalf("order: %+v", all)
	}
	// 按 action 过滤。
	got, err := audit.Query(ctx, AuditFilter{Action: "orchestrate"})
	if err != nil || len(got) != 1 || got[0].Target != "t1" {
		t.Fatalf("filter: %v %v", got, err)
	}
	// Payload 往返。
	if m, ok := all[1].Payload.(map[string]any); !ok || m["n"] != float64(1) {
		t.Fatalf("payload: %+v", all[1].Payload)
	}
	// 校验。
	if err := audit.Append(ctx, &AuditEvent{Action: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing agent: %v", err)
	}
	if err := audit.Append(ctx, &AuditEvent{AgentID: "a", Action: "x", Payload: make(chan int)}); err == nil {
		t.Fatal("unserializable payload must be rejected at append time")
	}
	if _, err := audit.Query(ctx, AuditFilter{Limit: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative limit: %v", err)
	}
	_ = time.Now
}
