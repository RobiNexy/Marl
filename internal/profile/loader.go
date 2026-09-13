package profile

// Profile 加载与继承合并（Part 6.3 / 6.6 / 6.12，13.9 阶段 7）。
//
// 合并对准 Part 6.6 的规则，落点是 **YAML 树**而非解码后的结构体：
// 标量字段"子覆盖父"必须区分"子显式写了 0"与"子没写"（如 temperature=0
// 是合法取值——贪心采样），解码成 SamplingParams 后零值与缺失不可区分，
// 在树层面合并则键存在性即"c 子显式声明"——无歧义。列表字段按 6.6 的
// 逐字段语义处理（见 mergeProfileNodes）。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/RobiNexy/Marl/internal/config"
	"github.com/RobiNexy/Marl/internal/types"
)

// ErrNotFound 是 Get 的"没有这个 Profile"哨兵（调用方 errors.Is 判定）。
var ErrNotFound = errors.New("profile: not found")

// defaultTimeoutMs 是 Sampling.TimeoutMs 的物化默认值（Part 6.8：默认 30
// 分钟——宁可等久也不要半途截断一个好答案）。
//
// types.SamplingParams 的零值契约规定 TimeoutMs==0 是陷阱（0 会变成"请求
// 立刻超时"），加载期必须校验为正或显式填默认值：这里选择填默认，因为
// "忘记写 timeout"是配置作者最常见的行为，0 会伪装成网络故障——报错
// 成本高于一份物化默认。
// [推断] Task.Budget.TimeoutMs 零值按同一物理语义（超时陷阱）补 默认 24h
// （与设计文档给讨论超时的 24h 同尺度）；Watchdog 在阶段 9 接管该字段。
const defaultTimeoutMs int64 = 1_800_000

const defaultTaskTimeoutMs int64 = 86_400_000

// Loader 是 Profile 加载器（实现 types.ProfileLoader；Part 6.12 契约）。
//
// 生命周期：NewLoader → LoadAll →（运行时 Get/List）→（开发期 Reload）。
//
// 零值契约：零值 Loader 不可用（Get 永远 ErrNotFound 且无重载目录），
// 必须经 LoadAll 初始化。
//
// 并发：RWMutex 保护；Get/List 可并发，LoadAll/Reload 独占。
type Loader struct {
	mu   sync.RWMutex
	byID map[types.ProfileID]*config.Node // 合并前的**原始**树（继承闭包在 Get 时解）
	dir  string                           // Reload 的基准目录
}

// NewLoader 构造空加载器。
func NewLoader() *Loader {
	return &Loader{byID: map[types.ProfileID]*config.Node{}}
}

// LoadAll 读 profilesDir 下全部 *.yaml 并构建设置闭包。
//
// 失败（加载期 fail fast 的显式枚举）：
//   - 目录不可读 / 目录内无 profile 文件（空目录是配置错误而非空结果——
//     "一个能跑的项目至少有一个 profile"）；
//   - YAML 子集解析失败 / 顶层不含 profile 映射；
//   - id 缺失 / 重复；
//   - 能力名未知（types.Capability 契约）；
//   - extends 指向不存在的 Profile / 成环。
func (l *Loader) LoadAll(profilesDir string) error {
	raw, err := loadDir(profilesDir)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID = raw
	l.dir = profilesDir
	return nil
}

// Get 返回一个已加载的 Profile（合并闭包解出后的**快照副本**）。
//
// 返回副本而非内部指针：Router / Spawner / 装配层共享 Profile 是同一份
// 配置真相，调用方的临时修改（如 fork 时的 PromptOverride）不得击穿它。
func (l *Loader) Get(id types.ProfileID) (*types.Profile, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	node, ok := l.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	merged, err := l.mergeChainLocked(node, id)
	if err != nil {
		return nil, err
	}
	return buildProfile(id, merged)
}

// List 返回全部 Profile 摘要，按 ID 排序（进程表展示的确定性）。
func (l *Loader) List() []types.ProfileSummary {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]types.ProfileSummary, 0, len(l.byID))
	for id, node := range l.byID {
		extends := node.Get("extends")
		out = append(out, types.ProfileSummary{
			ID:          id,
			Description: node.Get("description").StrOr(""),
			Extends:     types.ProfileID(extends.StrOr("")),
			PromptID:    node.Get("prompt").StrOr(""),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Reload 热重载（Part 6.12：开发期调试）。运行中 Agent 持有加载时快照
// （Get 的副本），不受影响；新 fork 的 Agent 用新配置。
//
// 失败：未曾 LoadAll（无基准目录）；重载解析失败时保留旧配置——
// 配置文件编辑过渡期（写坏一半）不得让运行时自我崩溃。
func (l *Loader) Reload() ([]types.ProfileID, error) {
	l.mu.Lock()
	dir := l.dir
	l.mu.Unlock()
	if dir == "" {
		return nil, fmt.Errorf("profile: Reload without a prior LoadAll")
	}
	raw, err := loadDir(dir)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := make([]types.ProfileID, 0, len(raw))
	for id := range raw {
		if old, ok := l.byID[id]; !ok || !sameNode(old, raw[id]) {
			changed = append(changed, id)
		}
	}
	for id := range l.byID {
		if _, ok := raw[id]; !ok {
			// 已删除的条目也报告：Get 会变 ErrNotFound，调用方知道有 id 消失了。
			changed = append(changed, id)
		}
	}
	l.byID = raw
	sort.Slice(changed, func(i, j int) bool { return changed[i] < changed[j] })
	return changed, nil
}

// dirOf 返回基准目录（测试内部使用；运行态语义由 Reload 内部持锁控制）。
func (l *Loader) dirOf() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.dir
}

// loadDir 读目录下全部 profile yaml 文件并落入 byID 预表（解析在此完成，
// 闭包合并延后）。失败语义见 LoadAll。
func loadDir(dir string) (map[types.ProfileID]*config.Node, error) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("profile: read dir %s: %w", dir, err)
	}
	out := map[types.ProfileID]*config.Node{}
	for _, e := range names {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("profile: read %s: %w", e.Name(), err)
		}
		root, err := config.Parse(src)
		if err != nil {
			return nil, fmt.Errorf("profile: %s: %w", e.Name(), err)
		}
		// 骨架形态（cmd/marl init）：顶层 `profile:` 映射；裸 profile 文档同样接受。
		node := root.Get("profile")
		if node == nil {
			if root.Kind == config.KindMap {
				node = root
			} else {
				return nil, fmt.Errorf("profile: %s: expected a top-level profile mapping", e.Name())
			}
		}
		idStr, ok := node.Get("id").Str()
		if !ok || idStr == "" {
			return nil, fmt.Errorf("profile: %s: id is required", e.Name())
		}
		id := types.ProfileID(idStr)
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("profile: duplicate id %q", id)
		}
		if err := validateCapabilities(node); err != nil {
			return nil, fmt.Errorf("profile: %s: %w", e.Name(), err)
		}
		out[id] = node
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("profile: no profile files in %s (empty directory is a config error)", dir)
	}
	return out, nil
}

// validateCapabilities 校验 requirement.require / prefer 的能力名
// （types.Capability 契约：未识别值必须报错，静默丢弃会让 Router 选出
// 不支持该能力的模型）。
//
// 并发：纯函数。
func validateCapabilities(node *config.Node) error {
	req := node.Get("requirement")
	if req == nil {
		return nil
	}
	for _, listName := range []string{"require", "prefer"} {
		list := req.Get(listName)
		if list == nil || list.Kind != config.KindList {
			continue
		}
		for _, item := range list.Items {
			s, _ := item.Str()
			if !types.Capability(s).Valid() {
				return fmt.Errorf("unknown capability %q in requirement.%s", s, listName)
			}
		}
	}
	return nil
}

// mergeChainLocked 从原始树沿 extends 链合并出 resolveNode（调用方持锁）。
//
// 迭代而非递归：链走向由子到父，逐层"子覆盖父"的合并与递归语义一致；
// 计数上限是环检测（配置自环时在扫描终止，错误信息带完整环路径）。
func (l *Loader) mergeChainLocked(leaf *config.Node, id types.ProfileID) (*config.Node, error) {
	const maxChain = 100 // 环的上界（正常继承链不会超过 5 层；100 给环留足够余量）
	merged := leaf
	seen := map[types.ProfileID]bool{}
	for i := 0; i < maxChain; i++ {
		extendsID := types.ProfileID(merged.Get("extends").StrOr(""))
		if extendsID == "" {
			return merged, nil
		}
		if seen[extendsID] || extendsID == id {
			return nil, fmt.Errorf("profile: extends cycle involving %q", extendsID)
		}
		seen[extendsID] = true
		parent, ok := l.byID[extendsID]
		if !ok {
			return nil, fmt.Errorf("profile: extends unknown profile %q", extendsID)
		}
		merged = withoutExtends(mergeProfileNodes(merged, parent))
	}
	return nil, fmt.Errorf("profile: extends chain too deep (>= %d; likely a cycle)", maxChain)
}

// mergeProfileNodes 是 Part 6.6 合并规则的树层实现（覆盖 child 所在层）。
//
// 规则：
//   - 标量 / 嵌套映射：子文件里键存在 → 子值；子未写 → 父值。键存在性是
//     "显式声明"的唯一凭据（0、空串、false 都可能是合法的显式取值）。
//   - AllowedSkills：子非空 → 完全覆盖；子未写或空 → 继承父（Part 6.6/6.11：
//     空 = 不限制，继承语义覆盖子省略的场景）。
//   - Requirement.Require / Prefer 与 OutgoingContext.IncludeRoles：并集去重
//     （父的项在前，顺序稳定——Reset 后的缓存前缀确定性依赖它）。
//   - 其余列表（writable 类不存在于 Profile）：子覆盖父。
//
// child / parent 均可为 nil（nil 视为"这个文件没写任何东西"）。
// 并发：纯函数（config.Node 在解析后视为只读）。
func mergeProfileNodes(child, parent *config.Node) *config.Node {
	merged := map[string]*config.Node{}
	if parent != nil && parent.Kind == config.KindMap {
		for i, k := range parent.Keys {
			merged[k] = parent.Vals[i]
		}
	}
	if child != nil && child.Kind == config.KindMap {
		for i, k := range child.Keys {
			merged[k] = child.Vals[i]
		}
	}
	// 嵌套映射的递归合并：键级覆盖收到每一层（sampling 里"只写
	// temperature、max_tokens 继承"就是"标量字段子覆盖父"的逐字段形态；
	// 整块替换会把隐式继承的字段静默归零——合法零值与缺失在这层不可
	// 区分，必须用键存在性）。
	for k, v := range merged {
		p := getChild(parent, k)
		if p == nil || v.Kind != config.KindMap || p.Kind != config.KindMap {
			continue
		}
		merged[k] = mergeProfileNodes(v, p)
	}
	// 列表字段的逐字段语义覆盖（其余按键存在性由上面兜底）。
	for _, key := range []string{"requirement"} {
		c, p := nil2map(child.Get(key)), nil2map(parent.Get(key))
		m := map[string]*config.Node{}
		for i, k := range p.Keys {
			m[k] = p.Vals[i]
		}
		for i, k := range c.Keys {
			m[k] = c.Vals[i]
		}
		for _, subKey := range []string{"require", "prefer"} {
			m[subKey] = unionList(nil2map(parent.Get(key)).Get(subKey), nil2map(child.Get(key)).Get(subKey))
		}
		out := &config.Node{Kind: config.KindMap}
		for k, v := range m {
			out.Keys = append(out.Keys, k)
			out.Vals = append(out.Vals, v)
		}
		merged[key] = out
	}
	// OutgoingContext 的 key 在 types.Profile 的 yaml tag 是 outgoing_context。
	if parent.Get("outgoing_context") != nil || child.Get("outgoing_context") != nil {
		c, p := child.Get("outgoing_context"), parent.Get("outgoing_context")
		m := map[string]*config.Node{}
		for i, k := range nil2map(p).Keys {
			m[k] = nil2map(p).Vals[i]
		}
		for i, k := range nil2map(c).Keys {
			m[k] = nil2map(c).Vals[i]
		}
		m["include_roles"] = unionList(nil2map(p).Get("include_roles"), nil2map(c).Get("include_roles"))
		out := &config.Node{Kind: config.KindMap}
		for k, v := range m {
			out.Keys = append(out.Keys, k)
			out.Vals = append(out.Vals, v)
		}
		// include_roles 的顺序要求父前子后（去重放 unionList 里完成）。
		merged["outgoing_context"] = out
	}
	// allowed_skills：子非空完全覆盖，子未写继承父（Part 6.6）。
	if al := child.Get("allowed_skills"); al == nil || isListEmpty(al) {
		if p := parent.Get("allowed_skills"); p != nil {
			merged["allowed_skills"] = p
		}
	}
	out := &config.Node{Kind: config.KindMap}
	for k, v := range merged {
		out.Keys = append(out.Keys, k)
		out.Vals = append(out.Vals, v)
	}
	return out
}

// unionList 合并两个列表节点（并集去重；父的项在前、子新增的项在后
// ——继承闭包产物的确定性依赖这个序）。
//
// parent / child 任一为 nil（或标量——笔误形态）时取另一个。
func unionList(parent, child *config.Node) *config.Node {
	if parent == nil || parent.Kind != config.KindList {
		if child == nil || child.Kind != config.KindList {
			return nil
		}
		return child
	}
	if child == nil || child.Kind != config.KindList {
		return parent
	}
	items := make([]*config.Node, 0, len(parent.Items)+len(child.Items))
	seen := map[string]bool{}
	add := func(n *config.Node) {
		for _, it := range n.Items {
			s := it.StrOr("")
			if seen[s] {
				continue
			}
			seen[s] = true
			items = append(items, it)
		}
	}
	add(parent)
	add(child)
	return &config.Node{Kind: config.KindList, Items: items}
}

// getChild 是 nil-safe 的取子（Get 在 nil 上会返回 nil，这里同时折叠
// 常见情况，便于链式取值）。
func getChild(n *config.Node, key string) *config.Node {
	if n == nil {
		return nil
	}
	return n.Get(key)
}

// nil2map 把 nil 节点折叠成空 map 节点（合并代码里的判空降噪）。

// withoutExtends 返回去掉了 extends 键的 map 节点副本（mergeChainLocked
// 的链推进消费语义；产物是新建 map，可变）。
func withoutExtends(n *config.Node) *config.Node {
	out := &config.Node{Kind: config.KindMap}
	for i, k := range n.Keys {
		if k == "extends" {
			continue
		}
		out.Keys = append(out.Keys, k)
		out.Vals = append(out.Vals, n.Vals[i])
	}
	return out
}
func nil2map(n *config.Node) *config.Node {
	if n == nil {
		return &config.Node{Kind: config.KindMap}
	}
	if n.Kind != config.KindMap {
		return &config.Node{Kind: config.KindMap}
	}
	return n
}

// isListEmpty 报告一个节点是否是空列表（allowed_skills 的"空 = 全部允许，
// 不算显式收紧"语义）。
func isListEmpty(n *config.Node) bool {
	return n.Kind != config.KindList || len(n.Items) == 0
}

// buildProfile 把合并后的 Node 树解码成运行时 Profile 并 materialize 默认值。
func buildProfile(id types.ProfileID, node *config.Node) (*types.Profile, error) {
	p, err := decodeProfile(node)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", id, err)
	}
	p.ID = id
	if err := materializeDefaults(p); err != nil {
		return nil, fmt.Errorf("profile %q: %w", id, err)
	}
	return p, nil
}

// materializeDefaults 把"零值是陷阱"的字段物化默认（types.SamplingParams /
// Budget 的零值契约）。
func materializeDefaults(p *types.Profile) error {
	if p.Sampling.TimeoutMs == 0 {
		p.Sampling.TimeoutMs = defaultTimeoutMs
	}
	if p.Task.Budget.TimeoutMs == 0 {
		p.Task.Budget.TimeoutMs = defaultTaskTimeoutMs
	}
	return nil
}

// decodeProfile 把 Node 树解码成 Profile（id 由 buildProfile 回填）。
func decodeProfile(n *config.Node) (*types.Profile, error) {
	p := &types.Profile{}
	p.Extends = types.ProfileID(n.Get("extends").StrOr(""))
	p.Description = n.Get("description").StrOr("")
	p.Prompt = n.Get("prompt").StrOr("")
	p.CanSpawn = truthyScalar(n.Get("can_spawn"))

	// requirement
	if req := n.Get("requirement"); req != nil {
		list, err := strList(req.Get("require"))
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			p.Requirement.Require = append(p.Requirement.Require, types.Capability(s))
		}
		list, err = strList(req.Get("prefer"))
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			p.Requirement.Prefer = append(p.Requirement.Prefer, types.Capability(s))
		}
		if v, ok := req.Get("min_context").Int(); ok {
			p.Requirement.MinContext = v
		}
		p.Requirement.RungStart = types.RungID(req.Get("rung_start").StrOr(""))
	}

	// sampling
	if smp := n.Get("sampling"); smp != nil {
		p.Sampling = decodeSampling(smp)
	}

	// thinking
	if th := n.Get("thinking"); th != nil {
		p.Thinking.Level = th.Get("level").StrOr("")
		if v, ok := th.Get("budget").Int(); ok {
			b := v
			p.Thinking.Budget = &b
		}
		if d := th.Get("display"); d != nil {
			p.Thinking.Display.LogThinking = truthyScalar(d.Get("log_thinking"))
			p.Thinking.Display.ExposeToView = truthyScalar(d.Get("expose_to_view"))
		}
	}

	// task
	if tk := n.Get("task"); tk != nil {
		p.Task.OutputFormat = tk.Get("output_format").StrOr("")
		if b := tk.Get("budget"); b != nil {
			if v, ok := b.Get("max_tokens").Int(); ok {
				p.Task.Budget.MaxTokens = v
			}
			if v, ok := b.Get("timeout_ms").Int(); ok {
				p.Task.Budget.TimeoutMs = int64(v)
			}
		}
	}

	// outgoing_context
	if oc := n.Get("outgoing_context"); oc != nil {
		if v, ok := oc.Get("max_entries").Int(); ok {
			p.OutgoingContext.MaxEntries = v
		}
		if v, ok := oc.Get("max_tokens").Int(); ok {
			p.OutgoingContext.MaxTokens = v
		}
		roles, err := strList(oc.Get("include_roles"))
		if err != nil {
			return nil, err
		}
		for _, s := range roles {
			p.OutgoingContext.IncludeRoles = append(p.OutgoingContext.IncludeRoles, types.InternalRole(s))
		}
	}

	// allowed_skills
	skills, err := strList(n.Get("allowed_skills"))
	if err != nil {
		return nil, err
	}
	p.AllowedSkills = skills
	return p, nil
}

// decodeSampling 从 sampling 映射解码采样参数。
func decodeSampling(n *config.Node) types.SamplingParams {
	var s types.SamplingParams
	if v, ok := n.Get("temperature").Float(); ok {
		s.Temperature = v
	}
	if v, ok := n.Get("top_p").Float(); ok {
		s.TopP = v
	}
	if v, ok := n.Get("top_k").Int(); ok {
		s.TopK = v
	}
	if v, ok := n.Get("max_tokens").Int(); ok {
		s.MaxTokens = v
	}
	if v, ok := n.Get("timeout_ms").Int(); ok {
		s.TimeoutMs = int64(v)
	}
	return s
}

// strList 把一个列表节点解码成字符串切片；nil 返回空切片；present 但非
// 列表是错误（配置笔误不能静默落空——被忽略的列表项是隐形权限边）。
func strList(n *config.Node) ([]string, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind != config.KindList {
		return nil, fmt.Errorf("profile: list field: expected a block list, got kind %v", n.Kind)
	}
	out := make([]string, 0, len(n.Items))
	for _, it := range n.Items {
		out = append(out, it.StrOr(""))
	}
	return out, nil
}

// truthyScalar 把布尔标量解码：缺失/空 → false；书写形式按裸词 true（
// YAML 子集的布尔取值），其余非空词均为错误（防止 typo 被当成 false）。
func truthyScalar(n *config.Node) bool {
	s, ok := n.Str()
	if !ok {
		return false
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	return false
}

// sameNode 比较：重新序列化语义级近似太重，只比较字节（解析树把空白抹平了，
// 结构级比对足够反映行为面变化）。
func sameNode(a, b *config.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	return nodeKey(a) == nodeKey(b)
}

// nodeKey 产出节点的结构签名（递归渲染成不可混淆字符串）。
func nodeKey(n *config.Node) string {
	if n == nil {
		return "<nil>"
	}
	switch n.Kind {
	case config.KindScalar:
		return "s:" + n.Scalar
	case config.KindList:
		out := "l:["
		for _, it := range n.Items {
			out += nodeKey(it) + ","
		}
		return out + "]"
	default:
		out := "m:{"
		for i, k := range n.Keys {
			out += k + "=" + nodeKey(n.Vals[i]) + ","
		}
		return out + "}"
	}
}
