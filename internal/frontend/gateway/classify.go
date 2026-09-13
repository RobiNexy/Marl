package gateway

// 领域事件分类：audit Action（开放集合）→ 稳定的呈现 Kind。
//
// Action 是开放集合（store.AuditEvent 的设计决策：新增模块不改 store 即可
// 记录）。因此分类必须是"已知 → 专属 Kind，未知 → KindOther + 原文透传"，
// 绝不做封闭枚举假设——未知 Action 的兜底渲染是契约的一部分，不是遗漏。

import "github.com/RobiNexy/Marl/internal/store"

// Kind 是事件的呈现类别（TUI 的图标/颜色选择键；Web/GUI 同样消费）。
type Kind string

const (
	KindGate        Kind = "gate"         // gate_request / gate_pending / gate_decision / gate_resolved...
	KindSpawn       Kind = "spawn"        // spawn / spawn_batch / actor_registered
	KindAgentState  Kind = "agent_state"  // 状态迁移
	KindModel       Kind = "model"        // model_upgrade / model_switch
	KindWatchdog    Kind = "watchdog"     // watchdog_*（无进展/终止/停摆）
	KindDiscussion  Kind = "discussion"   // discussion_*
	KindEscalation  Kind = "escalation"   // escalation_*
	KindOrchestrate Kind = "orchestrate"  // 编排操作
	KindCommit      Kind = "commit"       // 版本提交
	KindOther       Kind = "other"        // 未知 Action 的兜底（原文透传）
)

// DomainEvent 是分类后的审计事件（原始字段全保留 + 呈现 Kind）。
type DomainEvent struct {
	store.AuditEvent
	Kind Kind
}

// Classify 把一条审计事件折算成领域事件（纯函数）。
//
// 匹配用前缀而非全等：Action 的细分（如 gate_request/gate_pending/
// gate_resolved）共享呈现类，前缀归类让新增细分自动落入正确 Kind；
// "watchdog" 等独立词根全等匹配。
func Classify(ev store.AuditEvent) DomainEvent {
	a := ev.Action
	switch {
	case hasAnyPrefix(a, "gate_"):
		return DomainEvent{AuditEvent: ev, Kind: KindGate}
	case hasAnyPrefix(a, "spawn", "actor_registered"):
		return DomainEvent{AuditEvent: ev, Kind: KindSpawn}
	case a == "agent_state":
		return DomainEvent{AuditEvent: ev, Kind: KindAgentState}
	case hasAnyPrefix(a, "model_"):
		return DomainEvent{AuditEvent: ev, Kind: KindModel}
	case hasAnyPrefix(a, "watchdog"):
		return DomainEvent{AuditEvent: ev, Kind: KindWatchdog}
	case hasAnyPrefix(a, "discussion"):
		return DomainEvent{AuditEvent: ev, Kind: KindDiscussion}
	case hasAnyPrefix(a, "escalation"):
		return DomainEvent{AuditEvent: ev, Kind: KindEscalation}
	case a == "orchestrate":
		return DomainEvent{AuditEvent: ev, Kind: KindOrchestrate}
	case a == "commit":
		return DomainEvent{AuditEvent: ev, Kind: KindCommit}
	default:
		return DomainEvent{AuditEvent: ev, Kind: KindOther}
	}
}

// hasAnyPrefix 报告 s 是否以任一 prefix 开头。
func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if len(p) > 0 && len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}
