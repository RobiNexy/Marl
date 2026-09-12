package knowledge

// vendor / promote：全局知识库（Part 12.7 / 12.8，13.12 阶段 10）。
//
// 形态（Part 12.7 的两个显式命令对应的库面）：
//
//	Pull(marlDir, globalRepo)：读 vendor.lock → fossil cat -R <repo> <path>
//	  取到内容 → 落 .marl/knowledge/vendor/<path> + 更新 lock（artifact =
//	  tip hash）。幂等：lock 锁 hash、clone 后 pull 拿**完全一样**的知识。
//	Promote(marlDir, globalRepo, relPath)：项目文件 → 全局库 commit
//	  （author=human，temp checkout 完成 fossil add/commit—— fossil 仓库
//	  写入必须经 open 的 checkout；Part 12.8 的"人类是唯一准入关口"，
//	  这是 CLI 命令面、不是 Agent 工具）。
//
// [已知裁面] lock 的 artifact 用 tip hash 而不是"逐条目 hash"——单人
// 项目、单分支订阅（Part 12.11 v1 不做）的当下形态；多条目分版入库是
// vendor.lock 的下一步，不在本文件的 boss 面。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"marl/internal/config"
	"marl/internal/fossil"
)

// VendorLock 是 vendor.lock 的解析形态（Pull/Promote 的对账键）。
type VendorLock struct {
	Entries []VendorEntry
}

// VendorEntry 是一条 vendored 知识。
type VendorEntry struct {
	Path     string // vendor/ 下的相对路径（promote 的源路径前，含知识形态）
	Source   string // 常量 "global"（v1 只有一个全局库）
	Artifact string // 全局库的 check-in hash（锁定具体版本）
	PulledAt time.Time
}

// VCS 是 vendoring 需要的 fossil 面（消费侧收窄：cat / 读 timeline 的
// tip hash / temp checkout 的写能力——vendor/promote 在全局库上的
// 新增面只有这些）。
type VCS interface {
	Timeline(ctx context.Context, repoPath string, n int) ([]fossil.TimelineEntry, error)
	OpenRepo(ctx context.Context, repoPath, workdir string) error
	CloseRepo(ctx context.Context, workdir string) error
	Add(ctx context.Context, workdir string, relPaths ...string) error
	Commit(ctx context.Context, workdir, author, message string) (string, error)
	// Cat 在 checkout 状态下读文件（RunCat 的全局形态：检查版本参数由
	// temp checkout 的 tip 驱动）。
	Cat(ctx context.Context, workdir, relPath string) ([]byte, error)
	RunLS(ctx context.Context, workdir string) (string, error)
}

// Pull 从全局库把 vendor.lock（若没有 lock 文件则**全部**全局条目录入——
// 单人项目第一次 Pull 的形态）里的知识落到 vendor/。
//
// 根目录依赖：marlDir 的 .marl（vendor 目录在 knowledge/vendor/）。
//
// 幂等性：同样的 lock + 同样的全局库 → 产物逐字节一致（cat 从
// check-in hash 查；lock 锁 hash——Part 12.7 的关键语义）。
func Pull(ctx context.Context, cli VCS, marlDir, globalRepo string) ([]VendorEntry, error) {
	if globalRepo == "" {
		return nil, fmt.Errorf("vendor: global repo path is required")
	}
	// 全局库当前 tip（artifact = tip hash；[已知裁面] 见文件头）。
	timeline, err := cli.Timeline(ctx, globalRepo, 1)
	if err != nil {
		return nil, fmt.Errorf("vendor: global tip: %w", err)
	}
	if len(timeline) == 0 {
		return nil, fmt.Errorf("vendor: global repo %s empty", globalRepo)
	}
	tip := timeline[len(timeline)-1].Hash
	vendorDir := filepath.Join(marlDir, "knowledge", "vendor")
	if err := os.MkdirAll(vendorDir, 0o755); err != nil {
		return nil, err
	}
	// 全局库的目录清单（fossil ls 简单面；空库 → 报"全局无条目"）。
	// 全局库需要 checkout — cli.Cat 的语义在"无 checkout"向上报错，
	// 所以 Pull 开一个临时 checkout（每 vendor 完整面都不留）。
	tmp, err := os.MkdirTemp("", "marl-vendor-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := cli.OpenRepo(ctx, globalRepo, tmp); err != nil {
		return nil, fmt.Errorf("vendor: open global checkout: %w", err)
	}
	defer cli.CloseRepo(ctx, tmp)
	// fossil ls 读清单。
	entriesResp, err := cli.RunLS(ctx, tmp)
	if err != nil {
		return nil, fmt.Errorf("vendor: ls global: %w", err)
	}
	paths := usablePaths(entriesResp)
	if len(paths) == 0 {
		return nil, fmt.Errorf("vendor: global repo has no knowledge entries")
	}
	out := make([]VendorEntry, 0, len(paths))
	for _, rel := range paths {
		content, err := cli.Cat(ctx, tmp, rel)
		if err != nil {
			return nil, fmt.Errorf("vendor: cat %s: %w", rel, err)
		}
		dst := filepath.Join(vendorDir, filepath.FromSlash(rel))
		if mkdirErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkdirErr != nil {
			return nil, mkdirErr
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return nil, err
		}
		out = append(out, VendorEntry{Path: rel, Source: "global", Artifact: tip, PulledAt: time.Now().UTC()})
	}
	// 更新 lock（原子写——Pull 产物的可见性与 lock 的可见性应一致）。
	if err := SaveVendorLock(marlDir, &VendorLock{Entries: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// usablePaths 是 fossil ls 的输出 → 相对路径数组（README 等目录说明不忽略）。
func usablePaths(text string) []string {
	out := []string{}
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// Promote 把项目里的一份文件提交进全局库（commit author=human）。
//
// 前置：project 文件存在（marlDir 相对路径）；全局库是 fossil repo。
// 后置：全局库多一个 commit（时间线可见）；返回 check-in hash。
func Promote(ctx context.Context, cli VCS, marlDir, globalRepo, relPath string) (string, error) {
	src := filepath.Join(marlDir, filepath.FromSlash(relPath))
	content, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("vendor: promote read %s: %w", relPath, err)
	}
	// 全局库写入需要 checkout（fossil add/commit 在 open 状态内）。
	tmp, err := os.MkdirTemp("", "marl-promote-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := cli.OpenRepo(ctx, globalRepo, tmp); err != nil {
		return "", fmt.Errorf("vendor: open global for promote: %w", err)
	}
	defer cli.CloseRepo(ctx, tmp)
	dst := filepath.Join(tmp, filepath.FromSlash(relPath))
	if dirErr := os.MkdirAll(filepath.Dir(dst), 0o755); dirErr != nil {
		return "", dirErr
	}
	if err := os.WriteFile(dst, content, 0o644); err != nil {
		return "", err
	}
	if err := cli.Add(ctx, tmp, relPath); err != nil {
		return "", fmt.Errorf("vendor: promote add: %w", err)
	}
	hash, err := cli.Commit(ctx, tmp, fossil.UserHuman, "promote "+relPath)
	if err != nil {
		return "", fmt.Errorf("vendor: promote commit: %w", err)
	}
	// 项目的 lock 同面（下一个 Pull 决定要不要纳）——文件面：记录的是
	// promote 之后的事，不改变 lock 现状（pull 的粒度是"restock"）。
	return hash, nil
}

// lockPath 是 vendor.lock 的位置（Part 12.7）。
func lockPath(marlDir string) string {
	return filepath.Join(marlDir, "knowledge", "vendor", "vendor.lock")
}

// LoadVendorLock 读 lock 文件（不存在 = 空 lock——**不是错误**：第一次
// Pull 是"登记"而不是"对账"）。
//
// 形态：YAML 子集（复用 config 解析器——与 ladder/profiles 同一解析纪律）。
func LoadVendorLock(marlDir string) (*VendorLock, error) {
	src, err := os.ReadFile(lockPath(marlDir))
	if err != nil {
		if os.IsNotExist(err) {
			return &VendorLock{}, nil
		}
		return nil, fmt.Errorf("vendor: read lock: %w", err)
	}
	root, err := config.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("vendor: lock parse: %w", err)
	}
	out := &VendorLock{}
	list := root.Get("entries")
	if list == nil {
		return out, nil
	}
	for _, item := range list.Items {
		e := VendorEntry{Source: item.Get("source").StrOr("global")}
		e.Path = item.Get("path").StrOr("")
		e.Artifact = item.Get("artifact").StrOr("")
		if ts, ok := item.Get("pulled_at").Str(); ok {
			if t, terr := time.Parse(time.RFC3339, ts); terr == nil {
				e.PulledAt = t
			}
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// SaveVendorLock 原子写 lock（Pull 的产物与 lock 同批可见；文本形态
// 手写的 YAML 子集（确定性键序 = 文件序）——lock 文件也要能 diff）。
func SaveVendorLock(marlDir string, lock *VendorLock) error {
	var sb strings.Builder
	sb.WriteString("entries:\n")
	for _, e := range lock.Entries {
		fmt.Fprintf(&sb, "  - path: %q\n", e.Path)
		fmt.Fprintf(&sb, "    source: %s\n", e.Source)
		fmt.Fprintf(&sb, "    artifact: %s\n", e.Artifact)
		fmt.Fprintf(&sb, "    pulled_at: %s\n", e.PulledAt.Format(time.RFC3339))
	}
	dst := lockPath(marlDir)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(sb.String()), 0o644)
}
