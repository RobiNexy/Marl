package actor

// Mailbox 与双后端（Part 14.3 / 14.4）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/store"
)

// Envelope 是本包对 proto.Envelope 的别名（Part 14.4 的路由一致面）。
//
// 别名而非重新定义：信封是全框架的单一协议形态（proto 包所有），actor
// 只是路由与传输的宿主——重新定义会制造第二套信封协议。
type Envelope = proto.Envelope

// MailboxBackend 是 Mailbox 的传输后端（Part 14.4）。
//
// 路由完全一致（Deliver/Receive 两个动作），传输层按目标 Actor 类型选择：
// Agent = goroutine channel（ChannelBackend），Human = 控制面文件 +
// 轮询静默期（FileBackend——Part 11 已有机制的正式命名与收编）。
//
// 并发：实现必须可多 goroutine 并发调用（Spawner 的路由面）。
type MailboxBackend interface {
	// Deliver 投递一条信封。
	//
	// 失败语义：信封无法成为控制面事实（编码失败 / 磁盘故障 / 目标
	// channel 满且超时）→ error——投递者（Spawner / pump）必须把失败
	// 传给发送侧，"发出去但没人收到"不可归因（escalate 包头的同一纪律）。
	Deliver(env Envelope) error
	// Receive 返回收件箱的读端。
	//
	// FileBackend 的语义：Receive 是"人类回复"方向的信封流（轮询 +
	// 静默期 + 解析回信封）；Deliver 写出的纯投递文件（report 等无回执
	// 形态）不进这条流。ChannelBackend 的语义：与 Deliver 同一通道两端。
	Receive() <-chan Envelope
}

// DefaultChannelBuffer 是 channel 后端的缺省缓冲（Spawner 的既有口径）。
const DefaultChannelBuffer = 32

// deliverTimeout 是 channel 满时的背压等待（Spawner 的 deliverReport /
// SendTo 既有语义的搬移点：写给 pump 的信被卡住 5s 即丢弃并报错，审计
// 里可见——无限阻塞会让路由方死锁在满信箱上）。
const deliverTimeout = 5 * time.Second

// ChannelBackend 是 Agent 的 Mailbox 后端（goroutine channel）。
type ChannelBackend struct {
	ch   chan Envelope
	once sync.Once
}

// NewChannelBackend 构造（buffer <= 0 用缺省 32）。
func NewChannelBackend(buffer int) *ChannelBackend {
	if buffer <= 0 {
		buffer = DefaultChannelBuffer
	}
	return &ChannelBackend{ch: make(chan Envelope, buffer)}
}

// Deliver 投递到 channel（满则等待 deliverTimeout 后报错——与 Spawner
// 既有 SendTo 的超时语义一致）。
func (b *ChannelBackend) Deliver(env Envelope) error {
	select {
	case b.ch <- env:
		return nil
	case <-time.After(deliverTimeout):
		return fmt.Errorf("actor: channel mailbox full (deliver of %s dropped after %s)", env.Type, deliverTimeout)
	}
}

// Receive 返回读端（Agent 的 pump 消费）。
func (b *ChannelBackend) Receive() <-chan Envelope { return b.ch }

// Drain 取走当前缓冲的全部信封（断言/测试替身的观察面；不阻塞）。
func (b *ChannelBackend) Drain() []Envelope {
	var out []Envelope
	for {
		select {
		case env := <-b.ch:
			out = append(out, env)
		default:
			return out
		}
	}
}

// Close 关闭发送端（Run 停机的正常路径：pump 的 range 随之退出）。幂等。
func (b *ChannelBackend) Close() {
	b.once.Do(func() { close(b.ch) })
}

// FileBackend 是 Human 的 Mailbox 后端（控制面文件树，Part 14.3/14.4）。
//
// 物理形态（沿用 Part 11 已有机制，正式命名"人类的收件箱"）：
//
//	<root>/inbox/gate_<ulid>.md        # Gate 审批请求（人类编辑 @ 命令回执）
//	<root>/inbox/direct_<ulid>.md      # 人类直接消息（marl say 写入）
//	<root>/inbox/report_<ulid>.md      # 子 Agent 的 report（纯投递，无回执）
//	<root>/inbox/done/                 # 已消费的回执文件归档
//
// 双向语义：
//   - Deliver（框架 → 人类）：信封编码成 Markdown 文件（frontmatter 含
//     nonce）。gate_ 的模板含 @ 命令清单——人类编辑后同一文件成为回执。
//   - Receive（人类 → 框架）：轮询 inbox/（mtime 静默窗，与 discuss /
//     escalate / 旧 FileApprover 同一参数语义），把"带当轮凭据的回执"
//     解析回信封（MsgGateReply / MsgDirect），搬入 done/。纯投递形态
//     （report / escalation / 未裁决的 gate）不进读端。
//
// [偏离文档: fsnotify → mtime 轮询（与 discuss watcher / escalate
// Mailbox 同一取舍，已在 decisions 记录；参数语义不变面）。]
type FileBackend struct {
	root      string  // 控制面根（<project-id>）
	human     ActorID // 本收件箱的主人（From 的推导源）
	poll      time.Duration
	quiet     time.Duration
	seenMod   map[string]time.Time // 收看的文件 mtime（静默窗状态）
	seenFirst map[string]time.Time
	// issued 是当轮凭据的登记（requestID → nonce；Deliver(MsgGateRequest)
	// 写入，Receive 的解析对账——陈旧回放/伪造凭据在这里被拒）。mu 保护
	// （Deliver 在路由 goroutine，watch 在本 goroutine）。
	mu            sync.Mutex
	issued        map[string]string
	ch            chan Envelope
	stopWatch     chan struct{}
	stopWatchOnce sync.Once
}

// FileConfig 是 FileBackend 的装配参数。
type FileConfig struct {
	// Root 是控制面根 ~/.local/state/marl/<project-id>/（Agent 不可触）。
	Root string
	// Human 是收件箱主人的 ActorID（"human:<uid>"；回执 From 的推导源）。
	Human ActorID
	// PollInterval / Quiescence：静默窗参数（真实 500ms / 10s；测试缩小
	// ——契约测"静默窗口"的语义，不测具体数值）。
	PollInterval time.Duration
	Quiescence   time.Duration
	// Buffer 是读端 channel 缓冲（<=0 用 DefaultChannelBuffer）。
	Buffer int
}

// NewFileBackend 构造并创建目录（缺目录是信箱断链的隐形失败，必须显式）。
func NewFileBackend(cfg FileConfig) (*FileBackend, error) {
	switch {
	case cfg.Root == "":
		return nil, fmt.Errorf("actor: FileConfig.Root is required")
	case !ValidID(cfg.Human) || !IsHumanID(cfg.Human):
		return nil, fmt.Errorf("actor: FileConfig.Human 必须是 human:<uid> 形态（got %q）", cfg.Human)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.Quiescence <= 0 {
		cfg.Quiescence = 10 * time.Second
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultChannelBuffer
	}
	for _, sub := range []string{"inbox", filepath.Join("inbox", "done")} {
		if err := os.MkdirAll(filepath.Join(cfg.Root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("actor: mkdir %s: %w", sub, err)
		}
	}
	return &FileBackend{
		root:      cfg.Root,
		human:     cfg.Human,
		poll:      cfg.PollInterval,
		quiet:     cfg.Quiescence,
		seenMod:   map[string]time.Time{},
		seenFirst: map[string]time.Time{},
		issued:    map[string]string{},
		ch:        make(chan Envelope, cfg.Buffer),
		stopWatch: make(chan struct{}),
	}, nil
}

// inboxDir / doneDir 是收件箱的路径面。
func (b *FileBackend) inboxDir() string { return filepath.Join(b.root, "inbox") }
func (b *FileBackend) doneDir() string  { return filepath.Join(b.root, "inbox", "done") }

// Deliver 把信封编码成收件箱文件（框架 → 人类）。
//
// 支持的载荷与文件形态（类型断言失败 = 协议 bug，显式报错）：
//
//	MsgGateRequest  → gate_<RequestID>.md（frontmatter nonce + @ 命令模板）
//	MsgDirect       → direct_<ulid>.md（人类经由 CLI 发送时的文件形态）
//	MsgChildReport  → report_<ulid>.md（纯投递，无回执路径）
//	MsgEscalation   → escalation_<ulid>.md（纯投递；主流路径走 escalate
//	                  包自己的 requests/ 信箱——这里是防御性兜底）
func (b *FileBackend) Deliver(env Envelope) error {
	ulid, err := store.NewMessageID()
	if err != nil {
		return fmt.Errorf("actor: ulid: %w", err)
	}
	ulid = strings.ToLower(ulid)
	var name, body string
	switch env.Type {
	case proto.MsgGateRequest:
		req, ok := env.Payload.(*proto.GateRequest)
		if !ok || req == nil {
			return fmt.Errorf("actor: gate_request payload type mismatch (from %s)", env.From)
		}
		name = "gate_" + req.RequestID + ".md"
		body = gateFileOf(b.human, req)
	case proto.MsgDirect:
		msg, ok := env.Payload.(*proto.DirectMessage)
		if !ok || msg == nil {
			return fmt.Errorf("actor: direct payload type mismatch (from %s)", env.From)
		}
		name = "direct_" + ulid + ".md"
		body = directFileOf(env, msg)
	case proto.MsgChildReport:
		report, ok := env.Payload.(*proto.ChildReport)
		if !ok || report == nil {
			return fmt.Errorf("actor: child_report payload type mismatch (from %s)", env.From)
		}
		name = "report_" + ulid + ".md"
		body = reportFileOf(env, report)
	case proto.MsgEscalation:
		req, ok := env.Payload.(*proto.EscalationRequest)
		if !ok || req == nil {
			return fmt.Errorf("actor: escalation payload type mismatch (from %s)", env.From)
		}
		name = "escalation_" + ulid + ".md"
		body = escalationFileOf(env, req)
	default:
		return fmt.Errorf("actor: %s 不是人类收件箱的投递形态（回执类消息经 Receive 方向流转）", env.Type)
	}
	path := filepath.Join(b.inboxDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("actor: write %s: %w", name, err)
	}
	// 当轮凭据登记（回执对账的锚点——原则 4 的验证面）。
	if env.Type == proto.MsgGateRequest {
		if greq, ok := env.Payload.(*proto.GateRequest); ok && greq != nil {
			b.mu.Lock()
			b.issued[greq.RequestID] = greq.Nonce
			b.mu.Unlock()
		}
	}
	return nil
}

// Receive 启动轮询 watcher 并返回读端（幂等：watcher 只启动一次）。
//
// goroutine 所有权：本方法（唯一 owner）；退出条件 = Stop 或宿主 ctx 取消。
func (b *FileBackend) Receive() <-chan Envelope {
	b.stopWatchOnce.Do(func() {
		go b.watch()
	})
	return b.ch
}

// Stop 停止 watcher（幂等）。
func (b *FileBackend) Stop() {
	b.stopWatchOnce.Do(func() { close(b.stopWatch) })
}

// watch 是收件箱的轮询循环（mtime 静默窗；解析成功的回执搬入 done/）。
func (b *FileBackend) watch() {
	ticker := time.NewTicker(b.poll)
	defer ticker.Stop()
	for {
		select {
		case <-b.stopWatch:
			return
		case <-ticker.C:
			b.scanOnce()
		}
	}
}

// scanOnce 扫描一轮：inbox/ 下每个文件按形态解析（静默窗内的跳过）。
func (b *FileBackend) scanOnce() {
	entries, err := os.ReadDir(b.inboxDir())
	if err != nil {
		return // 收件箱被外力挪走：下一轮重试（控制面在人类手上，自愈）
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b.consider(e.Name())
	}
}

// quietFor 是**类型化静默窗**（[阶段 12 修正/真机发现 #7] Part 14.4 的
// 策略精确化）：静默窗防的是"编辑中的半截读取"——它的前提是**人类在
// 就地编辑**。两形态的写入者不同：
//
//	gate   —— 人类编辑（多击键脉冲）→ 10s 静默（防半截裁决）；
//	direct —— marl say 的 CLI 单次原子写 → 1s（纯传输延迟；10s 的编辑
//	          窗对机器写入是纯死等——真机实录：6 次注入 5 次因静默窗
//	          未到而错过 run 的剩余寿命）。
//
// 不对称是数据（frontmatter 的 type 字段），不是机制分叉。
func quietFor(fileType string, base time.Duration) time.Duration {
	if fileType == "direct" {
		if base > time.Second {
			return time.Second
		}
		return base
	}
	return base
}

// consider 对单个文件做静默窗判定与解析（状态经 seenMod/seenFirst）。
func (b *FileBackend) consider(name string) {
	path := filepath.Join(b.inboxDir(), name)
	st, err := os.Stat(path)
	if err != nil {
		delete(b.seenMod, name)
		delete(b.seenFirst, name)
		return
	}
	mod, first := b.seenMod[name], b.seenFirst[name]
	if st.ModTime() != mod {
		b.seenMod[name] = st.ModTime()
		b.seenFirst[name] = time.Now()
		return // 静默窗重新计时
	}
	if first.IsZero() || time.Since(first) < quietFor(fileTypeOf(name), b.quiet) {
		return // 静默中
	}
	content, rerr := os.ReadFile(path)
	if rerr != nil {
		return
	}
	env, consumed := b.parse(string(content))
	if !consumed {
		return // 纯投递形态 / 未裁决 / 凭据不匹配：继续留观
	}
	// 先归档再投递（可见即已消费的 exactly-once 语义：信封出现在读端的
	// 同一时刻，文件已不在收件箱——重扫不会二次解析）。
	b.archive(name)
	b.deleteSeen(name)
	select {
	case b.ch <- env:
	default:
		// 读端满：信封无处放——文件已归档，重投面在 done/ 的人工迁移
		//（缓冲 32 的满载是宿主停止消费的故障面，静默重试会掩盖它）。
		fmt.Printf("actor: inbox read side full（%s 已归档 done/，回执需人工迁移）\n", name)
	}
}

// fileTypeOf 从文件名取类型（命名约定 <type>_<ulid>.md；未知类型按最保守
// 处置 = 全长静默窗）。
func fileTypeOf(name string) string {
	if i := strings.IndexByte(name, '_'); i > 0 {
		return name[:i]
	}
	return ""
}

// parse 是收件箱文件的解析入口（带 nonce 对账——gate 回执必须原样带回
// Deliver 时登记的当轮凭据）。
func (b *FileBackend) parse(content string) (Envelope, bool) {
	env, ok := parseInboxFile(content, b.human, func(requestID, nonce string) bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		want, ok := b.issued[requestID]
		return ok && want == nonce
	})
	return env, ok
}

// archive 把已消费的文件搬入 done/（控制面事实保留——审计可回看）。
func (b *FileBackend) archive(name string) {
	in := filepath.Join(b.inboxDir(), name)
	out := filepath.Join(b.doneDir(), name)
	data, err := os.ReadFile(in)
	if err != nil {
		return
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return
	}
	_ = os.Remove(in)
}

func (b *FileBackend) deleteSeen(name string) {
	b.seenMod[name] = time.Time{}
	b.seenFirst[name] = time.Time{}
}

// PumpReceive 消费收件箱读端并把回执信封路由到目的地（宿主侧的统一
// 接收环；ScriptedHuman 与 cmd/start 共用同一形态）。
//
// route 的失败 = 路由断链（目标不存在等）——记 audit 由 route 自己负责；
// 本泵对 error 的处置是**停止**（fail-fast：静默重试会让"回执丢失"不可
// 归因）。goroutine owner = 调用方；退出条件 = ctx 取消或 route 返回 error。
func PumpReceive(ctx context.Context, b MailboxBackend, route func(Envelope) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-b.Receive():
			if !ok {
				return
			}
			if err := route(env); err != nil {
				return
			}
		}
	}
}
