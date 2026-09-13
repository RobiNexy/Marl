package gate

// GrantStore：GrantAlways 的落盘生命周期（Part 14.7 / 14.12 #11）。
//
// 两种 grant 生命周期（已确认的决定）：
//
//	session   —— 内存（Manager.sessionGrants），daemon 生命周期内；
//	permanent —— grants/grant_<ulid>.yaml 落盘，重启后仍生效——
//	             "人类批过的 always 不丢"。
//
// 形态：frontmatter（与审批文件 / verdict 同一解析纪律）。[权衡: 不复用
// internal/config 的 YAML 解析器——config 包依赖本包（gate 规则解析），
// 反向引包成环；grant 文件是键值对齐的 frontmatter 形态，20 行解析的
// 复制成本低于反转依赖方向。]
//
// 授权链（Part 14.10）：文件记录 granted_by（ActorID）与时间——审计面
// 的授权完整性从决策时点延伸到重启之后。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"marl/internal/store"
	"marl/internal/types"
)

// GrantStore 是 grants/ 目录的读写面。
type GrantStore struct {
	dir string // <ControlRoot>/grants
}

// NewGrantStore 构造并创建目录（缺目录是授权断链的隐形失败——显式创建）。
func NewGrantStore(controlRoot string) (*GrantStore, error) {
	if controlRoot == "" {
		return nil, fmt.Errorf("gate: controlRoot is required")
	}
	dir := filepath.Join(controlRoot, "grants")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("gate: mkdir grants: %w", err)
	}
	return &GrantStore{dir: dir}, nil
}

// PersistedGrant 是一份落盘 grant 的解析形态。
type PersistedGrant struct {
	Rule      Rule
	GrantedBy types.AgentID // 人类 ActorID（授权链）
	GrantedAt time.Time
}

// Save 落盘一份永久 grant（GrantAlways 的规则形态）。
//
// 幂等性：每次 Save 都是新文件（grant_<ulid>.yaml）——重复批同一规则
// 会产生两份文件，规则表里动态 allow 规则幂等（首中生效），多一份文件
// 是诚实的审批历史而不是错误。
func (s *GrantStore) Save(ctx context.Context, rule Rule, grantedBy types.AgentID) error {
	ulid, err := store.NewMessageID()
	if err != nil {
		return fmt.Errorf("gate: ulid: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "rule_id: %s\n", rule.ID)
	fmt.Fprintf(&sb, "kind: %s\n", rule.Match["kind"])
	fmt.Fprintf(&sb, "action: %s\n", rule.Action)
	fmt.Fprintf(&sb, "granted_by: %s\n", grantedBy)
	fmt.Fprintf(&sb, "granted_at: %s\n", time.Now().UTC().Format(time.RFC3339))
	if rule.Reason != "" {
		fmt.Fprintf(&sb, "reason: %s\n", sanitizeYAMLLine(rule.Reason))
	}
	// match 的其余键（数值表达式等）逐行登记——解析侧回读成完整 Match。
	matchKeys := make([]string, 0, len(rule.Match))
	for k := range rule.Match {
		if k == "kind" {
			continue
		}
		matchKeys = append(matchKeys, k)
	}
	sort.Strings(matchKeys)
	for _, k := range matchKeys {
		fmt.Fprintf(&sb, "match_%s: %s\n", k, sanitizeYAMLLine(rule.Match[k]))
	}
	sb.WriteString("---\n")
	path := filepath.Join(s.dir, "grant_"+strings.ToLower(ulid)+".yaml")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("gate: write grant: %w", err)
	}
	return nil
}

// sanitizeYAMLLine 单行化（换行/回车 → 空格——手写规则的 Reason 可能带
// 换行，落入 YAML 标量会破坏 frontmatter 结构）。
func sanitizeYAMLLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// LoadGrants 回读全部永久 grant（装配侧重启时回插动态规则；目录为空 =
// 无授权，不是错误）。
//
// 失败：单份文件损坏 → 显式错误（授权事实的静默丢失比报错危险）。
func (s *GrantStore) LoadGrants(ctx context.Context) ([]PersistedGrant, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("gate: read grants: %w", err)
	}
	out := []PersistedGrant{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("gate: read %s: %w", e.Name(), rerr)
		}
		g, perr := parseGrant(data)
		if perr != nil {
			return nil, fmt.Errorf("gate: parse %s: %w", e.Name(), perr)
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule.ID < out[j].Rule.ID })
	return out, nil
}

// parseGrant 解析一份 grant 文件（frontmatter → PersistedGrant）。
func parseGrant(data []byte) (PersistedGrant, error) {
	g := PersistedGrant{Rule: Rule{Match: map[string]string{}}}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return PersistedGrant{}, fmt.Errorf("frontmatter missing")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return PersistedGrant{}, fmt.Errorf("frontmatter not closed")
	}
	for i := 1; i < end; i++ {
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "rule_id":
			g.Rule.ID = v
		case "kind":
			g.Rule.Match["kind"] = v
		case "action":
			g.Rule.Action = Action(v)
		case "granted_by":
			g.GrantedBy = types.AgentID(v)
		case "granted_at":
			if t, terr := time.Parse(time.RFC3339, v); terr == nil {
				g.GrantedAt = t
			}
		case "reason":
			g.Rule.Reason = v
		default:
			if mk, ok := strings.CutPrefix(strings.TrimSpace(k), "match_"); ok && v != "" {
				g.Rule.Match[mk] = v
			}
		}
	}
	if g.Rule.ID == "" || g.Rule.Action == "" {
		return PersistedGrant{}, fmt.Errorf("missing rule_id/action")
	}
	return g, nil
}
