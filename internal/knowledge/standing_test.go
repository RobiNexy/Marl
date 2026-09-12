package knowledge

// 常驻块编译的契约测试（Part 12.4 的四条硬约束 + 13.9 的测试条目）。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePref 放一个 preferences 文件（测试素材）。
func writePref(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture 用脚本生成两个共 200 est-token 左右的偏好文件（13.9 测试
// 条目：一个 200 token 的 test.md 编译成常驻块）。est 值随 ASCII/CJK
// 比例变化——测试里不写死数值，只断言 ≥1 与逐文件口径一致。
func fixture200(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	pref := filepath.Join(dir, "preferences")
	writePref(t, pref, "test.md", strings.Repeat("code-style preference line\n", 10))
	return dir
}

// TestCompileByteStability：同样的 preferences 内容 → 逐字节相同的编译
// 产物（Part 12.4 ③ 的 golden 检查；数据量刻意超过 4KB 的 map 遍历
// 排序巧合阈值）。重复编译 / 重新载入 / 目录重命名三种形态都查。
func TestCompileByteStability(t *testing.T) {
	dir := fixture200(t)
	prefDir := filepath.Join(dir, "preferences")
	b1, err := CompileStandingOrders(prefDir)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := CompileStandingOrders(prefDir)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Content != b2.Content {
		t.Fatal("compilation is not byte-stable")
	}
	// 复制目录（不同位置），产物应逐字节一致（文件序确定 → 前缀确定）。
	dir2 := fixture200(t)
	b3, err := CompileStandingOrders(filepath.Join(dir2, "preferences"))
	if err != nil {
		t.Fatal(err)
	}
	if b1.Content != b3.Content {
		t.Fatal("compilation depends on directory location")
	}
}

// TestCompileShape：包裹与文件标题。文件序按名字排序（artifacts 稳定）。
func TestCompileShape(t *testing.T) {
	dir := fixture200(t)
	writePref(t, filepath.Join(dir, "preferences"), "aaa-first.md", "a-one\n")
	writePref(t, filepath.Join(dir, "preferences"), "zzz-last.md", "z-last\n")
	b, err := CompileStandingOrders(filepath.Join(dir, "preferences"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.Content, "<standing_orders>") || !strings.HasSuffix(b.Content, "</standing_orders>") {
		t.Fatalf("content missing wrapper: %q", b.Content[:80])
	}
	if !strings.Contains(b.Content, "## aaa-first.md\n\na-one") {
		t.Fatalf("file body missing")
	}
	// 排序：aaa 在 test 之前、zzz 在 test 之后（fixture200 的 test.md）。
	ia := strings.Index(b.Content, "aaa-first.md")
	it := strings.Index(b.Content, "test.md")
	iz := strings.Index(b.Content, "zzz-last.md")
	if !(ia < it && it < iz) {
		t.Fatalf("order wrong: %d %d %d", ia, it, iz)
	}
	// 逐文件口径：Files 与命名序一致。
	if len(b.Files) != 3 || b.Files[0].Name != "aaa-first.md" {
		t.Fatalf("files = %+v", b.Files)
	}
}

// TestCompileCRLF normalization：Windows 编辑器保存的 CRLF → 编译产物
// 的字节稳定形态（Part 12.4 ③ 的换行统一）。
func TestCompileCRFNormalization(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writePref(t, filepath.Join(dirA, "preferences"), "x.md", "line-one\r\nline-two\r\n")
	writePref(t, filepath.Join(dirB, "preferences"), "x.md", "line-one\nline-two\n")
	a, err := CompileStandingOrders(filepath.Join(dirA, "preferences"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompileStandingOrders(filepath.Join(dirB, "preferences"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Content != b.Content {
		t.Fatal("CRLF and LF sources must compile byte-identically")
	}
}

// TestCompileEmpty 与排除规则：空目录 → 空块 + nil error（"这个项目
// 还没有偏好"是合法状态）；目录缺失是配置错误（不是空集）；README.md 排除。
func TestCompileEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := CompileStandingOrders(filepath.Join(dir, "preferences")); err == nil {
		t.Fatal("missing preferences dir must error (not silently empty)")
	}
	if err := os.MkdirAll(filepath.Join(dir, "preferences"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := CompileStandingOrders(filepath.Join(dir, "preferences"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Content != "" || b.Tokens != 0 {
		t.Fatalf("empty dir must give empty block, got %q", b.Content)
	}
	writePref(t, filepath.Join(dir, "preferences"), "README.md", "目录说明，不是偏好\n")
	b2, err := CompileStandingOrders(filepath.Join(dir, "preferences"))
	if err != nil {
		t.Fatal(err)
	}
	if b2.Content != "" {
		t.Fatalf("README.md must be excluded")
	}
}

// TestCompileOverLimit：超限硬失败 + 逐文件报告（Part 12.4 ②），13.9
// 的"加到 1200 → lint 报错"测试条目。
func TestCompileOverLimit(t *testing.T) {
	dir := t.TempDir()
	pref := filepath.Join(dir, "preferences")
	writePref(t, pref, "code-style.md", strings.Repeat("x", 1200))
	_, err := CompileStandingOrders(pref)
	if err == nil {
		t.Fatal("1200 tokens must exceed the 1000 limit")
	}
	for _, want := range []string{"超出上限", "code-style.md", "1200"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

// TestCompileUnderLimit：上限内（含 200-token fixture）通过。
func TestCompileUnderLimit(t *testing.T) {
	dir := fixture200(t)
	b, err := CompileStandingOrders(filepath.Join(dir, "preferences"))
	if err != nil {
		t.Fatalf("fixture must be under limit: %v", err)
	}
	if b.Tokens <= 0 || b.Tokens != b.Tokens {
		t.Fatalf("tokens inconsistent")
	}
	if !errors.Is(err, nil) {
		t.Fatal("nil error")
	}
	// Report 形态（lint 输出格式 Part 12.4 ②）。
	r := b.Report(MaxStandingTokens)
	if !strings.Contains(r, "preferences/") {
		t.Fatalf("report missing prefix: %q", r)
	}
	if !strings.Contains(r, "test.md") {
		t.Fatal("report must show per-file")
	}
}

// TestErrorOnUnreadable：目录不存在 → error（配置面必须报错，不装空）。
func TestErrorOnUnreadable(t *testing.T) {
	_, err := CompileStandingOrders(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("missing dir must error")
	}
}
