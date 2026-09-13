package proto

// 冻结工具表的回归测试（Part 4.1 / ADR-0015：意图名与顺序是协议的一部分）。
//
// 意图名会进历史 Log 的 tool_call 记录；顺序是冻结前缀的字节组成。两处
// 的漂移都会造成"历史无法归因"或"全项目缓存前缀失效"——这里钉死。

import (
	"reflect"
	"testing"
)

func TestIntentToolNamesFrozen(t *testing.T) {
	want := []string{
		"spawn_subagent",
		"request_human",
		"request_reconfigure",
		"request_branch",
		"request_discussion",
		"report_to_parent",
		"spawn_batch",
		"llm_call",
	}
	if got := IntentToolNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("意图表漂移：\n got=%v\nwant=%v（改名/改序 = 破坏协议与缓存前缀）", got, want)
	}
}

func TestIsIntentTool(t *testing.T) {
	for _, n := range IntentToolNames() {
		if !IsIntentTool(n) {
			t.Fatalf("%q 应为意图工具", n)
		}
	}
	// 技能名（file_read 等）与未知名不得被当意图——防"意图名拼错被容错"。
	for _, n := range []string{"file_read", "file_write", "spawn_subagentx", "", "llm_call "} {
		if IsIntentTool(n) {
			t.Fatalf("%q 不应被识别为意图", n)
		}
	}
}

// 阶段 11 的错误码协议面（agent.llm_call 的错误码在 proto 侧无常量，
// 这里测的是工具名常量与 MsgType 的 Valid 面回归）。
func TestMsgTypeValid(t *testing.T) {
	if MsgUnknown.Valid() {
		t.Fatal("零值 MsgUnknown 不得 Valid")
	}
	for _, m := range []MsgType{MsgTaskAssign, MsgChildReport, MsgHumanInput, MsgEscalation, MsgEscalationReply, MsgReconfigure, MsgShutdown} {
		if !m.Valid() {
			t.Fatalf("%v 应 Valid", m)
		}
	}
}
