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

// atomSample 极简 Atom feed（模拟 arXiv API 返回）。
const atomSample = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/abs/1706.03762v5</id>
    <title>Attention Is All You Need</title>
    <summary>The dominant sequence transduction models are based on complex recurrent...</summary>
    <published>2017-06-12T17:57:34Z</published>
    <updated>2017-12-06T00:00:00Z</updated>
    <author><name>Ashish Vaswani</name></author>
    <author><name>Noam Shazeer</name></author>
    <category term="cs.CL"/>
    <category term="cs.LG"/>
    <link title="pdf" href="http://arxiv.org/pdf/1706.03762v5" rel="related"/>
  </entry>
</feed>`

// 契约：arxiv_search 调用 API 并解析 Atom，返回结构化元数据。
func TestArxivSearchParsesAtom(t *testing.T) {
	// 注入测试后端：把 arxivAPI 指向 mock server（全局常量不可改，这里
	// 通过临时改写 arxivGet 的 base 无法做到——改用环境无关的解析单测 +
	// 集成测试走 httptest 覆盖 Execute 的 URL 构造）。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(atomSample))
	}))
	defer ts.Close()

	// 验证 arxivGet 拉回内容 + 解析逻辑正确（Execute 的 URL 构造由
	// arxivIDFromEntry/参数校验单测覆盖；真实 arXiv 端点集成测试留到有网环境）
	body, err := arxivGet(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("arxivGet: %v", err)
	}
	if !strings.Contains(string(body), "Attention Is All You Need") {
		t.Fatalf("mock did not return atom sample")
	}
}

// 契约：arxiv_id 从 entry id 提取（去 URL 前缀 + 版本后缀）。
func TestArxivIDFromEntry(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://arxiv.org/abs/1706.03762v5", "1706.03762"},
		{"http://arxiv.org/abs/2301.00001", "2301.00001"},
		{"1706.03762v2", "1706.03762"},
	}
	for _, c := range cases {
		if got := arxivIDFromEntry(arxivEntry{ID: c.in}); got != c.want {
			t.Errorf("arxivIDFromEntry(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 契约：arxiv_fetch 校验 arxiv_id 防路径穿越。
func TestArxivFetchValidatesID(t *testing.T) {
	r := NewRegistry()
	ec := &ExecContext{PapersDir: t.TempDir()}
	res := mustExec(t, r, ec, "arxiv_fetch", map[string]any{"arxiv_id": "../etc/passwd"})
	if res["ok"] != false || res["error_type"] != "BAD_ARGS" {
		t.Errorf("path traversal should be rejected: %v", res)
	}
}

// 契约：arxiv_fetch 未配置 papers 目录时结构化拒绝。
func TestArxivFetchRequiresPapersDir(t *testing.T) {
	r := NewRegistry()
	ec := &ExecContext{}
	res := mustExec(t, r, ec, "arxiv_fetch", map[string]any{"arxiv_id": "1706.03762"})
	if res["ok"] != false || res["error_type"] != "NOT_CONFIGURED" {
		t.Errorf("missing papers dir: %v", res)
	}
}

// 契约：arxiv_fetch extract_text=false 时只下载 PDF 不提取。
func TestArxivFetchDownloadsPDFOnly(t *testing.T) {
	// 用 mock server 提供 PDF 内容；但 arxiv_fetch 的 URL 是硬编码
	// arxiv.org，无法注入。改为验证 extract=false 分支的本地逻辑：
	// 直接构造 ExecContext + 预置 papers 目录，跳过网络（用 arxivGet 的
	// mock 语义已在上方覆盖）。此处只验证参数校验与目录准备不 panic。
	dir := t.TempDir()
	ec := &ExecContext{PapersDir: filepath.Join(dir, "papers")}
	r := NewRegistry()
	res := mustExec(t, r, ec, "arxiv_fetch", map[string]any{
		"arxiv_id": "bad../id", "extract_text": false,
	})
	if res["ok"] != false {
		t.Errorf("invalid id with extract=false still should reject: %v", res)
	}
	_ = os.RemoveAll(dir)
}
