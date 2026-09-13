// Package actortest 提供统一 Actor 模型的测试替身（Part 14.9）。
//
// ScriptedHuman 是统一抽象的最大实务收益：人类的 Mailbox 后端换成
// channel，按剧本回消息——CI 里能跑完整的人机协作流程（讨论、审批、
// escalation、grant），不需要真人、不需要 UI、不需要等 fsnotify 静默期
// （channel 后端即时投递）。
//
// 文件后端本身（fsnotify/轮询、静默期、nonce、frontmatter 解析）在
// actor 包的单元测试里单独验证，与业务流程解耦（Part 14.9 的分工）。
package actortest

import (
	"context"
	"fmt"

	"marl/internal/actor"
)

// ScriptedReply 是剧本的一行：收到一条消息 → 产出若干回信。
type ScriptedReply struct {
	// When 匹配到达的信封（nil = 匹配任何）。用类型与载荷字段判定。
	When func(env actor.Envelope) bool
	// Reply 产出回信（From 由剧本填写——替身的人类身份是真实身份：
	// 它是控制面事实的唯一写者，原则 4 在测试里同样成立）。
	Reply func(env actor.Envelope) []actor.Envelope
}

// ScriptedHuman 是人类的测试替身（Part 14.9）。
//
// 复用 HumanActor 的全部裁决解析逻辑（capabilities / id 契约），只换
// Mailbox 后端：channel 直投替代文件后端。ReceiveSide 的消费由 Run 驱动
// ——装配方把 Run 的 route 接到 Spawner.SendTo，剧本回信即进入目标
// Agent 的信箱。
type ScriptedHuman struct {
	actor   *actor.HumanActor
	backend *actor.ChannelBackend
}

// New 构造（uid 缺省 "tester"；权限 = HumanCaps 的全量）。
func New(uid string) *ScriptedHuman {
	if uid == "" {
		uid = "tester"
	}
	b := actor.NewChannelBackend(32)
	h, err := actor.NewHuman(actor.HumanID(uid), b, actor.HumanCaps())
	if err != nil {
		panic(fmt.Sprintf("actortest: %v", err)) // 构造失败 = 测试代码 bug
	}
	return &ScriptedHuman{actor: h, backend: b}
}

// Actor 返回人类 Actor 本体（进程表注册用）。
func (s *ScriptedHuman) Actor() actor.Actor { return s.actor }

// ID 返回人类 ActorID。
func (s *ScriptedHuman) ID() actor.ActorID { return s.actor.ID() }

// Deliver 实现 MailboxBackend 的投递面（框架 → 人类）。
func (s *ScriptedHuman) Deliver(env actor.Envelope) error { return s.backend.Deliver(env) }

// Inbox 返回收到的信封快照（断言用）。
func (s *ScriptedHuman) Inbox() []actor.Envelope { return s.backend.Drain() }

// Run 消费收件箱并按剧本回信（阻塞直至 ctx 取消；route 是回信的出口
// ——装配为 Spawner.SendTo）。
//
// 剧本语义：逐个 When 匹配，首个命中的 Reply 产出回信（无命中 = 记录
// 后丢弃——与生产 pump 的"未处理必须可见"同纪律，测试断言用 Inbox）。
func (s *ScriptedHuman) Run(ctx context.Context, script []ScriptedReply, route func(actor.Envelope) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-s.backend.Receive():
			if !ok {
				return
			}
			for _, st := range script {
				if st.When == nil || st.When(env) {
					for _, rep := range st.Reply(env) {
						if err := route(rep); err != nil {
							return
						}
					}
					break
				}
			}
		}
	}
}
