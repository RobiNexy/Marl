package tui

// 消息与异步命令面：Gateway 的订阅通道 → tea.Msg 的唯一桥（bubbletea
// 的 TEA 纪律：一切状态变化经 Update，goroutine 只通过 Cmd 送回消息）。

import (
	"context"
	"time"

	"github.com/RobiNexy/Marl/internal/frontend/gateway"
	"github.com/RobiNexy/Marl/internal/types"

	tea "github.com/charmbracelet/bubbletea"
)

// updateMsg 是 Gateway 的一帧快照（含本轮增量事件；err 为非致命查询错误）。
type updateMsg struct {
	State  *gateway.State
	Events []gateway.DomainEvent
	Err    error
}

// convMsg 是选中 agent 的会话拉取结果（会话轮询的产出）。
type convMsg struct {
	agent   string // 缓存键：过期响应（选中已切换）会被丢弃
	entries []*types.LogEntry
	err     error
}

// toastTick 是 toast 过期信号（触发清屏）。
type toastTick struct{}

// convTick 驱动会话轮询的节拍（自续：每个 convTick 触发下一次拉取）。
type convTick struct{}

// listenOnce 订阅一次 Gateway 更新（返回后由 Update 重新注册——经典
// 一次性 Cmd 模式：通道语义由 Gateway 契约保证，满时丢帧无损）。
func listenOnce(gw *gateway.Gateway) tea.Cmd {
	return func() tea.Msg {
		ch, cancel := gw.Subscribe()
		defer cancel()
		u, ok := <-ch
		if !ok {
			return tea.Quit() // Gateway 关闭（父级退出）→ 界面随之退出
		}
		return updateMsg{State: u.State, Events: u.Events, Err: u.Err}
	}
}

// pollConv 按步长轮询选中 agent 的会话（自续节拍；agent 切换时旧轮询
// 的响应以 convMsg.agent 过期丢弃——缓存一致性由键校验保证）。
func pollConv(gw *gateway.Gateway, agent string) tea.Cmd {
	return func() tea.Msg {
		<-time.After(time.Second)
		ctx, cancel := contextWithTimeout()
		defer cancel()
		entries, err := gw.Conversation(ctx, agent)
		return convMsg{agent: agent, entries: entries, err: err}
	}
}

// toastTickCmd 定时清 toast（3s；一次性）。
func toastTickCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return toastTick{} })
}

// contextWithTimeout 是会话拉取的超时上下文（独立函数：测试可替换）。
func contextWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
