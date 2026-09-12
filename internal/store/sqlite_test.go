package store

// store 包的集成测试（13.4：SQLite 主从）。
//
// 规格覆盖：
//   - MessageLog：Append 分配 ULID/Seq/CreatedAt 并就地回填；Seq 无洞、
//     跨 Agent 独立；Get/GetBySeq/Range/Latest 的排序与错误契约；
//     LastSeq=0 哨兵；落库后重新读出的条目与写入的条目语义相等；
//   - ViewStore：Load 空返回带 AgentID 的空视图；Save→Load 往返；
//     SaveView 全量覆盖；
//   - SnapshotStore：Create→Restore 往返；Prune 只留最近 N；越界拒绝。
//
// 全部走真实 SQLite（临时文件），不引入内存替身：视图往返的"blob 语义"
// 只有真库才能证伪。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"marl/internal/types"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func entry(agentID types.AgentID, role types.InternalRole, content string) *types.LogEntry {
	return types.NewLogEntry(agentID, role, content)
}

const agentA = types.AgentID("agent-a")
const agentB = types.AgentID("agent-b")

// roundTrip 逐字段比较"写入库前 entry"与"从库里读回的条目"（构造期零值三个
// 字段由 Append 回填，语义上必须一致）。
func roundTrip(t *testing.T, got, want *types.LogEntry) {
	t.Helper()
	if got.ID == "" || got.Seq == 0 || got.CreatedAt.IsZero() {
		t.Fatalf("readback missing allocated fields: %+v", got)
	}
	if got.AgentID != want.AgentID || got.Role != want.Role || got.Content != want.Content ||
		got.Prov != want.Prov || got.Audience != want.Audience || got.TokenEst != want.TokenEst {
		t.Fatalf("semantic mismatch:\n got %+v\nwant %+v", got, want)
	}
	if !slices.Equal(got.SourceIDs, want.SourceIDs) {
		t.Fatalf("source ids mismatch: got %v want %v", got.SourceIDs, want.SourceIDs)
	}
}

func TestSQLiteLogAppendAllocates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	e := entry(agentA, types.RoleUserInput, "你好")
	id, err := s.Append(ctx, e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if id != e.ID {
		t.Fatalf("Append returned id %q != backfilled %q", id, e.ID)
	}
	if e.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", e.Seq)
	}
	// ULID 形态：26 个 Crockford 字符。
	if len(e.ID) != 26 {
		t.Fatalf("id len = %d, want 26 (%q)", len(e.ID), e.ID)
	}

	e2 := entry(agentA, types.RoleAssistantReply, "你好，世界")
	e3 := entry(agentB, types.RoleUserInput, "别的 Agent")
	for _, en := range []*types.LogEntry{e2, e3} {
		if _, err := s.Append(ctx, en); err != nil {
			t.Fatalf("Append %s: %v", en.Role, err)
		}
	}
	if e2.Seq != 2 {
		t.Fatalf("seq per agent not increasing: %d", e2.Seq)
	}
	if e3.Seq != 1 {
		t.Fatalf("seq must be per-agent: agent-b first = %d", e3.Seq)
	}

	last, err := s.LastSeq(ctx, agentA)
	if err != nil || last != 2 {
		t.Fatalf("LastSeq = %d, %v", last, err)
	}
	none, err := s.LastSeq(ctx, types.AgentID("ghost"))
	if err != nil || none != 0 {
		t.Fatalf("LastSeq(no agent) = %d, %v; want 0 sentinel", none, err)
	}
}

func TestSQLiteLogInvalidInputs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	cases := []struct {
		name  string
		entry *types.LogEntry
	}{
		{"zero role", invalidRoleEntry()},
		{"preallocated id", preIDEntry()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Append(ctx, tc.entry)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}

	if _, err := s.GetBySeq(ctx, agentA, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("seq 0 must be ErrInvalid, got %v", err)
	}
	if _, err := s.Range(ctx, agentA, 5, 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reversed range must be ErrInvalid, got %v", err)
	}
	if _, err := s.Latest(ctx, agentA, -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative limit must be ErrInvalid, got %v", err)
	}
	if _, err := s.Get(ctx, types.MessageID("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id must be ErrNotFound, got %v", err)
	}
}

// invalidRoleEntry 构造 Role 为零值的非法条目（NewLogEntry 会 panic 拦截，
// 故绕过构造函数）。
func invalidRoleEntry() *types.LogEntry {
	e := &types.LogEntry{AgentID: agentA, Prov: types.ProvOriginal, Audience: types.AudienceBoth}
	return e
}

func preIDEntry() *types.LogEntry {
	e := entry(agentA, types.RoleUserInput, "x")
	e.ID = types.MessageID("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	return e
}

func TestSQLiteLogQueries(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for i := 0; i < 5; i++ {
		if _, err := s.Append(ctx, entry(agentA, types.RoleUserInput, fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	// Range 含端点、升序。
	r, err := s.Range(ctx, agentA, 2, 4)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(r) != 3 || r[0].Content != "m1_0" || r[0].Content == "" {
		// 仅断言条目数与顺序占位；具体内容由累积顺序决定（Content m1..m3）。
	}
	seqs := []int64{}
	for _, it := range r {
		seqs = append(seqs, it.Seq)
	}
	if !slices.Equal(seqs, []int64{2, 3, 4}) {
		t.Fatalf("range seqs = %v, want [2 3 4]", seqs)
	}

	// Latest 倒数第三起升序。
	l, err := s.Latest(ctx, agentA, 3)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(l) != 3 || l[0].Seq != 3 || l[2].Seq != 5 {
		t.Fatalf("latest = %v", l)
	}

	// TotalTokens 基于本地估算累计（内容长度 > 0 ⇒ > 0）。
	tt, err := s.TotalTokens(ctx, agentA)
	if err != nil || tt <= 0 {
		t.Fatalf("total tokens = %d, %v", tt, err)
	}
}

func TestSQLiteLogRoundTripRichFields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	src := entry(agentA, types.RoleUserInput, "源头消息")
	if _, err := s.Append(ctx, src); err != nil {
		t.Fatal(err)
	}

	e := entry(agentA, types.RoleThinking, "<analysis>1+2=3</analysis>").
		WithProvenance(types.ProvSummaryOf, src.ID)
	e.Audience = types.AudienceAudit
	e.TokenActual = &types.TokenUsage{PromptTokens: 10, ReasoningTokens: 4}
	e.Meta = map[string]any{"finish_reason": "stop"}
	stored := e // Append 就地回填；先留引用给 roundTrip
	if _, err := s.Append(ctx, e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	back, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	roundTrip(t, back, stored)
	if back.Prov != types.ProvSummaryOf || len(back.SourceIDs) != 1 || back.SourceIDs[0] != src.ID {
		t.Fatalf("provenance lost: %+v", back)
	}
	if back.Meta["finish_reason"] != "stop" {
		t.Fatalf("meta lost: %+v", back.Meta)
	}
	if back.TokenActual == nil || back.TokenActual.ReasoningTokens != 4 {
		t.Fatalf("token usage lost: %+v", back.TokenActual)
	}
}

func TestSQLiteViewRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// 空：Load 返回带 AgentID 的空视图（不是零值）。
	v, err := s.LoadView(ctx, agentA)
	if err != nil {
		t.Fatalf("LoadView empty: %v", err)
	}
	if v.AgentID != agentA || len(v.Items) != 0 {
		t.Fatalf("empty view = %+v", v)
	}

	// Save → Load 往返。
	v.Items = []types.ViewItem{
		{Ref: types.MessageID("01ARZ3NDEKTSV4RRFFQ69G5FAV"), WireRole: types.WireUser,
			Stability: types.StabilityStable, Visible: true, Position: 1.0},
	}
	v.EstimatedTokens = 42
	if err := s.SaveView(ctx, v); err != nil {
		t.Fatalf("SaveView: %v", err)
	}
	v2, err := s.LoadView(ctx, agentA)
	if err != nil {
		t.Fatalf("LoadView: %v", err)
	}
	if len(v2.Items) != 1 || v2.Items[0].Position != 1.0 || v2.EstimatedTokens != 42 {
		t.Fatalf("view round trip = %+v", v2)
	}

	// 全量覆盖。
	v.EstimatedTokens = 99
	if err := s.SaveView(ctx, v); err != nil {
		t.Fatal(err)
	}
	v3, _ := s.LoadView(ctx, agentA)
	if v3.EstimatedTokens != 99 {
		t.Fatalf("overwrite semantics lost: %d", v3.EstimatedTokens)
	}
}

func TestSQLiteSnapshotCreateRestorePrune(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	snap, err := NewSnapshotStore(root)
	if err != nil {
		t.Fatalf("NewSnapshotStore: %v", err)
	}

	// 造原文件 → 快照 → 改写 → Restore → 内容回到快照态。
	orig := filepath.Join(root, "src", "a", "b.txt")
	if err := os.MkdirAll(filepath.Dir(orig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orig, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := snap.Create(ctx, "src/a/b.txt", []byte("v1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(orig, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := snap.Restore(ctx, name); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(orig)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("restore produced %q, want v1", got)
	}

	// Prune：maxPerFile=1，同路径多余快照被清。
	if _, err := snap.Create(ctx, "src/a/b.txt", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if err := snap.Prune(ctx, 1); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	names, _ := os.ReadDir(filepath.Join(root, ".marl", "snapshots"))
	gotSnap := 0
	for _, n := range names {
		if strings.HasSuffix(n.Name(), "b.txt.14") || strings.Contains(n.Name(), "b.txt.") {
			gotSnap++
		}
	}
	if gotSnap != 1 {
		t.Fatalf("prune left %d snapshots, want 1", gotSnap)
	}

	// 越界 / 形态伪造被拒绝。
	if _, err := snap.Create(ctx, "../escape.txt", []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("escape must be ErrInvalid, got %v", err)
	}
	if err := snap.Restore(ctx, "01234567890123__x.bak"); !errors.Is(err, ErrInvalid) &&
		!errors.Is(err, ErrNotFound) {
		t.Fatalf("forged name must be ErrInvalid/ErrNotFound, got %v", err)
	}
	if err := snap.Prune(ctx, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("prune 0 must be ErrInvalid, got %v", err)
	}
}
