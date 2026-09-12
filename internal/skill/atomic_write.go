package skill

// 原子写模块（沿用 old_skills/atomic.go 的实现与注释骨架；注释即契约）。
//
// 崩溃一致性：rename 前崩溃留下 ".tmp-*" 残留（无害）；rename 后目标要么
// 旧内容要么新内容，不存在半写状态——这是"原子性"技能契约的物理保证。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic temp + fsync + rename（POSIX 原子替换）。
// temp 必须与目标同目录：保证 rename 在同一文件系统内完成，不会退化为拷贝。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicWrite %q: create temp: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if err = tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite %q: chmod: %w", path, err)
	}
	if _, err = tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite %q: write: %w", path, err)
	}
	if err = tmp.Sync(); err != nil { // fsync：rename 指向的内容必须已落盘
		cleanup()
		return fmt.Errorf("atomicWrite %q: sync: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomicWrite %q: close: %w", path, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomicWrite %q: rename: %w", path, err)
	}
	return nil
}

// writeFileExclusive O_EXCL 防意外覆盖（file_write mode=create_new 语义）。
func writeFileExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errBusiness{code: "FILE_ALREADY_EXISTS", msg: fmt.Sprintf("file %q already exists (mode=create_new)", path)}
		}
		return fmt.Errorf("writeExclusive %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writeExclusive %q: %w", path, err)
	}
	return f.Sync()
}
