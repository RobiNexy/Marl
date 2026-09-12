package agent

// 讨论意图与阻塞语义（Part 11.2 / 8.2 / 8.7，13.10 阶段 8）。
//
// 闭环流程（Part 11.2 + 13.10 测试条目）：
//
//	Agent 调 request_discussion(topic, draft)
//	  → Manager.Open（讨论分支 + 控制面目录 + verdict 模板）
//	  → eventLoop 结束时返回 errDiscussing → Run 外层 awaitDiscussion
//	  → Blocked(Discussing)（不烧钱：无任何 LLM 调用）
//	  → 人类编辑 verdict.md（批注）→ Agent 恢复、批注入 Log/View、
//	    下一轮 LLM 响应/修订草稿（intentDiscussion 复用为 UpdateDraft）
//	  → 人类 @approve → Finalize（checkout trunk + 落地 + author=human）
//	    → 结论条目入 Log/View → 循环继续（或自然结束）。
//
// 阻塞与单写者：讨论阻塞不驱逐 pump（子的 report 与讨论互不排斥——
// 父在 Blocked(Discussing) 期间子照样报告，缓冲在 childReports，
// 恢复后的 errWaitChildren 路径统一落库）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"marl/internal/discuss"
	"marl/internal/skill"
	"marl/internal/types"
)

// DiscussionConfig 是讨论机制的装配参数（Config.Discussion 非 nil 即启用）。
type DiscussionConfig struct {
	// Manager 是讨论生命周期关口（internal/discuss.Manager；测试用假实现）。
	Manager DiscussionManager
}

// DiscussionManager 是 Agent 对 discuss.Manager 的窄接口（消费侧定义）。
//
// Session 由 Agent 持有：它承载"讨论进行中"的运行态（nonce/轮次），
// Manager 是无状态的关口。
type DiscussionManager interface {
	Open(ctx context.Context, req discuss.OpenRequest) (*discuss.Session, error)
	UpdateDraft(ctx context.Context, sess *discuss.Session, draft string) error
	Wait(ctx context.Context, sess *discuss.Session) (discuss.Outcome, error)
	RotateVerdict(sess *discuss.Session) error
	Finalize(ctx context.Context, sess *discuss.Session) (string, error)
}

// errDiscussing 是 eventLoop 的内部哨兵：讨论进行中，主循环需要进入
// Blocked(Discussing) 等人类（Run 捕获 errors.Is，不外泄）。
var errDiscussing = errors.New("agent: discussion in progress")

// discussArgs 是 request_discussion 的参数形态。
type discussArgs struct {
	Topic string `json:"topic"`
	Draft string `json:"draft"`
}

// intentDiscussion 处理 request_discussion。
//
// 两种形态（Part 11.2 的多轮语义）：
//   - 讨论未开：Open（登录到 discuss manager 的裁决关口）；
//   - 讨论进行中：UpdateDraft（agent 收到批注后的修订——同一次讨论的
//     新草稿版本，author=agent 的分支 commit）。
//
// 失败口径：Open/Update 的基础设施错误上抛（eventLoop 终结会话）；裁决
// 拒绝一类不存在——讨论没有配置面上的"不批准"（它本身就是人类审阅的
// 载体）。拒绝的近似形态是"草稿为空"（BAD_ARGS）。
func (a *Agent) intentDiscussion(ctx context.Context, call types.ToolCall) (*skill.SkillResult, error) {
	if a.discussCfg == nil || a.discussCfg.Manager == nil {
		return skill.NewFailure(ErrIntentNotHandled, "讨论机制未装配（框架级缺失；如实回填不装死）"), nil
	}
	var args discussArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			a.roundFormatErrors++
			return skill.NewFailure("BAD_ARGS", "arguments not valid JSON: %v", err), nil
		}
	}
	if args.Topic == "" {
		return skill.NewFailure("BAD_ARGS", "topic 不能为空：人类需要知道讨论什么"), nil
	}
	if args.Draft == "" {
		return skill.NewFailure("BAD_ARGS", "draft 不能为空：空草稿等价于把讨论成本交给你自己"), nil
	}
	if a.discussSess != nil {
		// 进行中：修订草稿（下一轮批注处理后的自然循环——Part 11.2 流程 5→3）。
		if err := a.discussCfg.Manager.UpdateDraft(ctx, a.discussSess, args.Draft); err != nil {
			return nil, fmt.Errorf("intend discuss draft update: %w", err)
		}
		a.mu.Lock()
		a.discussPending = true
		a.mu.Unlock()
		return skill.NewSuccess(map[string]any{
			"message": "草稿已修订（author=agent 的讨论分支 commit）。你将被再次暂停，直到人类的下一条批注或裁决出现。",
		}), nil
	}
	// 新讨论。
	sess, err := a.discussCfg.Manager.Open(ctx, discuss.OpenRequest{
		AgentID: a.id,
		Topic:   args.Topic,
		Draft:   args.Draft,
	})
	if err != nil {
		return nil, fmt.Errorf("intend discuss open: %w", err)
	}
	a.mu.Lock()
	a.discussSess = sess
	a.discussPending = true
	a.mu.Unlock()
	a.auditf(ctx, "discussion_requested", sess.ID, map[string]any{
		"agent_id": string(a.id), "topic": args.Topic,
	})
	// 主 Log 只记两条里的第一条（Part 11.2 流程 7：发起 + 结束；中间态
	// 在讨论分支的 fossil 历史里，不进主 Log）。
	e := types.NewLogEntry(a.id, types.RoleUserInput,
		fmt.Sprintf("Agent 发起讨论: %s", args.Topic))
	if _, err := a.log.Append(ctx, e); err != nil {
		return nil, err
	}
	a.addToView(RoleForLogRole(e.Role), e.ID)
	return skill.NewSuccess(map[string]any{
		"status":  "discussing",
		"message": "讨论已开启，等待人类审阅。期间你被暂停，直到批注或裁决出现。",
	}), nil
}

// awaitDiscussion 进入 Blocked(Discussing) 并等待一次有效裁决（Part 8.2
// Blocked 的子类型）。期间不烧钱：无 LLM 调用、无轮次推进。
//
// 裁决恢复的三条路径（Run 循环之上的语义）：
//   - Annotation：批注入 Log(RoleHumanNote)+View；verdict 重置新 nonce
//     （旧轮残留的 @approve 从此不再有效——伪造裁决的通道被切断）；
//     会话保留（Agent 在下一轮响应，可能 UpdateDraft）；
//   - Approved：Finalize（结论落地 author=human）；落库一条"讨论结束"
//     的 user 消息；会话清空（任务继续）；
//   - ctx 取消：恢复 Running 并上抛（调用方决定是否重启；讨论会话保留，
//     谁也别丢——重新 Run 会继续同一个等待）。
func (a *Agent) awaitDiscussion(ctx context.Context) error {
	if a.discussSess == nil || a.discussCfg == nil {
		return nil // 无讨论可等（状态机错乱的编程错误面；显式返回而非 panic）
	}
	sess := a.discussSess
	a.state = types.StateBlocked
	a.blockReason = types.BlockDiscussing
	a.auditState(ctx, "blocked", string(types.BlockDiscussing))
	a.discussPending = false // 恢复后的下一轮不被同一条 errDiscussing 重入
	outcome, err := a.discussCfg.Manager.Wait(ctx, sess)
	a.state = types.StateRunning
	a.blockReason = ""
	a.auditState(ctx, "running", "")
	if err != nil {
		return err // ctx 取消（唯一失败路径）
	}
	switch outcome.Kind {
	case discuss.OutcomeApproved:
		hash, ferr := a.discussCfg.Manager.Finalize(ctx, sess)
		if ferr != nil {
			// 落地失败 = 结论没进主干：上抛（Agent 不能"当作讨论完成"）。
			return fmt.Errorf("agent: discussion finalize: %w", ferr)
		}
		e := types.NewLogEntry(a.id, types.RoleUserInput,
			fmt.Sprintf("讨论结束，结论已落地: %s（%s）", sess.Target, shortRef(hash)))
		e.Meta = map[string]any{"discussion_id": sess.ID, "round": outcome.Round}
		if _, err := a.log.Append(ctx, e); err != nil {
			return fmt.Errorf("agent: append discussion conclusion: %w", err)
		}
		a.addToView(RoleForLogRole(e.Role), e.ID)
		a.mu.Lock()
		a.discussSess = nil
		a.mu.Unlock()
	default:
		if outcome.Annotation != "" {
			e := types.NewLogEntry(a.id, types.RoleHumanNote, outcome.Annotation)
			e.Meta = map[string]any{"discussion_id": sess.ID, "round": outcome.Round}
			if _, err := a.log.Append(ctx, e); err != nil {
				return fmt.Errorf("agent: append discussion annotation: %w", err)
			}
			a.addToView(RoleForLogRole(e.Role), e.ID)
		}
		if err := a.discussCfg.Manager.RotateVerdict(sess); err != nil {
			return fmt.Errorf("agent: rotate verdict: %w", err)
		}
	}
	return nil
}

// shortRef 缩短 hash 显示（审计可见性；无 hash 时不加噪音）。
func shortRef(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
