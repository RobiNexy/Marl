package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/RobiNexy/Marl/internal/types"
)

// TestConcurrentAppend 两 Agent 并发追加（MessageLog 契约：必须支持多 Agent
// 并发追加）。真机 fork 测试里暴露过 SQLITE_BUSY，这里是它的最小复现。
func TestConcurrentAppend(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, agent := range []types.AgentID{"a", "b"} {
		wg.Add(1)
		go func(id types.AgentID) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				e := types.NewLogEntry(id, types.RoleUserInput, "x")
				if _, err := s.Append(ctx, e); err != nil {
					errs <- err
					return
				}
			}
		}(agent)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
}
