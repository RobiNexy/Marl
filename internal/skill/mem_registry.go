package skill

// MemRegistry 是 Registry 的内存实现（13.4 阶段 2）。
//
// 稳定输出（Registry 契约）的实现方式：
//   - Names 按 SkillNames() 的固定序（不按注册顺序、不遍历 map）；
//   - Schemas 的每个 ToolSchema 直接取自 Skill 的三大纯函数产出，按
//     SkillNames() 排序；同一技能任何时刻产出逐字节一致（byte-stable
//     由各 Skill 的 Parameters 契约保证，注册表只负责不重排字节）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"marl/internal/types"
)

// MemRegistry 实现本包的 Registry 接口。
//
// 并发：注册（启动期，单 goroutine）与查询（运行期，多 goroutine）由
// sync.RWMutex 分隔。[权衡: 曾考虑"注册完成后 Seal() 冻结"的不可变纪律代替
// 锁——锁的写半边在生命周期里只活跃几微秒，而"冻结纪律"的正确性依赖
// 调用方自觉（测试里也会诱发并发 Register）；锁是可验证的那一半。]
type MemRegistry struct {
	mu sync.RWMutex
	by map[string]Skill
}

// NewMemRegistry 构造一个空注册表。
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{by: map[string]Skill{}}
}

// Register 实现 Registry.Register（注册期单 goroutine，错误即启动期致命）。
func (r *MemRegistry) Register(s Skill) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := s.Name()
	legal := false
	for _, n := range SkillNames() {
		if n == name {
			legal = true
			break
		}
	}
	switch {
	case name == "":
		return fmt.Errorf("registry: skill name empty")
	case !legal:
		return fmt.Errorf("registry: skill %q not in the 20-skill contract list", name)
	case !s.Kind().Valid():
		return fmt.Errorf("registry: skill %q kind %q invalid", name, s.Kind())
	case len(s.Parameters()) == 0 || !json.Valid(s.Parameters()):
		return fmt.Errorf("registry: skill %q parameters invalid", name)
	}
	if _, ok := r.by[name]; ok {
		return fmt.Errorf("registry: duplicate skill %q", name)
	}
	r.by[name] = s
	return nil
}

// Get 实现 Registry.Get。失败：未注册 → error（绝不 (nil, nil)）。
func (r *MemRegistry) Get(name string) (Skill, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.by[name]
	if !ok {
		return nil, fmt.Errorf("registry: skill %q not registered", name)
	}
	return s, nil
}

// Names 实现 Registry.Names（稳定顺序）。
func (r *MemRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := SkillNames()
	names := make([]string, 0, len(r.by))
	for _, n := range all {
		if _, ok := r.by[n]; ok {
			names = append(names, n)
		}
	}
	return names
}

// Schemas 实现 Registry.Schemas（逐字节稳定）。
//
// [推断] json.RawMessage 直接复制引用（不深拷贝）：Schemas 的消费点
// （Normalizer、冻结前缀生成）只读不写；Skill.Parameters 的契约"同一
// 技能任何时候返回的内容逐字节一致"意味着共享底层数组是安全的。若将来
// 出现"运行时生成 schema"的实现，本处仍应保持只读约定，而不复制两次。
func (r *MemRegistry) Schemas() []ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := SkillNames()
	out := make([]ToolSchema, 0, len(r.by))
	for _, n := range all {
		s, ok := r.by[n]
		if !ok {
			continue
		}
		out = append(out, ToolSchema{Name: n, Description: s.Description(), Parameters: s.Parameters()})
	}
	return out
}

// NewAuthorizer 构造阶段 2 的 Authorizer（最小形态：存在性 + 白名单）。
//
// 命名空间与深度的完整授权（错误码全部在技能路径内部处理）属阶段 3+；
// 阶段 2 的边界是 Part 6.4 修正 1 的直接推论：mini 无 Profile，白名单为
// nil = 全部允许。allowed 有值 → 白名单（Part 6.11 不支持黑名单）；
// 失败包装 ErrNotRegistered / ErrNotAllowed 哨兵，错误可机械翻译。
func NewAuthorizer(reg Registry, allowed []string) Authorizer {
	return authorizerFunc(func(name string) error {
		if _, err := reg.Get(name); err != nil {
			return fmt.Errorf("skill %q: %w: %v", name, ErrNotRegistered, err)
		}
		if !Allowed(allowed, name) {
			return fmt.Errorf("skill %q: %w (not in allowlist)", name, ErrNotAllowed)
		}
		return nil
	})
}

// authorizerFunc 让闭包满足接口。
type authorizerFunc func(name string) error

func (f authorizerFunc) Authorize(_ context.Context, _ types.AgentID, name string) error {
	return f(name)
}
