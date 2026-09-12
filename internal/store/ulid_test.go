package store

import (
	"strings"
	"testing"
)

func TestULIDKnownVector(t *testing.T) {
	if got := encodeULID([16]byte{}); got != strings.Repeat("0", 26) {
		t.Fatalf("zero vector = %q", got)
	}
	// 官方示例（spec）：时间戳 1469918176385 → 前 10 字符 "01ARYZ6S41"
	raw := [16]byte{}
	ms := uint64(1469918176385)
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	enc := encodeULID(raw)
	if want := "01ARYZ6S41"; enc[:10] != want {
		t.Fatalf("timestamp portion = %q, want %q", enc[:10], want)
	}
}
