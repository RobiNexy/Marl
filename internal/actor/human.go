package actor

// HumanActor：人类进进程表（Part 14.3）。
//
// 没有 Executor——人类自己就是执行器，框架不为人类安排任何"思考循环"
// （Part 14.13 的本体论条款）。Mailbox 的物理实现是控制面文件树（生产）
// 或 channel（ScriptedHuman / 测试）——差异全部在后端，本结构不感知。

import "fmt"

// HumanActor 是统一 Actor 模型的人类子类型（监督树的根，depth=0）。
//
// 零值契约：零值不可用（backend 缺失 = 收件箱断链；caps 的 Kind 空 =
// 未分类）。必须经 NewHuman 构造。
type HumanActor struct {
	id      ActorID
	backend MailboxBackend
	caps    CapSet
}

// NewHuman 构造（caps 显式传入——测试可以造"被收窄的人类"做反例验证，
// 生产装配用 HumanCaps()）。
//
// 失败：id 非 human: 形态 / backend nil / caps.Kind 不是 HumanKind。
func NewHuman(id ActorID, backend MailboxBackend, caps CapSet) (*HumanActor, error) {
	if !ValidID(id) || !IsHumanID(id) {
		return nil, fmt.Errorf("actor: human id must be %q prefixed (got %q)", HumanPrefix, id)
	}
	if backend == nil {
		return nil, fmt.Errorf("actor: human %s requires a mailbox backend", id)
	}
	if caps.Kind != HumanKind {
		return nil, fmt.Errorf("actor: human %s caps.Kind must be HumanKind (got %q)", id, caps.Kind)
	}
	return &HumanActor{id: id, backend: backend, caps: caps}, nil
}

// ID 实现 Actor（"human:<uid>"）。
func (h *HumanActor) ID() ActorID { return h.id }

// Mailbox 实现 Actor（后端的读端；生产 = 文件后端的回执流）。
func (h *HumanActor) Mailbox() <-chan Envelope { return h.backend.Receive() }

// Capabilities 实现 Actor（生产装配 = HumanCaps() 的全量交互面）。
func (h *HumanActor) Capabilities() CapSet { return h.caps }

// Deliver 把一条信封投进收件箱（路由方经 backend 走；这里给宿主一个
// 不经 Actor 接口的直接入口——Actor 接口只暴露读端，写端是传输细节）。
func (h *HumanActor) Deliver(env Envelope) error { return h.backend.Deliver(env) }

// Backend 暴露后端（宿主需要 Receive 通道给 PumpReceive / 需要 Stop 时）。
func (h *HumanActor) Backend() MailboxBackend { return h.backend }
