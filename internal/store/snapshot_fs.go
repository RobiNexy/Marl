package store

// SnapshotStore 的文件系统实现（13.4 阶段 2：file_write 的"写前快照"）。
//
// 文件名形态：<rel_path 内 '/' 换成 '_'>.<毫秒时间戳>.bak，落在
// root/.marl/snapshots/ 下（不进版本控制；Part 8.4 保留最近 10 个）。
// 文件名内编码了原路径与毫秒时间戳：肉眼可查，且字符串排序即时间排序。
//
// 安全契约（本实现最重要的部分，见 SnapshotStore 接口注释）：
//   - Create 的 relPath 必须解析到 workspace 内——把系统文件复制进快照
//     目录不只是越界读：快照目录随 .marl 打包/上报时，里面的内容就是
//     泄露面；
//   - Restore 的 snapshotPath 必须是 Create 产出的形态且解析后落在
//     snapshots/ 内——Create 越界是读出，Restore 越界是**任意路径写**。
//
// [权衡: 快照名用"路径内的 / 替换为 _"这种有损映射（a/b 与 a_b 冲突）。
// 最坏后果是两个文件共享 prune 序列，个别多余快照被提前清掉；正确性
// 无损，换来零 schema、零旁档文件。]

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// snapshotName 是 root/.marl/snapshots 下相对路径引用的唯一合法形态
// （Restore 的防线之一：允许任意文件名等于允许伪造"指向 snapshots/ 内
// 任意文件"的引用）。
var snapshotName = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)\.[0-9]{14}\.bak$`)

// NewSnapshotStore 构造快照存储（快照目录 = root/.marl/snapshots，
// Part 4.4 的目录布局；.marl 在任何 Agent 的 namespace 里都是 hidden，
// 快照目录因此天然处于所有 Agent 的可见范围之外）。
func NewSnapshotStore(root string) (SnapshotStore, error) {
	dir := filepath.Join(root, ".marl", "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshots: mkdir %s: %w", dir, err)
	}
	return &fileSnapshots{root: root, dir: dir}, nil
}

// fileSnapshots 实现 store.SnapshotStore（文件名与目录布局见文件头）。
type fileSnapshots struct {
	root string // 工作区根，relPath 相对于它
	dir  string // root/.marl/snapshots，快照文件的唯一存放点
}

// Create 对 relPath 当前内容做快照，返回快照文件名（Restore 的"门票"）。
//
// relPath 由技能层经 Resolver 传来，这里保留两道剩余校验（root 内 + 未
// 越界）：防御冗余的成本是一行的 Rel 检查，收益是 Create 不依赖调用方
// 是否真的过了 Resolver。写入目标是 snapshots/ 内自己的文件，不再触碰
// 原路径。
func (f *fileSnapshots) Create(ctx context.Context, relPath string, content []byte) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("snapshot: %w: empty relPath", ErrInvalid)
	}
	clean := filepath.ToSlash(filepath.Clean(relPath))
	if !pathInsideRoot(clean) {
		return "", fmt.Errorf("snapshot %q: %w", relPath, ErrInvalid)
	}
	name := snapshotNameOf(clean, time.Now().UnixMilli())
	if err := os.WriteFile(filepath.Join(f.dir, name), content, 0o600); err != nil {
		return "", fmt.Errorf("snapshot %s: write: %w", clean, err)
	}
	return name, nil
}

// Restore 把快照内容写回原位。
//
// 校验顺序（缺一不可）：形态校验（正则白名单）→ symlink 消解 → 落在
// snapshots/ 内复查。三道合成"白名单引用"语义：不是 Create 产出的形态
// 进不来；即便出现同名伪造文件，也必须落在 snapshots/ 目录之内。
func (f *fileSnapshots) Restore(ctx context.Context, snapshotPath string) error {
	if snapshotPath == "" {
		return fmt.Errorf("snapshot: %w: empty path", ErrInvalid)
	}
	if !snapshotName.MatchString(snapshotPath) {
		return fmt.Errorf("snapshot %q: %w: not a snapshot reference this store produced",
			snapshotPath, ErrInvalid)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(f.dir, snapshotPath))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("snapshot %s: %w", snapshotPath, ErrNotFound)
		}
		return fmt.Errorf("snapshot %s: resolve: %w", snapshotPath, err)
	}
	if rel, relErr := filepath.Rel(f.dir, real); relErr != nil || filepath.IsAbs(rel) ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("snapshot %q: %w: resolved outside snapshots dir", snapshotPath, ErrInvalid)
	}
	content, err := os.ReadFile(real)
	if err != nil {
		return fmt.Errorf("snapshot %s: read: %w", snapshotPath, err)
	}
	origin, err := snapshotOriginOf(snapshotPath)
	if err != nil {
		return err
	}
	// 还原出的目标路径同样不得越界。这层检查与 Create 不同：还原的计算
	// 链未必与 Create 对称（见 snapshotOriginOf 的注释），在写路径上多查
	// 一次比信任签名便宜。
	if !pathInsideRoot(origin) {
		return fmt.Errorf("snapshot %q: %w: origin escapes root", snapshotPath, ErrInvalid)
	}
	target := filepath.Join(f.root, filepath.FromSlash(origin))
	if err := os.WriteFile(target, content, 0o644); err != nil {
		return fmt.Errorf("snapshot %s: write %s: %w", snapshotPath, target, err)
	}
	return nil
}

// Prune 把每个文件的快照修剪到最近 maxPerFile 个（目录容量闸）。
func (f *fileSnapshots) Prune(ctx context.Context, maxPerFile int) error {
	if maxPerFile <= 0 {
		// 0 不是"清空"。清空需显式 API；"漏传参数"必须被拒绝而非清空。
		return fmt.Errorf("prune: %w: maxPerFile must be positive", ErrInvalid)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return fmt.Errorf("prune: read %s: %w", f.dir, err)
	}
	byOrigin := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !snapshotName.MatchString(e.Name()) {
			continue
		}
		origin, err := snapshotOriginOf(e.Name())
		if err != nil {
			continue // 不认识的文件不删：删除必须明确
		}
		byOrigin[origin] = append(byOrigin[origin], e.Name())
	}
	var failures []error
	for _, names := range byOrigin {
		sort.Strings(names) // 文件名内嵌定长毫秒 → 字符串序 = 时间序
		staleCount := len(names) - maxPerFile
		for i := 0; i < staleCount; i++ {
			if err := os.Remove(filepath.Join(f.dir, names[i])); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("remove %s: %w", names[i], err))
			}
		}
	}
	return errors.Join(failures...)
}

// snapshotNameOf 由干净相对路径 + 毫秒时间戳生成快照名（Create 的编码方向）。
func snapshotNameOf(relPath string, ms int64) string {
	id := strings.ReplaceAll(relPath, "/", "_")
	return fmt.Sprintf("%s.%014d.bak", id, ms)
}

// snapshotOriginOf 把快照名还原出原始相对路径（Create 的反向）。
//
// 已知的[有损]点：a/b 与 a_b 的快照名同为 a_b。最坏后果是 prune 按"
// 同一个源文件"计数——各自的时间序仍然内部一致，不会越界或数据串。
func snapshotOriginOf(name string) (string, error) {
	m := snapshotName.FindStringSubmatch(name)
	if m == nil {
		return "", fmt.Errorf("snapshot name %q: %w", name, ErrInvalid)
	}
	return strings.ReplaceAll(m[1], "_", "/"), nil
}

// pathInsideRoot 判定 slash 形式的相对路径是否留在 root 内。
// 与 ns.normalizeInRoot 的方向一致但独立实现（本包不依赖 ns）；两处
// 规则漂移的防线是各自包内测试 + 真实快照的 E2E 检查。
func pathInsideRoot(clean string) bool {
	return clean != "" && clean != "." && clean != ".." &&
		!strings.HasPrefix(clean, "../") && !strings.HasSuffix(clean, "/..")
}
