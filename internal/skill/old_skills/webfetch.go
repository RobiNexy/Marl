//go:build ignore

package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// webFetchSkill 抓取 URL 并转为 Markdown（Patch 2/9：环境类 read_only 技能）。
//
// 设计约束（Patch 8 权衡表）："只有 web_fetch，无搜索引擎——防止 LLM 撒网，
// 只能顺着人类给的门户读"。因此不做任何智能行为：单页抓取、无重定向之外
// 的探索、无 JS 渲染。仅支持 http/https（拒绝 file:// 等 SSRF 式本地读取）。
// 结果按 URL 哈希缓存在 .agents/web_cache/（Fossil ignore），重复阅读零成本。
type webFetchSkill struct{}

const (
	fetchMaxBytes = 5 << 20          // 5MB：文档页面远小于此；防御异常大响应撑爆上下文
	fetchTimeout  = 30 * time.Second // 单次请求超时（宁等勿截断，但 30s 足够静态页面）
	cacheKeyVer   = "v1"             // 缓存键版本：转换器升级后换版本即可全量失效
)

func (s *webFetchSkill) Name() string { return "web_fetch" }
func (s *webFetchSkill) Description() string {
	return "Fetch an http(s) URL and convert the page to Markdown. " +
		"Results are cached under .agents/web_cache (pass cache=false to force a re-fetch)."
}
func (s *webFetchSkill) Capability() Capability { return CapReadOnly }

// webCacheEntry 磁盘缓存条目。JSON 落盘，人可以直接查看/删除。
type webCacheEntry struct {
	URL       string `json:"url"`
	FetchedAt string `json:"fetched_at"` // RFC3339
	Title     string `json:"title"`
	Markdown  string `json:"markdown"`
}

func (s *webFetchSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	rawURL := strings.TrimSpace(strArg(args, "url", ""))
	if rawURL == "" {
		return nil, errf("INVALID_URL", "url parameter is required")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return nil, &SkillError{
			Type: "INVALID_URL",
			Msg:  fmt.Sprintf("only http/https URLs are supported, got %q", rawURL),
			Hint: "This skill cannot read local files — use file_read for workspace paths.",
		}
	}
	useCache := boolArg(args, "cache", true)

	// 缓存查找（缓存目录未接线时静默跳过：WebCacheDir 为可选配置）
	var cachePath string
	if useCache && ec != nil && ec.WebCacheDir != "" {
		sum := sha256.Sum256([]byte(cacheKeyVer + "\x00" + rawURL))
		cachePath = filepath.Join(ec.WebCacheDir, hex.EncodeToString(sum[:])+".json")
		if entry, ok := readWebCache(cachePath); ok {
			return Result("url", entry.URL, "title", entry.Title,
				"content", entry.Markdown, "content_length", len(entry.Markdown),
				"cached", true), nil
		}
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errf("INVALID_URL", "malformed url %q: %v", rawURL, err)
	}
	req.Header.Set("User-Agent", "SamphiHarness-web_fetch/1.0 (+local agent harness)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &SkillError{
			Type: "FETCH_FAILED",
			Msg:  fmt.Sprintf("request to %q failed: %v", rawURL, err),
			Hint: "Check network connectivity and that the host is reachable.",
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &SkillError{
			Type: "FETCH_FAILED",
			Msg:  fmt.Sprintf("%s returned HTTP %d for %q", resp.Status, resp.StatusCode, rawURL),
			Hint: "A 404/410 usually means a stale link; verify via the site's index page.",
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes+1))
	if err != nil {
		return nil, errf("FETCH_FAILED", "reading response body: %v", err)
	}
	if len(body) > fetchMaxBytes {
		return nil, &SkillError{
			Type:  "CONTENT_TOO_LARGE",
			Msg:   fmt.Sprintf("response exceeds %d byte limit", fetchMaxBytes),
			Hint:  "The page is too large to inline. Ask the human for a narrower URL or a text-only mirror.",
			Extra: map[string]any{"limit_bytes": fetchMaxBytes},
		}
	}

	title, markdown := htmlToMarkdown(string(body))

	// 写缓存尽力而为：磁盘满/只读不阻断返回（下次会重新抓取而已）
	if cachePath != "" {
		_ = os.MkdirAll(ec.WebCacheDir, 0o755)
		entry := webCacheEntry{URL: rawURL, FetchedAt: time.Now().Format(time.RFC3339),
			Title: title, Markdown: markdown}
		if data, jErr := json.MarshalIndent(entry, "", "  "); jErr == nil {
			_ = writeFileAtomic(cachePath, data, 0o644)
		}
	}

	return Result("url", rawURL, "title", title, "content", markdown,
		"content_length", len(markdown), "cached", false), nil
}

func readWebCache(path string) (webCacheEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return webCacheEntry{}, false
	}
	var entry webCacheEntry
	if json.Unmarshal(data, &entry) != nil || entry.Markdown == "" {
		return webCacheEntry{}, false
	}
	return entry, true
}

// ── HTML → Markdown（纯 stdlib 正则管线，无解析器依赖）──────────────

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	// RE2 无反向引用，逐标签编译"开闭匹配"正则（script/style/head 整块丢弃）
	droppedTags  = []string{"script", "style", "noscript", "svg", "head"}
	droppedTagRe = buildDroppedTagRe()
	titleRe      = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title\s*>`)
	headingRe    = regexp.MustCompile(`(?is)<h([1-6])[^>]*>(.*?)</h[1-6]\s*>`)
	linkRe       = regexp.MustCompile(`(?is)<a\s[^>]*href\s*=\s*["']([^"']*)["'][^>]*>(.*?)</a\s*>`)
	boldRe       = regexp.MustCompile(`(?is)<(?:strong|b)\b[^>]*>(.*?)</(?:strong|b)\s*>`)
	italicRe     = regexp.MustCompile(`(?is)<(?:em|i)\b[^>]*>(.*?)</(?:em|i)\s*>`)
	preBlockRe   = regexp.MustCompile(`(?is)<pre\b[^>]*>(.*?)</pre\s*>`)
	inlineCodeRe = regexp.MustCompile(`(?is)<code\b[^>]*>(.*?)</code\s*>`)
	breakRe      = regexp.MustCompile(`(?is)<br\s*/?>`)
	// no-op keeper：保持 gofmt 分组对齐
	listItemOpenRe = regexp.MustCompile(`(?is)<li\b[^>]*>`)
	blockTagRe     = regexp.MustCompile(`(?i)</?(?:p|div|ul|ol|table|tr|blockquote|section|article|header|footer|h[1-6]|dl|dt|dd|form)\b[^>]*>`)
	anyTagRe       = regexp.MustCompile(`<[^>]+>`)
	lineTailWsRe   = regexp.MustCompile(`[ \t]+\n`)
	blankRunRe     = regexp.MustCompile(`\n{3,}`)
)

func buildDroppedTagRe() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(droppedTags))
	for i, tag := range droppedTags {
		out[i] = regexp.MustCompile(`(?is)<` + tag + `\b.*?</` + tag + `\s*>`)
	}
	return out
}

// htmlToMarkdown 把 HTML 文本降级为 LLM 可读的 Markdown。
// [推断] 刻意用正则管线而非完整 DOM 解析器：目标是"可读性降级"（标题层级、
// 列表、链接、代码块保留），不是保真渲染。畸形/嵌套标签场景输出可能不完美，
// 但未识别标签一律剥壳留文，不会产生错误的事实内容。约百行换零第三方依赖，
// 与 matchGlob 同一取舍（纯二进制部署取向）。
func htmlToMarkdown(src string) (title, md string) {
	if m := titleRe.FindStringSubmatch(src); m != nil {
		title = strings.TrimSpace(html.UnescapeString(stripTags(m[1])))
	}

	s := htmlCommentRe.ReplaceAllString(src, "")
	for _, re := range droppedTagRe { // script/style/head 连内容整体丢弃
		s = re.ReplaceAllString(s, "")
	}
	s = headingRe.ReplaceAllStringFunc(s, func(h string) string {
		m := headingRe.FindStringSubmatch(h)
		level := strings.Repeat("#", int(m[1][0]-'0'))
		return "\n\n" + level + " " + stripTags(strings.TrimSpace(m[2])) + "\n\n"
	})
	s = preBlockRe.ReplaceAllStringFunc(s, func(pre string) string {
		m := preBlockRe.FindStringSubmatch(pre)
		return "\n```\n" + m[1] + "\n```\n"
	})
	s = listItemOpenRe.ReplaceAllString(s, "\n- ")
	s = breakRe.ReplaceAllString(s, "\n")
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = linkRe.ReplaceAllString(s, "[$2]($1)")
	s = boldRe.ReplaceAllString(s, "**$1**")
	s = italicRe.ReplaceAllString(s, "*$1*")
	s = inlineCodeRe.ReplaceAllString(s, "`$1`")

	md = anyTagRe.ReplaceAllString(s, " ")
	md = lineTailWsRe.ReplaceAllString(md, "\n") // 行尾空白来自内联标签间隙
	lines := strings.Split(md, "\n")
	for i, ln := range lines {
		lines[i] = strings.Join(strings.Fields(ln), " ")
	}
	md = blankRunRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return title, strings.TrimSpace(html.UnescapeString(md))
}

// stripTags 快速去标签（仅用于 title/heading 内层小片段，避免二次转义伪影）。
func stripTags(s string) string { return anyTagRe.ReplaceAllString(s, "") }
