package agent

// agent 包的测试基建（拼装 sqlite store + resolver + 文件工作区）。

import (
	"marl/internal/types"
	"os"
	"path/filepath"
	"testing"

	"marl/internal/ns"
	"marl/internal/store"
)

// newStoreForTest 是 SQLite 后端的最小装配（文件在临时目录，随 t 结束清理）。
func newStoreForTest(t *testing.T) (*store.SQLiteStore, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { s.Close() })
	return s, nil
}

// workspaceRoot 返回工作区根（测试里既是 resolver 的锚也是 file_write 的基线）。
func workspaceRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// mustResolver 构造 ns.Resolver（root 必须存在）。
func mustResolver(t *testing.T, root string) types.Resolver {
	t.Helper()
	r, err := ns.NewResolver(root)
	if err != nil {
		t.Fatalf("ns.NewResolver: %v", err)
	}
	return r
}

// writeFile 在工作区里放一个文件（测试素材）。
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}
