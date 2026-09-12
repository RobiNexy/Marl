package skill

// file_write —— 全量写入（原子写）（Part 4.4）。
//
// 要点（延用 old_skills 的实现）：
//   - 原子写 temp + fsync + rename（atomic_write.go，物理保证见其注释）；
//   - 写前快照（仅对已存在的文件——新文件没有可保护的旧内容）；
//   - 相同内容的重复写不落盘（mtime 稳定 → watcher 不误触发）；
//   - 写入必须经 Resolver 的 types.PathWrite 判定（红线：能力约束代替惩罚）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"marl/internal/types"
)

type fileWriteSkill struct{}

// FileWrite 是 file_write 的共享实例。
var FileWrite Skill = &fileWriteSkill{}

func (s *fileWriteSkill) Name() string { return SkillFileWrite }
func (s *fileWriteSkill) Description() string {
	return "Write text content to a file in the workspace. Full-content write (atomic via temp+rename). Existing files are snapshotted before overwrite."
}
func (s *fileWriteSkill) Kind() SkillKind { return SkillMutating }

// Parameters 逐字节写死（冻结前缀，见 ToolSchema 契约）。
func (s *fileWriteSkill) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"path":{"type":"string","description":"Path relative to the workspace root"},` +
		`"content":{"type":"string","description":"Full file content (UTF-8 text)"},` +
		`"mode":{"type":"string","enum":["overwrite","create_new"],"description":"overwrite (default): replace content; create_new: fail if file exists"}},` +
		`"required":["path","content"]}`)
}

// Execute 实现 Skill.Execute。
func (s *fileWriteSkill) Execute(ctx context.Context, args map[string]any, env *SkillEnv) (*SkillResult, error) {
	res, err := resolvePath(env, strArg(args, "path", ""), types.PathWrite)
	if err != nil {
		return businessOf(err)
	}
	content, ok := args["content"].(string)
	if !ok {
		return NewFailure("BAD_ARGS", "content is required (string)"), nil
	}
	mode := strArg(args, "mode", "overwrite")
	switch mode {
	case "overwrite", "create_new":
	default:
		return NewFailure("BAD_ARGS", "invalid mode %q", mode), nil
	}

	// create_new：O_EXCL 检查在写入层做（writeFileExclusive），这里只挡掉
	// 明确已存在的情形（省一次系统调用往返）。
	if mode == "create_new" {
		if _, statErr := os.Stat(res.RealPath); statErr == nil {
			return NewFailure("FILE_ALREADY_EXISTS", "file %s exists (mode=create_new)", res.RealPath), nil
		}
	}

	// 父目录不存在时自动创建（ensure_parent 默认 true，old_skills 的语义）。
	if _, statErr := os.Stat(filepath.Dir(res.RealPath)); os.IsNotExist(statErr) {
		if mkErr := os.MkdirAll(filepath.Dir(res.RealPath), 0o755); mkErr != nil {
			return nil, fmt.Errorf("file_write %s: mkdir parent: %w", res.RealPath, mkErr)
		}
	}

	// 写前快照：仅对已存在文件（新文件没有可保护的旧内容）。
	oldData, statErr := os.ReadFile(res.RealPath)
	exists := statErr == nil
	var snapshot string
	if exists && env.Snapshots != nil {
		name, snapErr := env.Snapshots.Create(ctx, relFromWorkspace(env, res.RealPath), oldData)
		if snapErr != nil {
			// 快照失败 ≠ 写入失败：写入仍可进行，但必须在结果里让模型知道
			// 本次改动没有 undo 面（可观测性准则 4）。
			snapshot = fmt.Sprintf("snapshot failed: %v", snapErr)
		} else {
			snapshot = name
		}
	}

	// 相同内容不重复写：mtime 稳定 → 下游 watcher 不误触发（old_skills 的
	// 既有语义，完整保留）。
	if exists && mode == "overwrite" && string(oldData) == content {
		return &SkillResult{OK: true, Data: map[string]any{
			"mode": mode, "path": res.RealPath,
			"bytes_written": 0, "new_file": false, "changed": false, "snapshot": snapshot,
		}}, nil
	}

	perm := os.FileMode(0o644)
	if fi, statErr := os.Stat(res.RealPath); statErr == nil {
		perm = fi.Mode().Perm() // 保留原文件权限
	}

	switch mode {
	case "overwrite":
		if err := writeFileAtomic(res.RealPath, []byte(content), perm); err != nil {
			return nil, fmt.Errorf("file_write %s: %w", res.RealPath, err) // IO 故障 → 基础设施错误
		}
	case "create_new":
		if err := writeFileExclusive(res.RealPath, []byte(content), perm); err != nil {
			var eb errBusiness
			if errors.As(err, &eb) {
				return &SkillResult{OK: false, ErrorType: eb.code, Message: eb.msg}, nil
			}
			return nil, fmt.Errorf("file_write %s: %w", res.RealPath, err)
		}
	}

	return &SkillResult{OK: true, Data: map[string]any{
		"mode": mode, "path": res.RealPath,
		"bytes_written": len(content), "new_file": !exists, "changed": true, "snapshot": snapshot,
	}}, nil
}

// relFromWorkspace 由 env.ProjectRoot 取出"快照接口的 relPath 参数"形态
// （Snapshotter 契约要求 workspace 相对路径）。RealPath 已经过 Resolver，
// 若这里取不到相对路径属内部一致性破坏，宁可退化为 base 文件名
// （快照面上少一个目录层次）也不要让一次写盘失败。
func relFromWorkspace(env *SkillEnv, realPath string) string {
	rel, err := filepath.Rel(env.ProjectRoot, realPath)
	if err != nil || rel == ".." || filepath.IsAbs(rel) {
		return filepath.Base(realPath)
	}
	return filepath.ToSlash(rel)
}
