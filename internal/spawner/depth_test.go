package spawner

// 深度闸的独立测试（13.11"到顶拒绝 + 错误回传"的判据形状）。

import (
	"strings"
	"testing"
)

func alwaysTrue(int) bool     { return true }
func onlyRoot(int) bool       { return false } // 未达到任何深度（角色谓词全关）
func allowUpToTwo(d int) bool { return d <= 1 }

// TestDepthGate：三种裁决形态（权限 → 到顶 → 通过）。
func TestDepthGate(t *testing.T) {
	childDepth, deny := depthGate(alwaysTrue, 1, 2)
	if deny != nil || childDepth != 2 {
		t.Fatalf("pass case: depth=%d deny=%v", childDepth, deny)
	}
	_, deny = depthGate(alwaysTrue, 2, 2)
	if deny == nil || string(deny.Code) != "MAX_DEPTH_REACHED" {
		t.Fatalf("cap case: %v", deny)
	}
	// 措辞纪律（Part 9.4 第一条）：错误回传文本要指出正确路径。
	if !strings.Contains(deny.Reason, "请直接执行任务") {
		t.Fatalf("cap reason: %q", deny.Reason)
	}
	_, deny = depthGate(onlyRoot, 0, 2)
	if deny == nil || string(deny.Code) != "PROFILE_NOT_PERMITTED" {
		t.Fatalf("permitted case: %v", deny)
	}
	if _, deny := depthGate(allowUpToTwo, 1, 3); deny != nil {
		t.Fatalf("depth<cap with permission must pass: %v", deny)
	}
}
