//go:build ignore

package skill

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ────────────────────────────── arxiv_search / arxiv_fetch ──────────────────────────────
//
// arXiv 专项（Patch 1/3：文献类）。只读网络 + 写入 papers/。
// 设计要点（Patch 3）：search 只回元数据（有限深入探索），fetch 下载 PDF +
// 提取文本。不引 PDF 解析库（纯二进制取向）：提取用外部 pdftotext，不可用则
// 只存 PDF 并提示（退路优先）。

const arxivAPI = "http://export.arxiv.org/api/query"

type arxivSearchSkill struct{}

func (s *arxivSearchSkill) Name() string { return "arxiv_search" }
func (s *arxivSearchSkill) Description() string {
	return "Search arXiv papers via the official API. Returns metadata only (title/authors/abstract/pdf link), not full text. Use arxiv_fetch to download a specific paper."
}
func (s *arxivSearchSkill) Capability() Capability { return CapReadOnly }

// arxivFeed Atom 响应（只取需要的字段）。
type arxivFeed struct {
	XMLName xml.Name     `xml:"feed"`
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	ID         string          `xml:"id"`
	Title      string          `xml:"title"`
	Summary    string          `xml:"summary"`
	Published  string          `xml:"published"`
	Updated    string          `xml:"updated"`
	Authors    []arxivAuthor   `xml:"author"`
	Categories []arxivCategory `xml:"category"`
	Links      []arxivLink     `xml:"link"`
}

type arxivAuthor struct {
	Name string `xml:"name"`
}

type arxivCategory struct {
	Term string `xml:"term,attr"`
}

type arxivLink struct {
	Title string `xml:"title,attr"`
	Href  string `xml:"href,attr"`
	Rel   string `xml:"rel,attr"`
}

func (s *arxivSearchSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	query := strArg(args, "query", "")
	if query == "" {
		return nil, errf("BAD_ARGS", "query is required (arXiv API syntax, e.g. 'au:Hinton AND cat:cs.LG')")
	}
	maxResults := clamp(intArg(args, "max_results", 10), 1, 50)
	sortBy := strArg(args, "sort_by", "relevance")
	switch sortBy {
	case "relevance", "submittedDate", "lastUpdatedDate":
	default:
		return nil, errf("BAD_ARGS", "sort_by must be relevance|submittedDate|lastUpdatedDate, got %q", sortBy)
	}

	u := fmt.Sprintf("%s?search_query=%s&start=0&max_results=%d&sortBy=%s",
		arxivAPI, url.QueryEscape(query), maxResults, sortBy)
	body, err := arxivGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var feed arxivFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("arxiv_search: parse Atom: %w", err)
	}

	results := make([]map[string]any, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		results = append(results, map[string]any{
			"arxiv_id":   arxivIDFromEntry(e),
			"title":      strings.TrimSpace(e.Title),
			"abstract":   strings.TrimSpace(e.Summary),
			"authors":    arxivAuthorNames(e.Authors),
			"categories": arxivCategoryTerms(e.Categories),
			"published":  e.Published,
			"updated":    e.Updated,
			"pdf_url":    arxivPDFURL(e),
		})
	}
	return Result("total", len(results), "results", results), nil
}

type arxivFetchSkill struct{}

func (s *arxivFetchSkill) Name() string { return "arxiv_fetch" }
func (s *arxivFetchSkill) Description() string {
	return "Download an arXiv paper PDF and extract text to Markdown. Text extraction needs the external 'pdftotext' tool; if absent, the PDF is saved and extraction is skipped."
}
func (s *arxivFetchSkill) Capability() Capability { return CapReadOnly }

func (s *arxivFetchSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	arxivID := strArg(args, "arxiv_id", "")
	if arxivID == "" {
		return nil, errf("BAD_ARGS", "arxiv_id is required (e.g. '1706.03762')")
	}
	// 防御：arxiv_id 只能含 [0-9a-zA-Z.\-]，防路径穿越
	for _, r := range arxivID {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-') {
			return nil, errf("BAD_ARGS", "invalid arxiv_id %q", arxivID)
		}
	}
	papersDir := ec.PapersDir
	if papersDir == "" {
		return nil, errf("NOT_CONFIGURED", "papers dir not configured")
	}
	if err := os.MkdirAll(papersDir, 0o755); err != nil {
		return nil, fmt.Errorf("arxiv_fetch: mkdir papers: %w", err)
	}

	pdfURL := "https://arxiv.org/pdf/" + arxivID
	body, err := arxivGet(ctx, pdfURL)
	if err != nil {
		return nil, err
	}
	pdfPath := filepath.Join(papersDir, arxivID+".pdf")
	if err := os.WriteFile(pdfPath, body, 0o644); err != nil {
		return nil, fmt.Errorf("arxiv_fetch: write pdf: %w", err)
	}

	extract := boolArg(args, "extract_text", true)
	out := map[string]any{"ok": true, "arxiv_id": arxivID, "pdf_path": pdfPath}
	if !extract {
		return out, nil
	}
	textPath := filepath.Join(papersDir, arxivID+".md")
	preview := ""
	if pdftotext, lerr := exec.LookPath("pdftotext"); lerr == nil {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, pdftotext, "-layout", pdfPath, "-")
		var sb strings.Builder
		cmd.Stdout = &sb
		if runErr := cmd.Run(); runErr == nil {
			text := sb.String()
			if werr := os.WriteFile(textPath, []byte(text), 0o644); werr == nil {
				out["text_path"] = textPath
				preview = text
				if len(preview) > 4000 {
					preview = preview[:4000] + "\n... (truncated)"
				}
			}
		}
	} else {
		out["text_path"] = ""
		out["hint"] = "pdftotext not available; PDF saved, text extraction skipped (install poppler-utils to enable)"
	}
	out["text_preview"] = preview
	return out, nil
}

// arxivGet 单次 HTTP GET（带超时 + 64MB 上限 + 非 200 报错）。
func arxivGet(ctx context.Context, rawURL string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("arxiv: build request: %w", err)
	}
	req.Header.Set("User-Agent", "samphiharness/0.1 (research agent)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("arxiv: GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arxiv: GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("arxiv: read %s: %w", rawURL, err)
	}
	return body, nil
}

func arxivIDFromEntry(e arxivEntry) string {
	// id 形如 http://arxiv.org/abs/1706.03762v5 → 1706.03762
	s := e.ID
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	// 去版本后缀 v\d+（如 1706.03762v5）
	if j := strings.LastIndex(s, "v"); j > 0 && allDigits(s[j+1:]) {
		s = s[:j]
	}
	return s
}

func arxivAuthorNames(authors []arxivAuthor) []string {
	out := make([]string, 0, len(authors))
	for _, a := range authors {
		if n := strings.TrimSpace(a.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func arxivCategoryTerms(cats []arxivCategory) []string {
	out := make([]string, 0, len(cats))
	for _, c := range cats {
		if c.Term != "" {
			out = append(out, c.Term)
		}
	}
	return out
}

func arxivPDFURL(e arxivEntry) string {
	for _, l := range e.Links {
		if l.Title == "pdf" || (l.Rel == "related" && strings.Contains(l.Href, "/pdf/")) {
			return l.Href
		}
	}
	return ""
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
