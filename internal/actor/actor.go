// Package actor 实现统一 Actor 模型的交互面（Part 14，阶段 12）。
//
// 定位（Part 14.1）：这不是加层，是删特例。人类与 Agent 在进程表里同为
// Actor——人类消息早就是一等 LogEntry，discussion/escalation 已被 Gate
// 收编，authorship 早就是编码字段。本包只补最后一块：人类进进程表、
// Mailbox 双后端（Agent = goroutine channel，Human = 控制面文件）。
//
// 边界（Part 14.1 的"统一只发生在交互面"）：frozen 前缀经济学、阶梯、
// 命名空间、Log/View、技能——认知面一个字不改。本包不 import agent；
// agent 经窄接口（HumanLink）消费本包，方向恒定指向抽象。
//
// 三条纪律（Part 14.2，违反任何一条这个抽象就烂掉）：
//  1. 权威判断只读 Capabilities()，永远不读 Kind——Kind 只允许呈现层
//     （气泡颜色、图标）与拓扑记账（深度语义）使用；
//  2. 胖接口即失败——Actor 核不挂 View / Binding / Skills 这些 LLM
//     专属物；人类子类型对 Agent 扩展接口是"没有"，不是"空实现"；
//  3. 不对称是数据不是机制——人类拥有 OwnsControlPlane、AI 不拥有，
//     控制面的写权限因此是能力模型的推论，不再是一套专门机制。
package actor

import (
	"strings"

	"github.com/RobiNexy/Marl/internal/types"
)

// ActorID 是进程表里统一实体的标识，格式 "human:<uid>"（人类）或既有
// AgentID（Agent）。
//
// [偏离文档: 文档 Part 14.2 把 ActorID 定义为独立类型 "agent:<ulid>" |
// "human:<uid>"。实现取 types.AgentID 的**别名**——既有 Agent 身份
// （Log 归属、audit 的 actor 字段、缓存桶 CacheBucket=AgentID、fossil
// 提交 author）全部以 AgentID 为键，换类型等于全仓扫描替换且无一收益；
// 可判别性由 human: 前缀承担（Agent 是"无前缀"），格式契约由本包的
// 构造与校验函数收口。呈现层翻译回 "human:xxx" / "agent:xxx" 标签
// （Part 14.10 的兼容面）。]
//
// 权威来自能力集不来自 ID：IsHumanID 只允许出现在路由与呈现里，出现在
// 权限判断里 = 抽象失败（纪律 1）。
type ActorID = types.AgentID

// HumanPrefix 是人类 ActorID 的前缀。后缀 = OS UID（能写控制面文件的
// 进程就是同 UID 的人类侧入口——Part 14.5 的 From 推导）。
const HumanPrefix = "human:"

// HumanID 构造人类 ActorID（uid 为 OS UID；空 uid 是装配错误）。
func HumanID(uid string) ActorID {
	return ActorID(HumanPrefix + uid)
}

// IsHumanID 报告 id 是否为人类 ActorID（human: 前缀）。
//
// 用面：路由（文件后端 vs channel 后端的分派）与呈现。**禁止**进入权限
// 判断——权威判断只读 Capabilities()（纪律 1）。
func IsHumanID(id ActorID) bool {
	return strings.HasPrefix(string(id), HumanPrefix)
}

// HumanUIDOf 拆出 uid（非人类 ID 返回 false——调用方按"不是人类"处理，
// 不猜默认值）。
func HumanUIDOf(id ActorID) (string, bool) {
	if !IsHumanID(id) {
		return "", false
	}
	uid := strings.TrimPrefix(string(id), HumanPrefix)
	return uid, uid != ""
}

// ValidID 报告 id 是否为合法 ActorID 形态：human:<非空 uid>，或非空的
// Agent ID（无前缀形态；格式由 Agent 侧的注册面保证）。
//
// 并发：纯函数。
func ValidID(id ActorID) bool {
	if IsHumanID(id) {
		_, ok := HumanUIDOf(id)
		return ok
	}
	return string(id) != ""
}

// ActorKind 是呈现层的实体种类（Part 14.2 的元数据字段）。
//
// 权威判断禁止读取本字段（纪律 1）；它只服务于 UI 渲染（气泡颜色）与
// 拓扑记账（max_depth 只约束 AI→AI fork——Part 14.6 的记账点）。
type ActorKind string

const (
	HumanKind ActorKind = "human"
	AgentKind ActorKind = "agent"
)

// Valid 报告 k 是否为已定义种类。零值非法（未分类即拒绝呈现，不猜默认）。
func (k ActorKind) Valid() bool {
	switch k {
	case HumanKind, AgentKind:
		return true
	}
	return false
}

// CapSet 是 Actor 的权威能力集（Part 14.2）。
//
// 交互面能力（CanSpawn/CanMessage/OwnsControlPlane）驱动裁决关口；
// 认知面能力（UsesSkills/HasBinding/HasView）是形态描述——人类为零值。
// Kind 是元数据：权限判断禁止读取（纪律 1），呈现层专用。
//
// 零值契约：零值 CapSet 的 Kind 为空 = 未分类，不可用。构造必须经
// HumanCaps / AgentCaps（或显式填充全部字段）。
type CapSet struct {
	// 交互面能力
	CanSpawn         bool // 能 fork 项目/子 Agent（Spawner 裁决的第一问）
	CanMessage       bool // 能向任意 Actor 发消息
	OwnsControlPlane bool // 能写控制面（审批文件、verdict、escalation 回复）
	// 认知面能力（人类为零值——人类自己就是执行器，Part 14.13）
	UsesSkills bool // 有技能表（LLM 工具面）
	HasBinding bool // 有阶梯绑定
	HasView    bool // 有 ContextView
	// 元数据（呈现层专用，权限判断禁止读取）
	Kind ActorKind
}

// HumanCaps 是人类的 CapSet：交互面全量，认知面零值（Part 14.3）。
//
// "收窄人类等于给监督树根带镣铐"（Part 14.13）——控制面写权限在人类的
// 能力集里、AI 的没有，Resolver 的能力校验自然拒绝（原则 4 降级为推论）。
func HumanCaps() CapSet {
	return CapSet{
		CanSpawn:         true,
		CanMessage:       true,
		OwnsControlPlane: true,
		UsesSkills:       false,
		HasBinding:       false,
		HasView:          false,
		Kind:             HumanKind,
	}
}

// AgentCaps 是 Agent 的 CapSet（CanSpawn 由 Profile.CanSpawn 推导；
// 控制面永远不在 AI 的能力集里——不对称是数据，纪律 3）。
func AgentCaps(canSpawn bool) CapSet {
	return CapSet{
		CanSpawn:         canSpawn,
		CanMessage:       true,
		OwnsControlPlane: false,
		UsesSkills:       true,
		HasBinding:       true,
		HasView:          true,
		Kind:             AgentKind,
	}
}

// Actor 是进程表里统一实体的最小核（Part 14.2）。
//
// 三方法即全部——View/Binding/Skills 等 Agent 扩展物不在这里（纪律 2）。
// 实现方：HumanActor（本包）与 agent 包的适配器（AgentActor）。
type Actor interface {
	ID() ActorID
	// Mailbox 返回收件箱读端。人类实现的后端是控制面文件（Receive 通道
	// 侧）；Agent 是 goroutine channel。路由方（Spawner）不关心差异。
	Mailbox() <-chan Envelope
	// Capabilities 返回权威能力集——一切裁决关口只读这里。
	Capabilities() CapSet
}

// Deliverer 是 Actor 的可选投递面（进程表的行需要它路由消息——Actor
// 接口只暴露读端，写端是传输细节）。HumanActor 实现；测试替身（channel
// 后端）同样实现。
type Deliverer interface {
	Deliver(env Envelope) error
}

// BackendOf 把（投递函数 + 读端）折算成 MailboxBackend（装配层的适配点：
// 持有 Actor 接口的代码经它获得完整后端视图）。
func BackendOf(deliver func(Envelope) error, recv <-chan Envelope) MailboxBackend {
	return backendFunc{deliver: deliver, recv: recv}
}

type backendFunc struct {
	deliver func(Envelope) error
	recv    <-chan Envelope
}

func (b backendFunc) Deliver(env Envelope) error { return b.deliver(env) }
func (b backendFunc) Receive() <-chan Envelope   { return b.recv }
