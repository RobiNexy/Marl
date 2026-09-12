package skill

// 文件类技能的阶段 2 测试（13.4：content/summary 模式、图片 Attachment 通道、
// list_dir 的 exclude/depth、file_write 的原子写 + 快照 + 只读拒绝）。
// 环境拼装：真实文件系统（临时目录）+ ns.Resolver + 内存快照存储。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marl/internal/ns"
	"marl/internal/store"
	"marl/internal/types"
)

// newTestEnv 构造 SkillEnv：workspace = 临时目录，全工作区可写
// （"**" write 挂载），快照走 store.NewSnapshotStore。
func newTestEnv(t *testing.T) (env *SkillEnv, root string) {
	t.Helper()
	root = t.TempDir()
	resolver, err := ns.NewResolver(root)
	if err != nil {
		t.Fatalf("ns.NewResolver: %v", err)
	}
	snap, err := store.NewSnapshotStore(root)
	if err != nil {
		t.Fatalf("NewSnapshotStore: %v", err)
	}
	env = &SkillEnv{
		AgentID:     "agent-1",
		Namespace:   &types.Namespace{AgentID: "agent-1", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}}},
		Resolver:    resolver,
		ProjectRoot: root,
		WorkDir:     root,
		Depth:       1, MaxDepth: 3,
		Snapshots: snap,
	}
	return env, root
}

func mustSuccess(t *testing.T, s Skill, env *SkillEnv, args map[string]any) map[string]any {
	t.Helper()
	r, err := s.Execute(context.Background(), args, env)
	if err != nil {
		t.Fatalf("Execute %s: %v", s.Name(), err)
	}
	if r == nil || !r.OK {
		t.Fatalf("Execute %s not ok: %+v", s.Name(), r)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("SkillResult invalid: %v", err)
	}
	return r.Data
}

// wantFailure 断言"业务失败 + 具体错误码"（Skill 契约：失败必须结构化）。
func wantFailure(t *testing.T, s Skill, env *SkillEnv, args map[string]any, code string) {
	t.Helper()
	r, err := s.Execute(context.Background(), args, env)
	if err != nil {
		t.Fatalf("Execute %s: infrastructure error must not flow here: %v", s.Name(), err)
	}
	if r != nil && r.OK {
		t.Fatalf("Execute %s unexpectedly ok: %+v", s.Name(), r)
	}
	if r == nil || r.ErrorType != code {
		t.Fatalf("error type = %+v, want %s", r, code)
	}
}

func TestFileReadContentPagination(t *testing.T) {
	env, root := newTestEnv(t)
	var lines []string
	for i := 1; i <= 500; i++ {
		lines = append(lines, fmt.Sprintf("line-%03d", i))
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	res := mustSuccess(t, FileRead, env, map[string]any{"path": "big.txt", "mode": "content", "offset": 490, "limit": 20})
	if res["total_lines"] != 500 || res["eof"] != true {
		t.Fatalf("pagination flags: %v", res)
	}
	if !strings.Contains(fmt.Sprint(res["content"]), "line-490") {
		t.Fatalf("offset wrong: %v", res["content"])
	}

	res = mustSuccess(t, FileRead, env, map[string]any{"path": "big.txt", "mode": "content", "limit": 9999})
	if res["limit"] != 200 {
		t.Fatalf("hard limit not enforced: %v", res["limit"])
	}
}

func TestFileReadAutoLargeFileSummarizes(t *testing.T) {
	env, root := newTestEnv(t)
	var lines []string
	for i := 1; i <= 500; i++ {
		lines = append(lines, "filler line")
	}
	if err := os.WriteFile(filepath.Join(root, "auto.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	// auto → 大文件自动 summary；纯文本 → 相同分支：preview + truncated。
	// summary 模式必须**绝不**返回全量 content（Part 4.4 的红线）。
	res := mustSuccess(t, FileRead, env, map[string]any{"path": "auto.txt"})
	if res["type"] != "text" {
		t.Fatalf("auto large file should summarize as text: %v", res)
	}
	if _, has := res["content"]; has {
		t.Fatalf("summary must not carry full content: %v", res)
	}
}

func TestFileReadMarkdownOutline(t *testing.T) {
	env, root := newTestEnv(t)
	md := "# Title\n\ntext\n## Section\n"
	if err := os.WriteFile(filepath.Join(root, "r.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	res := mustSuccess(t, FileRead, env, map[string]any{"path": "r.md", "mode": "summary"})
	if res["type"] != "markdown" {
		t.Fatalf("markdown summary type: %v", res)
	}
	if !strings.Contains(fmt.Sprint(res["outline"]), "Section") {
		t.Fatalf("outline missing entries: %v", res["outline"])
	}
}

func TestFileReadMissingAndEscape(t *testing.T) {
	env, _ := newTestEnv(t)
	wantFailure(t, FileRead, env, map[string]any{"path": "no/such/file.txt"}, ErrNotFound) // ENOENT
	wantFailure(t, FileRead, env, map[string]any{"path": "../outside.txt"}, ErrNotFound)   // 越界伪装为不存在
}

func TestFileReadImageAttachment(t *testing.T) {
	env, root := newTestEnv(t)
	// 一个 1px PNG（文件头 + 最小 IEND）。
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R'}
	if err := os.WriteFile(filepath.Join(root, "pic.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	res := mustSuccess(t, FileRead, env, map[string]any{"path": "pic.png"})
	if res["mime"] != "image/png" || res["size_bytes"] != len(png) {
		t.Fatalf("image metadata: %v", res)
	}
	att, ok := res["attachment"].(types.Attachment)
	if !ok {
		t.Fatalf("attachment field: %T", res["attachment"])
	}
	if att.Kind != types.AttachImage || att.Source != types.SourceFile ||
		!strings.HasSuffix(string(att.Data), "pic.png") {
		t.Fatalf("attachment shape: %+v", att)
	}
	if err := att.Validate(); err != nil {
		t.Fatalf("attachment validation: %v", err)
	}
}

// TestImageMimeMagicEdge 守魔数探测的越界边界：短字节串不得 panic，
// 也不得把被截断的花样判成图片。
func TestImageMimeMagicEdge(t *testing.T) {
	cases := []struct {
		name  string
		raw   []byte
		mime  string
		isImg bool
	}{
		{"empty", nil, "", false},
		{"three bytes jpeg prefix", []byte{0xFF, 0xD8, 0xFF}, "image/jpeg", true},
		{"gif truncated", []byte("GIF"), "", false},
		{"gif full", []byte("GIF89a"), "image/gif", true},
		{"png truncated", []byte{0x89, 'P', 'N'}, "", false},
		{"webp short", []byte("RIFF\x00\x00\x00\x00WEB"), "", false},
		{"text", []byte("hello world"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, ok := imageMime(tc.raw)
			if ok != tc.isImg || (!ok && mime != "") {
				t.Fatalf("imageMime(%v) = (%q, %v)", tc.raw, mime, ok)
			}
			if tc.isImg && mime != tc.mime {
				t.Fatalf("mime = %q, want %q", mime, tc.mime)
			}
		})
	}
}

func TestListDirExcludesAndDepth(t *testing.T) {
	env, root := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "deep", "x.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "sec", "third"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("package src"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := mustSuccess(t, ListDir, env, map[string]any{"path": "."})
	if strings.Contains(fmt.Sprint(res["tree"]), "node_modules") {
		t.Fatalf("default excludes not applied: %v", res["tree"])
	}
	tree := treeJSON(t, res["tree"])
	if !strings.Contains(tree, "a.go") {
		t.Fatalf("src/a.go missing: %v", tree)
	}
	if strings.Contains(tree, "third") {
		t.Fatalf("depth=2 must prune deeper dirs: %v", tree)
	}

	res = mustSuccess(t, ListDir, env, map[string]any{"path": ".", "depth": 5})
	if !strings.Contains(treeJSON(t, res["tree"]), "third") {
		t.Fatalf("depth=5 should reveal third: %v", res["tree"])
	}
}

func treeJSON(t *testing.T, tree any) string {
	t.Helper()
	b, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("marshal tree: %v", err)
	}
	return string(b)
}

func TestListDirOnFileRejected(t *testing.T) {
	env, root := newTestEnv(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantFailure(t, ListDir, env, map[string]any{"path": "f.txt"}, "NOT_A_DIRECTORY")
}

func TestFileWriteAtomicAndSnapshot(t *testing.T) {
	env, root := newTestEnv(t)

	// 新建。
	res := mustSuccess(t, FileWrite, env, map[string]any{"path": "new.txt", "content": "v0"})
	if res["new_file"] != true || res["bytes_written"] == 0 {
		t.Fatalf("create: %v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(got) != "v0" {
		t.Fatalf("content mismatch: %q", got)
	}

	// 覆盖 → 快照存在，内容 v0 已备份。
	res = mustSuccess(t, FileWrite, env, map[string]any{"path": "new.txt", "content": "v1"})
	snapName, _ := res["snapshot"].(string)
	if snapName == "" {
		t.Fatalf("snapshot missing: %v", res)
	}
	snapPath := filepath.Join(root, ".marl", "snapshots", snapName)
	snapData, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot %s: %v", snapName, err)
	}
	if string(snapData) != "v0" {
		t.Fatalf("snapshot content = %q, want v0", snapData)
	}

	// 相同内容 → 不改写。
	res = mustSuccess(t, FileWrite, env, map[string]any{"path": "new.txt", "content": "v1"})
	if res["changed"] != false || res["bytes_written"] != 0 {
		t.Fatalf("idempotent write: %v", res)
	}

	// create_new 已存在 → FILE_ALREADY_EXISTS。
	wantFailure(t, FileWrite, env, map[string]any{"path": "new.txt", "content": "x", "mode": "create_new"}, "FILE_ALREADY_EXISTS")

	// 穿透只读挂载：把 src/** 设为 read。
	envRead := *env
	envRead.Namespace = &types.Namespace{AgentID: "agent-1", Mounts: []types.Mount{
		{Pattern: "**", Mode: types.PathWrite},
		{Pattern: "src/**", Mode: types.PathRead},
	}}
	wantFailure(t, FileWrite, &envRead, map[string]any{"path": "src/a.go", "content": "x"}, ErrPathReadonly)
}

func TestFileWriteCreateParentDir(t *testing.T) {
	env, root := newTestEnv(t)
	if _, err := FileWrite.Execute(context.Background(), map[string]any{"path": "deep/nest/f.txt", "content": "x"}, env); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deep", "nest", "f.txt")); err != nil {
		t.Fatalf("parent not created: %v", err)
	}
}
