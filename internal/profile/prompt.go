package profile

// PromptIndex：读 prompts/_index.yaml，按 id 取整个 md 文件（Part 6.2 / 6.12）。
//
// 框架不解析提示词内容（Part 6.1）：ReadPrompt 返回的就是文件字节，
// 修改的入口只在人类手里（进版本控制，Fossil 管历史）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"marl/internal/config"
	"marl/internal/types"
)

// PromptStore 是提示词索引的文件实现（满足 types.PromptIndex）。
//
// 零值契约：零值不可用（base 为空 → 所有 ReadPrompt 失败），必须经
// LoadPromptIndex 构造。
type PromptStore struct {
	base    string              // .marl 目录（索引里的相对路径以此为基准）
	entries []types.PromptEntry // 按 ID 排序（List 的确定性）
}

// LoadPromptIndex 读 marlDir/prompts/_index.yaml 并校验索引。
//
// 失败：索引文件不可读 / YAML 非法 / 条目缺 id 或缺 path / path 越界
// （含 `..` 或绝对路径——索引是可控面，但路径注入还处在提示词文件的
// 攻击面上；边界防御与 ns.Resolver 同一纪律）。
//
// 条目的 path 相对于 .marl 目录（骨架形态 "prompts/default.md"）。
func LoadPromptIndex(marlDir string) (*PromptStore, error) {
	idxPath := filepath.Join(marlDir, "prompts", "_index.yaml")
	src, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, fmt.Errorf("prompt index: read %s: %w", idxPath, err)
	}
	root, err := config.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("prompt index: %s: %w", idxPath, err)
	}
	entries := root.Get("prompts")
	if entries == nil || entries.Kind != config.KindList {
		return nil, fmt.Errorf("prompt index: %s: expected top-level prompts list", idxPath)
	}
	out := make([]types.PromptEntry, 0, len(entries.Items))
	for i, it := range entries.Items {
		id, ok := it.Get("id").Str()
		if !ok || id == "" {
			return nil, fmt.Errorf("prompt index: %s: entries[%d]: id is required", idxPath, i)
		}
		path, ok := it.Get("path").Str()
		if !ok || path == "" {
			return nil, fmt.Errorf("prompt index: %s: entries[%d] (%s): path is required", idxPath, i, id)
		}
		if filepath.IsAbs(path) || containsDotDot(path) {
			return nil, fmt.Errorf("prompt index: %s: entries[%d] (%s): path %q must be relative to %s", idxPath, i, id, path, marlDir)
		}
		out = append(out, types.PromptEntry{ID: id, Path: path, Description: it.Get("description").StrOr("")})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	// 同名 id 在索引里是笔误形态：加载期报错（运行时按名字找，天然
	// 静默落在"第一个匹配"的行为会导致谁的提示词被用不可归因）。
	for i := 1; i < len(out); i++ {
		if out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("prompt index: duplicate prompt id %q", out[i].ID)
		}
	}
	return &PromptStore{base: marlDir, entries: out}, nil
}

// List 返回索引条目（按 ID 排序的副本）。
func (p *PromptStore) List() []types.PromptEntry {
	out := make([]types.PromptEntry, len(p.entries))
	copy(out, p.entries)
	return out
}

// ReadPrompt 返回提示词文件的全部内容（不解析、不裁剪）。
//
// 失败：id 不在索引 / 文件不可读。
func (p *PromptStore) ReadPrompt(id string) (string, error) {
	for _, e := range p.entries {
		if e.ID == id {
			b, err := os.ReadFile(filepath.Join(p.base, filepath.FromSlash(e.Path)))
			if err != nil {
				return "", fmt.Errorf("prompt index: read %s (id %q): %w", e.Path, id, err)
			}
			return string(b), nil
		}
	}
	return "", fmt.Errorf("prompt index: unknown prompt id %q", id)
}

// containsDotDot 报告路径片段里是否有 `..`（索引路径的越界防御）。
func containsDotDot(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../")
}
