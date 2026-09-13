package types

// ReplyFinalMarker 协议的单元规格（读写两侧的字节面契约）。

import (
	"strings"
	"testing"
)

func TestReplyFinalMarker(t *testing.T) {
	t.Run("HasReplyFinal roundtrip", func(t *testing.T) {
		if HasReplyFinal("正文") {
			t.Fatal("无标记内容误报")
		}
		marked := WithReplyFinal("正文\n")
		if !HasReplyFinal(marked) {
			t.Fatal("WithReplyFinal 后应检出标记")
		}
		if !strings.HasPrefix(marked, "正文\n") {
			t.Fatalf("标记必须附加在内容之后（不污染正文头部）: %q", marked)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		once := WithReplyFinal("x")
		if twice := WithReplyFinal(once); twice != once {
			t.Fatalf("重复附加应幂等: %q vs %q", once, twice)
		}
	})
	t.Run("truncated marker falls back", func(t *testing.T) {
		// 撕裂自愈的根：标记**文本**不完整即不检出（写入横跨截断 → 慢路径）。
		// 尾部换行的丢失不在此列——标记本身完整时内容已可用。
		full := WithReplyFinal("x")
		for cut := 2; cut <= len(ReplyFinalMarker)+1; cut++ {
			truncated := full[:len(full)-cut]
			if HasReplyFinal(truncated) {
				t.Fatalf("截断 %d 字节后不应检出标记", cut)
			}
		}
		if !HasReplyFinal(full) {
			t.Fatal("完整内容必须检出")
		}
	})
}
