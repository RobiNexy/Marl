package profile

// Profile 加载与继承的契约测试（13.9 测试矩阵）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RobiNexy/Marl/internal/types"
)

// writeProfile 是测试素材写入器（多个文件、独立小场景）。
func writeProfile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, dir string) *Loader {
	t.Helper()
	l := NewLoader()
	if err := l.LoadAll(dir); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return l
}

// TestLoadAndDefaults：基础解析 + TimeoutMs 物化（零值契约：0 是陷阱）。
func TestLoadAndDefaults(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a.yaml", `profile:
  id: "base"
  description: "基础"
  requirement:
    require: [tool_call]
  can_spawn: true
`)
	l := mustLoad(t, dir)
	p, err := l.Get("base")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case p.ID != "base":
		t.Fatalf("id = %q", p.ID)
	case len(p.Requirement.Require) != 1 || p.Requirement.Require[0] != types.CapToolCall:
		t.Fatalf("require = %v", p.Requirement.Require)
	case p.Sampling.TimeoutMs != defaultTimeoutMs:
		t.Fatalf("TimeoutMs = %d, want materialized default %d", p.Sampling.TimeoutMs, defaultTimeoutMs)
	case p.Task.Budget.TimeoutMs != defaultTaskTimeoutMs:
		t.Fatalf("task TimeoutMs = %d, want %d", p.Task.Budget.TimeoutMs, defaultTaskTimeoutMs)
	case !p.CanSpawn:
		t.Fatal("can_spawn lost")
	}
	// List 稳定（ID 排序明确，与文件序无关）。
	if got := l.List()[0].PromptID; got != "" {
		t.Fatalf("List PromptID = %q", got)
	}
}

// TestInheritanceMerge：Part 6.6 规则的每种字段类别。
func TestInheritanceMerge(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "base.yaml", `profile:
  id: "base"
  requirement:
    require: [tool_call]
    prefer: [thinking]
  sampling:
    temperature: 0.2
    max_tokens: 8000
    timeout_ms: 111
  outgoing_context:
    include_roles:
      - user_input
      - shared_memory
    max_entries: 5
  allowed_skills:
    - file_read
    - file_write
  can_spawn: true
  prompt: "base-prompt"
`)
	writeProfile(t, dir, "reviewer.yaml", `profile:
  id: "reviewer"
  extends: "base"
  prompt: "code-reviewer"
  sampling:
    temperature: 0.1     # 覆盖 base 的 0.2（其余无 → 继承）
  outgoing_context:
    include_roles:
      - shared_memory    # 重复 → 去重
      - human_note       # 新增
  allowed_skills:
    - list_dir
    - file_read
`)
	l := mustLoad(t, dir)
	p, err := l.Get("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// 标量：子覆盖。
	if p.Sampling.Temperature != 0.1 {
		t.Fatalf("temperature = %v, want child's 0.1", p.Sampling.Temperature)
	}
	// 继承：子未写 max_tokens / timeout → 父的。
	if p.Sampling.MaxTokens != 8000 || p.Sampling.TimeoutMs != 111 {
		t.Fatalf("sampling inherited wrong: %+v", p.Sampling)
	}
	// Require 并集去重：base 的 tool_call 在前。
	if len(p.Requirement.Require) != 1 || p.Requirement.Require[0] != types.CapToolCall {
		t.Fatalf("require = %v", p.Requirement.Require)
	}
	// Prefer 并集：父的在前、无重复。
	if len(p.Requirement.Prefer) != 1 || p.Requirement.Prefer[0] != types.CapThinking {
		t.Fatalf("prefer = %v", p.Requirement.Prefer)
	}
	// allowed_skills 非空 → 完全覆盖（不合并）。
	if len(p.AllowedSkills) != 2 {
		t.Fatalf("allowed_skills = %v", p.AllowedSkills)
	}
	// IncludeRoles 合并去重：父顺序 + 新增。
	want := []types.InternalRole{"user_input", "shared_memory", "human_note"}
	if len(p.OutgoingContext.IncludeRoles) != len(want) {
		t.Fatalf("include_roles = %v", p.OutgoingContext.IncludeRoles)
	}
	for i := range want {
		if p.OutgoingContext.IncludeRoles[i] != want[i] {
			t.Fatalf("include_roles[%d] = %v, want %v", i, p.OutgoingContext.IncludeRoles[i], want[i])
		}
	}
	// 标量继承：max_entries 未写 → 父的。
	if p.OutgoingContext.MaxEntries != 5 {
		t.Fatalf("max_entries = %d", p.OutgoingContext.MaxEntries)
	}
	// Prompt 子覆盖父（不合并）。
	if p.Prompt != "code-reviewer" {
		t.Fatalf("prompt = %q", p.Prompt)
	}
	// can_spawn 子未写 → 继承父 true。
	if !p.CanSpawn {
		t.Fatal("can_spawn inherited wrongly")
	}
}

// TestInheritanceEmptyListInherits：allowed_skills 空列表 = 未声明
// （Part 6.11：空 = 全部允许的语义不破坏继承）。
func TestInheritanceEmptyListInherits(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "base.yaml", `profile:
  id: "base"
  allowed_skills:
    - file_read
`)
	writeProfile(t, dir, "child.yaml", `profile:
  id: "child"
  extends: "base"
`)
	l := mustLoad(t, dir)
	p, _ := l.Get("child")
	if len(p.AllowedSkills) != 1 || p.AllowedSkills[0] != "file_read" {
		t.Fatalf("allowed_skills = %v (empty child list must inherit)", p.AllowedSkills)
	}
}

// TestInheritErrors：extends 缺失 / 环。
func TestInheritErrors(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
  extends: "b"
`)
	l := NewLoader()
	if err := l.LoadAll(dir); err != nil {
		t.Fatalf("LoadAll should not fail on unresolved extends (it's resolved on Get): %v", err)
	}
	if _, err := l.Get("a"); err == nil {
		t.Fatal("want error on extends to unknown profile")
	}
	// 环。
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
  extends: "a"
`)
	l2 := NewLoader()
	if err := l2.LoadAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.Get("a"); err == nil {
		t.Fatal("want error on self-extends cycle")
	}
}

// TestValidation：加载期 fail fast 的显式枚举。
func TestValidation(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "missing id",
			content: `profile:
  description: "无 id"
`,
			wantErr: "id is required",
		},
		{
			name: "unknown capability must error (not silently dropped)",
			content: `profile:
  id: "bad"
  requirement:
    require:
      - toolcall   # 拼写错误：宽容会让 Router 选出不支持该能力的模型
`,
			wantErr: "unknown capability",
		},
		{
			name: "duplicate id",
			content: `profile:
  id: "dup"
`,
			wantErr: "duplicate id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProfile(t, dir, "one.yaml", tc.content)
			writeProfile(t, dir, "two.yaml", tc.content)
			l := NewLoader()
			err := l.LoadAll(dir)
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.wantErr)
			}
			if !errors.Is(err, ErrNotFound) && !containsStr(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOfStr(s, sub) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestGetReturnsSnapshot：Get 返回副本——修改不击穿共享配置。
func TestGetReturnsSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
  allowed_skills:
    - file_read
`)
	l := mustLoad(t, dir)
	p, _ := l.Get("a")
	p.AllowedSkills = append(p.AllowedSkills, "file_write")
	p.CanSpawn = true
	p2, _ := l.Get("a")
	if len(p2.AllowedSkills) != 1 || p2.CanSpawn {
		t.Fatalf("Get must return a snapshot, got %+v", p2)
	}
}

// TestGetNotFound 哨兵。
func TestGetNotFound(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
`)
	l := mustLoad(t, dir)
	_, err := l.Get("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestEmptyDirIsError：无 profile 的目录是配置错误，不是空集。
func TestEmptyDirIsError(t *testing.T) {
	err := NewLoader().LoadAll(t.TempDir())
	if err == nil {
		t.Fatal("empty profiles dir must error")
	}
	// reload 同纪律。
	l := NewLoader()
	if err := l.LoadAll(t.TempDir()); err == nil {
		t.Fatal("empty dir LoadAll must error")
	}
	// Reload 无 LoadAll：显式错误。
	if _, err := l.Reload(); err == nil {
		t.Fatal("Reload without LoadAll must error")
	}
}

// TestReload：热重载的变更报告 + 失败保留。
func TestReload(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
  can_spawn: true
`)
	l := mustLoad(t, dir)
	// 无变化 reload：空列表。
	changed, err := l.Reload()
	if err != nil || len(changed) != 0 {
		t.Fatalf("reload no-change: changed=%v err=%v", changed, err)
	}
	// 修改配置 → 变更报告含 id。
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
  can_spawn: false
`)
	changed, err = l.Reload()
	if err != nil || len(changed) != 1 || changed[0] != types.ProfileID("a") {
		t.Fatalf("reload changed=%v err=%v", changed, err)
	}
	if p, _ := l.Get("a"); p.CanSpawn {
		t.Fatal("reload did not take effect")
	}
	// 写坏一个文件 → Reload 失败且旧配置保留。
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
  requirement:
    require: [bogus_cap]
`)
	if _, err := l.Reload(); err == nil {
		t.Fatal("reload with broken file must error")
	}
	if p, _ := l.Get("a"); p == nil {
		t.Fatal("failed reload must keep old profiles")
	}
}

// 计数器断言：Get 阻塞下的并发安全（-race 是默认通过条件）。
func TestGetConcurrent(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a.yaml", `profile:
  id: "a"
`)
	l := mustLoad(t, dir)
	var done []bool
	sync := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				_, _ = l.Get("a")
			}
			sync <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-sync
	}
	done = nil
	_ = done
	_ = context.Background()
}
