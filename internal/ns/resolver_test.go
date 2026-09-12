package ns

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"marl/internal/types"
)

// 测试规格（契约来自 types.Resolver 与 Part 5.1/5.2/5.4）：
//
//   - 全覆盖挂载 "write:**" 之上再挂 "read:src/**"：
//     src 内可读不可写（能力序），src 外可读写；
//   - hidden 与更宽授权同等具体度冲突时 hidden 胜；
//   - ".." / 绝对路径 / 空路径 → ErrUnavailable（不区分成因）；
//   - 未识别 Mode → hidden（fail-closed）；
//   - symlink 指出根 → ErrUnavailable（写入不可逃逸）。

func testNS(tokens ...string) *types.Namespace {
	var ms []types.Mount
	for _, str := range tokens {
		var pattern, mode string
		switch str {
		case "w":
			pattern, mode = "**", "write"
		case "r":
			pattern, mode = "src/**", "read"
		case "h":
			pattern, mode = ".git/**", "hidden"
		case "zz":
			pattern, mode = "**", "" // 未识别的模式 → hidden（fail-closed）
		default:
			panic("unknown mount token")
		}
		ms = append(ms, types.Mount{Pattern: pattern, Mode: types.PathMode(mode)})
	}
	return &types.Namespace{AgentID: "agent-1", Mounts: ms}
}

func newTestResolver(t *testing.T) types.Resolver {
	t.Helper()
	root := t.TempDir()
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestResolverResolve(t *testing.T) {
	cases := []struct {
		name    string
		mounts  []string
		path    string
		need    types.PathMode
		wantErr error
		wantMod types.PathMode
	}{
		{"write under full write mount", []string{"w"}, "notes/a.md", types.PathWrite, nil, types.PathWrite},
		{"read under write mount suffices for read", []string{"w"}, "notes/a.md", types.PathRead, nil, types.PathWrite},
		{"write denied on read mount", []string{"r"}, "src/x.go", types.PathWrite, ErrReadonly, ""},
		{"read allowed on read mount", []string{"r"}, "src/x.go", types.PathRead, nil, types.PathRead},
		{"outside any mount hidden", []string{"r"}, "docs/x.md", types.PathRead, ErrUnavailable, ""},
		{"read mount covers src itself", []string{"r"}, "src", types.PathRead, nil, types.PathRead},
		{"parent traversal rejected", []string{"w"}, "../out", types.PathWrite, ErrUnavailable, ""},
		{"parent traversal masked", []string{"w"}, "a/../../b", types.PathRead, ErrUnavailable, ""},
		{"absolute rejected as unavailable", []string{"w"}, "/etc/passwd", types.PathRead, ErrUnavailable, ""},
		{"empty rejected", []string{"w"}, "", types.PathRead, ErrUnavailable, ""},
		{"unrecognized mode reads as hidden", []string{"zz"}, "x.txt", types.PathRead, ErrUnavailable, ""},
		{"hidden beats equal-specificity read", []string{"r", "h"}, ".git/config", types.PathRead, ErrUnavailable, ""},
		{"write on nil-compatible empty ns hidden", []string{}, "x.txt", types.PathRead, ErrUnavailable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestResolver(t)
			ns := testNS(tc.mounts...)
			got, err := r.Resolve(ns, tc.path, tc.need)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Resolve(%s): unexpected error %v", tc.path, err)
				}
				if got.Mode != tc.wantMod {
					t.Fatalf("mode = %q, want %q", got.Mode, tc.wantMod)
				}
				if !filepath.IsAbs(got.RealPath) {
					t.Fatalf("real path %q not absolute", got.RealPath)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Resolve(%s) error = %v, want %v", tc.path, err, tc.wantErr)
			}
		})
	}
}

func TestResolverSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	r, err := NewResolver(dir)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	workspace := types.Namespace{AgentID: "a", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}}}

	// 根外创建真实目标。
	outside := filepath.Join(t.TempDir(), "leak.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 根内一个指向根外的 symlink。
	if err := os.Symlink(outside, filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Resolve(&workspace, "leak", types.PathWrite); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("symlink escaping root must be unavailable, got err=%v", err)
	}
}

func TestResolverWriteTargetNotCreatedYet(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewResolver(dir)
	workspace := types.Namespace{AgentID: "a", Mounts: []types.Mount{{Pattern: "**", Mode: types.PathWrite}}}
	res, err := r.Resolve(&workspace, "sub/new.txt", types.PathWrite)
	if err != nil {
		t.Fatalf("write to not-yet-created file: %v", err)
	}
	if got, want := res.RealPath, filepath.Join(dir, "sub", "new.txt"); got != want {
		t.Fatalf("realpath = %q, want %q", got, want)
	}
}

func TestResolverCanWrite(t *testing.T) {
	r := newTestResolver(t)
	nsWrite := testNS("w")
	nsRead := testNS("r")
	if !r.CanWrite(nsWrite, "a.txt") {
		t.Fatal("write mount: canWrite must be true")
	}
	if r.CanWrite(nsRead, "src/a.go") {
		t.Fatal("read mount: canWrite must be false")
	}
	if r.CanWrite(nsRead, "other") {
		t.Fatal("uncovered: canWrite must be false")
	}
}
