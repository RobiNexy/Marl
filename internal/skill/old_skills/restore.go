//go:build ignore

package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ──────────────────────────── restore_snapshot ─────────────────────────────
//
// 工作区快照恢复（Part 4.4.6）。capability=mutating 但本质是 undo：
// 恢复前先对当前文件再拍一次快照，保证"恢复"本身也可撤销（退路优先于功能）。

type restoreSnapshotSkill struct{}

func (s *restoreSnapshotSkill) Name() string { return "restore_snapshot" }
func (s *restoreSnapshotSkill) Description() string {
	return "Restore a file from a snapshot created by file_write or file_edit."
}
func (s *restoreSnapshotSkill) Capability() Capability { return CapMutating }

func (s *restoreSnapshotSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	path, err := ec.Resolve(strArg(args, "path", ""))
	if err != nil {
		return nil, err
	}
	if err := ec.checkWritable(path); err != nil {
		return nil, err
	}
	prefix := snapshotName(path, ec.WorkspaceRoot)
	names, err := listSnapshots(ec.SnapshotDir, prefix)
	if err != nil {
		return nil, fmt.Errorf("restore_snapshot: %w", err)
	}
	if len(names) == 0 {
		return nil, errf("NO_SNAPSHOT", "no snapshots for %q", path)
	}

	wantID := strArg(args, "snapshot_id", "")
	snapName := names[len(names)-1] // 默认最新
	if wantID != "" {
		found := false
		for _, n := range names {
			// 接受完整快照名或时间戳片段两种写法
			if n == wantID || strings.HasPrefix(n, prefix+"."+wantID) {
				snapName = n
				found = true
				break
			}
		}
		if !found {
			return nil, errf("SNAPSHOT_NOT_FOUND",
				"snapshot %q not found for %q (have %d)", wantID, path, len(names))
		}
	}

	data, err := os.ReadFile(filepath.Join(ec.SnapshotDir, snapName))
	if err != nil {
		return nil, fmt.Errorf("restore_snapshot: read snapshot: %w", err)
	}

	if boolArg(args, "preview", false) {
		preview := string(data)
		if len(preview) > 4000 { // 与 escalation 回调同量级的预览上限 [推断]
			preview = preview[:4000] + "\n...(truncated)"
		}
		return Result("path", path, "snapshot", snapName,
			"content", preview, "restored", false), nil
	}

	// 先给当前内容留快照：undo 必须可再 undo
	if _, err := snapshotFile(ec, path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("restore_snapshot: snapshot current: %w", err)
	}
	perm := os.FileMode(0o644)
	if fi, statOk := os.Stat(path); statOk == nil {
		perm = fi.Mode().Perm()
	}
	if err := writeFileAtomic(path, data, perm); err != nil {
		return nil, fmt.Errorf("restore_snapshot: write: %w", err)
	}
	return Result("path", path, "snapshot", snapName,
		"bytes_written", len(data), "restored", true), nil
}
