package escalate

// FrameworkClient：Agent 侧发送/等待 + 回复分发（路由 rt）。

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/types"
)

// Manager 是框架侧的 Escalation 客户端（发送 + 等回 + 分发）。
//
// 并发：pump（DeliverReply，子信箱路径）与 goroutine（人类信箱路径）都
// 可以回调 deliver——经 mutex；等待通道 keyed by TraceID（同一"因果链"
// 的对账键，Part 11.3 的 TraceID 不变量）。
type Manager struct {
	mu      sync.Mutex
	pending map[types.TraceID]chan *proto.EscalationReply
	cfg     Config
}

// Config 是 Manager 的装配参数。
type Config struct {
	// Rule 在每次 Send 时调用（父的运行状态是实时的——Blocked 判定
	// 不能缓存到构造期）。
	Rule func() (proto.EscalationRule, error)
	// Mailbox 是人类信箱（TargetHuman 的交付面；使用前必须构造过）。
	Mailbox *Mailbox
	// ParentSender 是父信封的投递面（装配注入——Manager 不认识父进程表；
	// 实现一般会把信封 push 进父的 Mailbox 通道 + 记审计）。
	ParentSender func(ctx context.Context, rule proto.EscalationRule, req *proto.EscalationRequest) error
}

// NewManager 构造。
//
// Mailbox 可以不带（配置面只走父路由）：无出口的判定交给 Route 的
// rule.Validate（fail-closed）。
func NewManager(cfg Config) (*Manager, error) {
	return &Manager{pending: map[types.TraceID]chan *proto.EscalationReply{}, cfg: cfg}, nil
}

// SendEscalation 发送一次求助（非阻塞投递： goroutine 承担等待面（文件
// 信箱的轮询 / 父信箱 ACK 的等待），回复经 DeliverReply 转回等待者）。
//
// 返回的 EscalationID 是对账键（审计与 status 显示）；TraceID 与 request
// 一致（Part 11.3 不变量）。
func (m *Manager) SendEscalation(ctx context.Context, req *proto.EscalationRequest) (types.EscalationID, error) {
	if req == nil || req.From == "" || req.Question == "" {
		return "", fmt.Errorf("escalate: request requires from/question")
	}
	if req.TraceID == "" {
		req.TraceID = types.TraceID("esc-" + string(req.From) + "-" + shortUlid())
	}
	rule, err := m.cfg.Rule()
	if err != nil {
		return "", err
	}
	target, err := Route(rule, req)
	if err != nil {
		return "", err
	}
	switch target {
	case TargetParent:
		sender := m.cfg.ParentSender
		if sender == nil {
			return "", fmt.Errorf("escalate: parent route requires ParentSender (assembly missing)")
		}
		if err := sender(ctx, rule, req); err != nil {
			return "", fmt.Errorf("escalate: send to parent %s: %w", rule.ParentID, err)
		}
		return types.EscalationID(req.TraceID), nil
	default:
		if m.cfg.Mailbox == nil {
			return "", fmt.Errorf("escalate: human path requires Mailbox (assembly missing)")
		}
		f, err := m.cfg.Mailbox.Submit(req)
		if err != nil {
			return "", err
		}
		f.Req = req
		// 人类信箱的等待由本 goroutine 承担（回复到达 → deliver → 回收）。
		go func() {
			// goroutine 的 owner = 本 Manager；退出条件 = ctx 取消（宿主
			// Stop）或回复到达（单次交付后删除 reg）。
			reply, werr := m.cfg.Mailbox.WaitReply(ctx, f)
			if werr != nil {
				return
			}
			m.DeliverReply(&proto.EscalationReply{
				IsHuman: true,
				Content: reply,
				TraceID: req.TraceID,
			})
		}()
		return f.ID, nil
	}
}

// DeliverReply 投递一份回复（agent pump 的 MsgEscalationReply 上行；
// 人类信箱 goroutine 的完成路径也走它——两条路共用同一分发语义）。
func (m *Manager) DeliverReply(reply *proto.EscalationReply) {
	if reply == nil || reply.TraceID == "" {
		return // 无对账键的回复无入格（不静默丢弃到某个错误 sink 即可）
	}
	m.mu.Lock()
	ch := m.pending[reply.TraceID]
	delete(m.pending, reply.TraceID)
	m.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- reply:
	default:
	}
}

// WaitBack register 等待通道（eventLoop 发送前调用——回复早到的竞态由
// 先注册再投递的顺序消掉）。
func (m *Manager) WaitBack(ctx context.Context, traceID types.TraceID) (*proto.EscalationReply, error) {
	ch := make(chan *proto.EscalationReply, 1)
	m.mu.Lock()
	m.pending[traceID] = ch
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.mu.Lock()
		delete(m.pending, traceID)
		m.mu.Unlock()
		return nil, ctx.Err()
	case r := <-ch:
		return r, nil
	}
}

func shortUlid() string {
	b := make([]byte, 6)
	renErr := randRead(b)
	if renErr != nil {
		return "000000"
	}
	return fmt.Sprintf("%x", b)
}

// randRead 是 crypto/rand.Read 的窄别名（包内测试注入点）。
var randRead = func(b []byte) error { _, err := rand.Read(b); return err }
