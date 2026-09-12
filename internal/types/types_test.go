package types

// types 包的零值契约测试（阶段 0 只钉了注释，阶段 2 消费前补规格）。
//
// 规格覆盖：
//   - LogEntry.Validate 的四条拒绝项（Role/Prov/Audience 零值；非 Original
//     的 Prov 必须带 SourceIDs）；
//   - NewLogEntry 的安全默认与非法入参 panic（构造期程序员错误的契约）；
//   - WithProvenance 的 SourceIDs 深拷贝（血缘不随切片扩容漂移）；
//   - ContextView.Validate 的 Stability 逆序拒绝；
//   - Attachment / ToolResult 的互斥不变量；
//   - EstimateTokens 的量级（中文高估、ASCII 接近）。

import (
	"strings"
	"testing"
)

func validEntry() *LogEntry {
	return &LogEntry{AgentID: "a-1", Role: RoleUserInput, Content: "hi",
		Prov: ProvOriginal, Audience: AudienceBoth}
}

func TestLogEntryValidate(t *testing.T) {
	cases := []struct {
		name  string
		entry func() *LogEntry
		want  bool // 是否合法
	}{
		{"nil entry", func() *LogEntry { return nil }, false},
		{"zero role", func() *LogEntry { e := validEntry(); e.Role = ""; return e }, false},
		{"zero provenance", func() *LogEntry { e := validEntry(); e.Prov = ""; return e }, false},
		{"zero audience", func() *LogEntry { e := validEntry(); e.Audience = ""; return e }, false},
		{"summary without source", func() *LogEntry {
			e := validEntry()
			e.Prov = ProvSummaryOf
			return e
		}, false},
		{"valid original", func() *LogEntry { return validEntry() }, true},
		{"valid summary with source", func() *LogEntry {
			e := validEntry()
			e.Prov = ProvSummaryOf
			e.SourceIDs = []MessageID{"m1"}
			return e
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entry().Validate()
			if tc.want != (err == nil) {
				t.Fatalf("Validate = %v, want valid=%v", err, tc.want)
			}
		})
	}
}

func TestNewLogEntryDefaults(t *testing.T) {
	e := NewLogEntry("a-1", RoleUserInput, "内容")
	if e.Prov != ProvOriginal || e.Audience != AudienceBoth || e.AgentID != "a-1" {
		t.Fatalf("defaults: %+v", e)
	}
	if e.TokenEst < 1 || e.ID != "" || e.Seq != 0 || !e.CreatedAt.IsZero() {
		t.Fatalf("construction fields: %+v", e)
	}
}

func TestLogEntryConstructionPanics(t *testing.T) {
	// 程序员错误的惯用表达（构造期不可恢复的错误）。
	cases := []struct {
		name string
		act  func()
	}{
		{"zero role", func() { _ = NewLogEntry("a", InternalRole(""), "x") }},
		{"empty agent", func() { _ = NewLogEntry("", RoleUserInput, "x") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic on %s", tc.name)
				}
			}()
			tc.act()
		})
	}
}

func TestWithProvenanceCopiesSources(t *testing.T) {
	srcs := []MessageID{MessageID("s1"), MessageID("s2")}
	e := NewLogEntry("a-1", RoleUserInput, "x").WithProvenance(ProvSplitOf, srcs...)
	if e.Prov != ProvSplitOf || len(e.SourceIDs) != 2 {
		t.Fatalf("provenance: %+v", e)
	}
	// 调用方随后扩容原切片，已落（构造期）条目的血缘不得漂移。
	srcs[0] = MessageID("poison")
	if e.SourceIDs[0] != "s1" {
		t.Fatalf("source ids aliased: %+v", e.SourceIDs)
	}
}

func TestContextViewValidate(t *testing.T) {
	item := func(st Stability) ViewItem {
		return ViewItem{Ref: MessageID("m"), WireRole: WireTool, Stability: st, Visible: true}
	}
	cases := []struct {
		name  string
		items func() []ViewItem
		want  bool
	}{
		{"empty view", func() []ViewItem { return nil }, true},
		{"stable ascending", func() []ViewItem { return []ViewItem{item(StabilityStable), item(StabilityStable)} }, true},
		{"volatile before stable rejected", func() []ViewItem {
			return []ViewItem{item(StabilityVolatile), item(StabilityStable)}
		}, false},
		{"empty ref rejected", func() []ViewItem {
			it := item(StabilityStable)
			it.Ref = ""
			return []ViewItem{it}
		}, false},
		{"zero stability rejected", func() []ViewItem {
			it := item(StabilityStable)
			it.Stability = ""
			return []ViewItem{it}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &ContextView{AgentID: "a-1", Items: tc.items()}
			err := v.Validate()
			if tc.want != (err == nil) {
				t.Fatalf("Validate = %v, want valid=%v", err, tc.want)
			}
		})
	}
}

func TestAttachmentValidate(t *testing.T) {
	att := Attachment{Kind: AttachImage, MimeType: "image/png", Source: SourceFile, Data: []byte("p.png")}
	if err := att.Validate(); err != nil {
		t.Fatalf("valid attachment rejected: %v", err)
	}
	noData := att
	noData.Data = nil
	if err := noData.Validate(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty data must be named: %v", err)
	}
	pdfKind := att
	pdfKind.Kind = AttachPDF
	if err := pdfKind.Validate(); err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("unimplemented kind must fail loudly: %v", err)
	}
}

func TestToolResultValidate(t *testing.T) {
	if err := (ToolResult{ToolCallID: "c", OK: true}).Validate(); err != nil {
		t.Fatalf("ok without error type: %v", err)
	}
	if err := (ToolResult{ToolCallID: "c", ErrorType: "ENOENT"}).Validate(); err != nil {
		t.Fatalf("failure with error type: %v", err)
	}
	if err := (ToolResult{ToolCallID: "c"}).Validate(); err == nil {
		t.Fatal("failure without error type must be rejected")
	}
	if err := (ToolResult{ToolCallID: "c", OK: true, ErrorType: "ENOENT"}).Validate(); err == nil {
		t.Fatal("ok with error type must be rejected")
	}
}

func TestEstimateTokensMagnitude(t *testing.T) {
	// 中文：高估方向（≥字符数）；ASCII：接近 bytes*0.625 的量级。
	cjk := "这是一段足够长的中文文本"
	if got := EstimateTokens(cjk); got < len([]rune(cjk)) {
		t.Fatalf("cjk estimate %d must not undercount(%d runes)", got, len([]rune(cjk)))
	}
	ascii := "abcdefghijklmnopqrstuvwxyz"
	if got := EstimateTokens(ascii); got < 3 || got > len(ascii) {
		t.Fatalf("ascii estimate %d out of plausible range (26 chars)", got)
	}
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("empty content = %d, want 0", got)
	}
}

func TestMountValid(t *testing.T) {
	cases := []struct {
		name  string
		mount Mount
		want  bool
	}{
		{"valid write", Mount{Pattern: "**", Mode: PathWrite}, true},
		{"empty pattern", Mount{Mode: PathWrite}, false},
		{"zero mode", Mount{Pattern: "src/**"}, false},
		{"traversal pattern", Mount{Pattern: "../**", Mode: PathWrite}, false},
		{"absolute pattern", Mount{Pattern: "/etc/**", Mode: PathWrite}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mount.Valid()
			if tc.want != (err == nil) {
				t.Fatalf("Valid = %v, want valid=%v", err, tc.want)
			}
		})
	}
}
