package proto

import (
	"strconv"

	"github.com/RobiNexy/Marl/internal/types"
)

// MsgType 是 Mailbox 消息类型（Part 8.7；Part 14.5 的统一修订）。
//
// 零值契约：MsgUnknown = 0 是**刻意的哨兵**。这是对设计文档的修正——文档
// Part 8.7 直接从 MsgTaskAssign 开始 iota，也就是让零值等于"分配任务"。
// 那意味着 Envelope 的 Type 一旦漏设（构造点忘记赋值、反序列化失败、跨版本
// 消息），零值会被解释成一个**具体的**消息类型：一条"没有类型的空消息"
// 变成"给这个 Agent 分配一个 nil 任务"，故障表现是派发流程深处的空指针
// 解引用，与真正的原因（信封缺字段）相隔极远。
//
// 有了哨兵后，未设置类型在接收侧第一跳就被拒绝（"收到未知消息类型"），
// 错误归因点与故障点重合。代价是每个 iota 值 +1、与文档里的数字不再一致；
// 但类型码从未被持久化成数值（Mailbox 是进程内通道，DB 里存的是字符串），
// 不存在兼容性问题。
//
// [偏离文档: 已在 decisions.md 记录为对 Part 8.7 的缺陷修正。]
//
// Part 14（阶段 12）修订：MsgHumanInput（marl say 的专用注入）由 MsgDirect
// 收编（人类发给任意 Actor 的直接消息——旧机制的合并，见 Part 14.12 改动
// 清单 #5）；新增 MsgGateRequest / MsgGateReply（Gate 的审批往返，投递走
// 人类 Actor 的文件后端）。类型码未持久化为数值，重排无兼容负担。
type MsgType int

const (
	MsgUnknown         MsgType = iota // 哨兵：零值 = "类型未设置/不识别"，必须拒绝处理
	MsgTaskAssign                     // 分配任务（任何 Actor → Agent；人类经 marl start）
	MsgChildReport                    // 子的 report（含框架代报的 failed；可投递到人类）
	MsgDirect                         // 直接消息（任何 → 任何；marl say、Agent 间显式通信）
	MsgEscalation                     // 下级上浮的求助
	MsgEscalationReply                // 上级的答复
	MsgGateRequest                    // PEP → 人类 Actor：审批请求（Part 14.7）
	MsgGateReply                      // 人类 Actor → Agent：裁决 + grant
	MsgReconfigure                    // 参数调整（人类或框架）
	MsgShutdown                       // 优雅关闭
)

// Valid 报告 t 是否为已知消息类型。
//
// 后置条件：MsgUnknown 返回 false——它存在恰恰是为了被拒绝；任何越界值
// （跨版本消息、内存损坏）同样返回 false。
// 并发：纯函数。
func (t MsgType) Valid() bool {
	switch t {
	case MsgTaskAssign, MsgChildReport, MsgDirect, MsgEscalation,
		MsgEscalationReply, MsgGateRequest, MsgGateReply, MsgReconfigure, MsgShutdown:
		return true
	}
	return false
}

// String 返回人类可读的类型名，供日志与审计使用。
// 未识别值输出 "MsgType(<n>)" 而不是空串：空串会让日志出现"类型："这种
// 无法区分"未知类型"与"忘记打印"的记录。
func (t MsgType) String() string {
	switch t {
	case MsgUnknown:
		return "unknown"
	case MsgTaskAssign:
		return "task_assign"
	case MsgChildReport:
		return "child_report"
	case MsgDirect:
		return "direct"
	case MsgEscalation:
		return "escalation"
	case MsgEscalationReply:
		return "escalation_reply"
	case MsgGateRequest:
		return "gate_request"
	case MsgGateReply:
		return "gate_reply"
	case MsgReconfigure:
		return "reconfigure"
	case MsgShutdown:
		return "shutdown"
	default:
		return "MsgType(" + strconv.Itoa(int(t)) + ")"
	}
}

// Envelope 是 Mailbox 里传递的消息封套（Part 8.7）。
//
// From 由框架按代码路径填写，Agent 无法通过任何参数影响——这是原则 4
// 在消息层的落点。收到消息的 Agent 可以信任 From。
//
// 不变量：
//   - From / To 非空（零值 AgentID 表示"未分配"，对一条待投递的消息无意义）；
//   - Type 必须 Valid()（零值哨兵会被接收侧拒绝）；
//   - Payload 的具体类型必须与 Type 对应。这是信封唯一的静态弱环节——Go 的
//     any 无法约束二者关系，因此接收侧必须按 Type 做类型断言，并对断言失败
//     返回错误，**禁止**用"断言失败就当 nil"的写法（那会把协议不匹配伪装成
//     "任务内容为空"，让模型对着空白任务工作）。
//
// TraceID 用 types.TraceID 而非裸 string：信封是所有跨 Agent 调用都会经过的
// 地方，而 TraceID/AgentID/MessageID 都是短字符串，在调用点极易互换；一次
// 串位会让整条因果链静默断裂——追踪失效不报错，比崩溃更难发现。
//
// 零值契约：Envelope{} 非法（From/To 为空、Type 是哨兵）。
type Envelope struct {
	From    types.AgentID
	To      types.AgentID
	TraceID types.TraceID // 因果链追踪；空 = 不追踪（合法，见 types.TraceID）
	Type    MsgType
	Payload any
}
