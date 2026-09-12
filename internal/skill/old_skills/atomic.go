//go:build ignore

package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ────────────────────────────── 快照模块 ──────────────────────────────
//
// 为什么不依赖 Fossil revert（Part 4.4.6）：Fossil revert 是 commit 级别的，
// 工作区未提交的改动没法 revert。快照是"修改前"的——相当于每一步都有 undo。
// M1 直接文件系统实现；M3 引入 Fossil 后本模块语义不变。

// snapshotName 将 workspace 相对路径扁平化为快照文件名前缀。
func snapshotName(absPath, workspaceRoot string) string {
	rel, err := filepath.Rel(workspaceRoot, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(absPath) // 沙箱外不该发生，兜底用文件名 [推断]
	}
	return strings.ReplaceAll(filepath.ToSlash(rel), "/", "_")
}

// snapshotFile 把当前文件内容复制到 SnapshotDir，返回快照绝对路径。
// 每个文件只保留最近 10 个快照，旧的自动清理。
func snapshotFile(ec *ExecContext, absPath string) (string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	prefix := snapshotName(absPath, ec.WorkspaceRoot)
	name := fmt.Sprintf("%s.%s.bak", prefix, time.Now().UTC().Format("20060102T150405.000000000"))
	dst := filepath.Join(ec.SnapshotDir, name)
	if err := os.MkdirAll(ec.SnapshotDir, 0o755); err != nil {
		return "", err
	}
	// 快照写入不用原子写：快照自身损坏不影响目标文件，代价可接受 [推断]
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	pruneSnapshots(ec.SnapshotDir, prefix, 10)
	return dst, nil
}

// listSnapshots 返回某文件的快照名列表，按时间戳升序。
// 文件名字典序即时间序——零填充定宽 UTC 时间戳保证了这一点。
func listSnapshots(dir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix+".") && strings.HasSuffix(name, ".bak") {
			out = append(out, name)
		}
	}
	sortStrings(out)
	return out, nil
}

// pruneSnapshots 只保留最新 keep 个快照。清理失败可容忍，下次触发再试。
func pruneSnapshots(dir, prefix string, keep int) {
	names, err := listSnapshots(dir, prefix)
	if err != nil || len(names) <= keep {
		return
	}
	for _, n := range names[:len(names)-keep] {
		os.Remove(filepath.Join(dir, n)) //nolint:errcheck // 见上
	}
}

// sortStrings 小切片插入排序：避免仅为排序引入 sort 的接口开销可读性争议，
// 这里纯粹是少一个依赖点；规模恒小（≤ 快照数上限）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ────────────────────────────── 原子写模块 ──────────────────────────────

// writeFileAtomic temp + fsync + rename（POSIX 原子替换）。
// temp 必须与目标同目录：保证 rename 在同一文件系统内完成，不会退化为拷贝。
// 崩溃一致性：rename 前崩溃留下 ".tmp-*" 残留（无害）；rename 后目标要么旧内容
// 要么新内容，不存在半写状态——这是"原子性"技能契约的物理保证。
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

// writeFileExclusive O_EXCL 防意外覆盖（create_new 语义）。
func writeFileExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if os.IsExist(err) {
			return errf("FILE_ALREADY_EXISTS", "file %q already exists (mode=create_new)", path)
		}
		return fmt.Errorf("writeExclusive %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writeExclusive %q: %w", path, err)
	}
	return f.Sync()
}

func writeFileAppend(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("writeAppend %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writeAppend %q: %w", path, err)
	}
	return f.Sync()
}
