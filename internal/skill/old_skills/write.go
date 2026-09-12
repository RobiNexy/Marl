//go:build ignore

package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ────────────────────────────── file_write ──────────────────────────────
//
// 实现要点（Part 4.4.4）：原子写 temp+fsync+rename；保留权限；内容比较避免
// 相同内容重复写（mtime 不变，下游 watcher 不触发）；create_new 用 O_EXCL
// 防意外覆盖；写入前自动快照；路径所有权校验。

type fileWriteSkill struct{}

func (s *fileWriteSkill) Name() string { return "file_write" }
func (s *fileWriteSkill) Description() string {
	return "Write text content to a file. Atomic write via temp+rename. Never writes outside workspace."
}
func (s *fileWriteSkill) Capability() Capability { return CapMutating }

func (s *fileWriteSkill) Execute(ctx context.Context, ec *ExecContext, args map[string]any) (map[string]any, error) {
	path, err := ec.Resolve(strArg(args, "path", ""))
	if err != nil {
		return nil, err
	}
	if err := ec.checkWritable(path); err != nil {
		return nil, err
	}
	contentRaw, ok := args["content"]
	if !ok {
		return nil, errf("BAD_ARGS", "content is required")
	}
	content, ok := contentRaw.(string)
	if !ok {
		return nil, errf("BAD_ARGS", "content must be a string")
	}
	mode := strArg(args, "mode", "overwrite")
	switch mode {
	case "overwrite", "append", "create_new":
	default:
		return nil, errf("BAD_ARGS", "invalid mode %q", mode)
	}

	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if !boolArg(args, "ensure_parent", true) {
			return nil, errf("PARENT_MISSING", "parent dir %q does not exist and ensure_parent=false", dir)
		}
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, fmt.Errorf("file_write %q: mkdir parent: %w", path, mkErr)
		}
	}

	oldData, statErr := os.ReadFile(path)
	exists := statErr == nil

	// 内容比较：相同内容不重复写，保持 mtime 稳定
	if exists && mode == "overwrite" && string(oldData) == content {
		return Result("mode", mode, "path", path,
			"bytes_written", 0, "new_file", false, "changed", false), nil
	}

	// 写入前自动快照（仅对已存在文件）
	var snapshotPath string
	if exists {
		snapshotPath, err = snapshotFile(ec, path)
		if err != nil {
			return nil, fmt.Errorf("file_write %q: snapshot: %w", path, err)
		}
	}

	perm := os.FileMode(0o644)
	if fi, statOk := os.Stat(path); statOk == nil {
		perm = fi.Mode().Perm() // 保留原文件权限
	}

	newFile := !exists
	switch mode {
	case "overwrite":
		err = writeFileAtomic(path, []byte(content), perm)
	case "create_new":
		err = writeFileExclusive(path, []byte(content), perm)
	case "append":
		err = writeFileAppend(path, []byte(content))
	}
	if err != nil {
		return nil, err
	}

	res := Result("mode", mode, "path", path,
		"bytes_written", len(content),
		"new_file", newFile,
		"changed", true)
	if snapshotPath != "" {
		res["snapshot"] = filepath.Base(snapshotPath)
	}
	return res, nil
}
