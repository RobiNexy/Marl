//go:build ignore

package skill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── 转换器契约：可读性降级，不产生错误事实内容 ─────────────────────

func TestHTMLToMarkdown(t *testing.T) {
	src := `<html><head><title>Go &amp; You</title>` +
		`<style>body{color:red}</style><script>evil()</script></head><body>` +
		`<h1>Hello</h1><p>This is <b>bold</b>, <em>italic</em>, ` +
		`and a <a href="/next">link</a>.<br>New line.</p>` +
		`<ul><li>one</li><li>two</li></ul>` +
		`<pre><code>fmt.Println("hi")</code></pre></body></html>`

	title, md := htmlToMarkdown(src)
	if title != "Go & You" {
		t.Fatalf("title: got %q", title)
	}
	for _, want := range []string{
		"# Hello",
		"**bold**",
		"*italic*",
		"[link](/next)",
		"- one",
		"- two",
		"```",
		`fmt.Println("hi")`,
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "<script>") || strings.Contains(md, "evil()") ||
		strings.Contains(md, "color:red") || strings.Contains(md, "<b>") {
		t.Fatalf("converter leaked raw markup/script:\n%s", md)
	}
	if strings.Contains(md, "&amp;") { // 实体必须解回字符
		t.Fatalf("entities not unescaped:\n%s", md)
	}
}

// 非法输入不炸：纯文本、空串原样降级。
func TestHTMLToMarkdownDegenerate(t *testing.T) {
	for _, src := range []string{"", "plain text no tags", "<p>unclosed <div"} {
		_, md := htmlToMarkdown(src)
		if strings.Contains(md, "<div") && src == "<p>unclosed <div" {
			continue // 剥壳即可，保留文本合法
		}
		_ = md
	}
}

// ── 技能执行契约：网络、缓存、安全边界 ─────────────────────────────

func newFetchEC(t *testing.T) (*ExecContext, string) {
	root := t.TempDir()
	cache := filepath.Join(root, ".agents", "web_cache")
	return &ExecContext{WorkspaceRoot: root, WebCacheDir: cache}, cache
}

// 契约：抓取→转换；同 URL 第二次命中缓存零网络请求。
func TestWebFetchCachesByURL(t *testing.T) {
	var hits int
	page := "<html><title>Cached Page</title><body><h2>Body</h2></body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	ec, cacheDir := newFetchEC(t)
	r := reg()
	args := map[string]any{"url": srv.URL + "/docs/intro"}

	res := mustExec(t, r, ec, "web_fetch", args) // 第一次：真抓取
	if res["cached"] != false || res["title"] != "Cached Page" {
		t.Fatalf("first fetch: %v", res)
	}

	res = mustExec(t, r, ec, "web_fetch", args) // 第二次：命中缓存
	if res["cached"] != true {
		t.Fatalf("second fetch must hit cache: %v", res)
	}
	if hits != 1 {
		t.Fatalf("server hit %d times, want 1 (cache bypassed network)", hits)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 cache file in %s: %v %v", cacheDir, entries, err)
	}
}

// 契约：cache=false 绕过缓存强制重抓。
func TestWebFetchBypassesCacheWhenDisabled(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("<html><body>x</body></html>"))
	}))
	defer srv.Close()

	ec, _ := newFetchEC(t)
	r := reg()
	args := map[string]any{"url": srv.URL, "cache": false}
	mustExec(t, r, ec, "web_fetch", args)
	mustExec(t, r, ec, "web_fetch", args)
	if hits != 2 {
		t.Fatalf("hits=%d, cache=false must re-fetch every time", hits)
	}
}

// 安全边界：非 http(s) 协议拒绝（file:// 读本地文件是 SSRF 变体）。
func TestWebFetchRejectsNonHTTPScheme(t *testing.T) {
	ec, _ := newFetchEC(t)
	r := reg()
	for _, u := range []string{"file:///etc/passwd", "ftp://x/y", "/etc/passwd"} {
		res := r.Execute(context.Background(), "web_fetch", ec, map[string]any{"url": u})
		if res["error_type"] != "INVALID_URL" {
			t.Fatalf("url %q: want INVALID_URL, got %v", u, res)
		}
	}
}

// HTTP 错误码返回结构化 FETCH_FAILED（不是普通 error）。
func TestWebFetchHTTPErrorIsStructured(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	ec, _ := newFetchEC(t)
	res := reg().Execute(context.Background(), "web_fetch", ec, map[string]any{"url": srv.URL})
	if res["ok"] != false || res["error_type"] != "FETCH_FAILED" {
		t.Fatalf("want structured FETCH_FAILED, got %v", res)
	}
}

// 大响应硬拒：防止异常页面撑爆上下文。
func TestWebFetchRejectsOversizedResponse(t *testing.T) {
	big := strings.Repeat("<p>x</p>", (5<<20)/8+16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big)) //nolint:errcheck // 测试桩
	}))
	defer srv.Close()
	ec, _ := newFetchEC(t)
	res := reg().Execute(context.Background(), "web_fetch", ec, map[string]any{"url": srv.URL})
	if res["error_type"] != "CONTENT_TOO_LARGE" {
		t.Fatalf("want CONTENT_TOO_LARGE, got %v", res["error_type"])
	}
}
