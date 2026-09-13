package discuss

// verdict.md 的检测与裁决解析（Part 11.2 流程 5，13.10 的 fsnotify 条目）。
//
// [偏离文档: fsnotify 库 → mtime 轮询]。设计文档写 fsnotify，本实现用
// 轮询 + Quiescence 静默窗口：人手编辑在 mtime 上表现为快速脉冲序列，
// 静默窗口保证读到的是收笔后的形态；轮询消除了一个第三方依赖，且
// PollInterval / Quiescence 都在 Config 可配，换 fsnotify 时语义不变。

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// verdict 是一次解析后的 verdict 内容（收笔后的整文件）。
type verdict struct {
	nonce string // frontmatter 里声明的当轮凭据
	body  string // frontmatter 之后的正文
}

// pattern 消息无需导出；这里不做任何导出。
const (
	approveCmd    = "@approve"
	rejectCmd     = "@reject"
	annotationHdr = "## 批注"
)

// parseVerdict 解析 verdict.md 的文本形态。
//
// 容错（人类手写的中间态是预期场景，返回错误让 watcher 继续等）：
//   - frontmatter 缺失 / 未闭合 / nonce 行缺失 → 错误（没有凭据的 verdict
//     必是编辑事故或伪造，两者都不可当裁决）。
//
// body 允许为空（只保存了模板 → 继续等批注）。
func parseVerdict(content string) (*verdict, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("verdict: frontmatter missing")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("verdict: frontmatter not closed")
	}
	v := &verdict{}
	for i := 1; i < end; i++ {
		k, val, ok := strings.Cut(lines[i], ":")
		if !ok || strings.TrimSpace(k) != "nonce" {
			continue
		}
		v.nonce = strings.TrimSpace(val)
	}
	if v.nonce == "" {
		return nil, fmt.Errorf("verdict: nonce missing in frontmatter")
	}
	v.body = strings.Join(lines[end+1:], "\n")
	// 协议标记是管道不是内容（结构化前端的"写完"声明不应作为批注进入
	// agent 上下文——escalate.replyOf 同纪律）。
	v.body = strings.ReplaceAll(v.body, types.ReplyFinalMarker, "")
	return v, nil
}

// verdictOutcome 从 body 判定裁决。
//
// 规则（Part 11.2 的关键字约定）：
//   - `@approve` 单独成行 → OutcomeApproved，附带的其余行是审批理由；
//   - 其余（含 `@reject`）→ OutcomeAnnotation。[推断] 最小闭环不实现
//     reject/abandon 的独立语义（13.10 "不做"清单边界；Annotation 通道
//     让模型的下一轮自己处理"人类不同意"的文本，最终 @approve 落地或
//     放弃由下一轮行为表达）。
func verdictOutcome(v *verdict) Outcome {
	// 先剥模板自带提示段（提示词段落里出现了 @approve 的字样——不做
	// 该步剥离，模板自己就会成为"永久批准"——真机测试踩果本坑）；
	// 再在人类书写的区域里找裁决关键词。
	body := annotationBody(strings.TrimSpace(v.body))
	if i := strings.Index(body, approveCmd); i >= 0 {
		rest := body[:i] + body[i+len(approveCmd):]
		return Outcome{Kind: OutcomeApproved, Annotation: annotationBody(excludingLine(rest, approveCmd))}
	}
	return Outcome{Kind: OutcomeAnnotation, Annotation: body}
}

// verdictOutcome 由此暴露给测试（与 Wait 同一轮判定的唯一消费点）。
func readVerdictOutcome(v *verdict) Outcome { return verdictOutcome(v) }

// excludingLine 从文本中去掉单独成行的 cmd。
func excludingLine(s, cmd string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == cmd {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// annotationBody 剥掉模板自带提示段："## 批注"之后的内容才是人类动手的
// 区域；标题缺失时整段按自定义批注处理（人类删掉模板说明是合法编辑）。
func annotationBody(body string) string {
	if i := strings.Index(body, annotationHdr); i >= 0 {
		return strings.TrimSpace(body[i+len(annotationHdr):])
	}
	return strings.TrimSpace(body)
}

// readRandom：newNonce 的实现点（crypto/rand 全宽熵；熵源故障 → 显式
// 报错，不回落弱随机——回落意味着"nonce 可猜"，安全凭据的意义消失）。
func readRandom(b []byte) error {
	_, err := rand.Read(b)
	return err
}

// newNonce 每轮的随机凭据（32 hex 字符）。
func newNonce() (string, error) {
	b := make([]byte, 16)
	if err := readRandom(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// waitOutcome 是 Wait 抽出的可测试核心：不持有 tick 循环（goroutine 面
// 收敛在 Wait 一个方法里——goroutine 的所有者注释见 Wait）。
func waitOnce(cfg *Config, sess *Session, content []byte, now time.Time) (Outcome, bool) {
	v, perr := parseVerdict(string(content))
	if perr != nil {
		return Outcome{}, false
	}
	if got := v.nonce; sess.nonce == "" || got != sess.nonce {
		return Outcome{}, false // 陈旧/伪造：非当轮凭据不是裁决
	}
	return readVerdictOutcome(v), true
}

// Wait 实现 agent.DiscussionManager.Wait：阻塞直到一次**有效**裁决或
// ctx 取消。
//
// 语义：
//   - verdict mtime 变化 → 起静默窗口 → 收笔后读文件；
//   - 解析错误 / nonce 不匹配 → 忽略，继续等（人类编辑中的中间态 /
//     陈旧 frontmatter 不是本轮裁决——nonce 换轮的机制在 RotateVerdict）；
//   - 真实读文件故障（非 NotExist）同样继续轮询不作报错——控制面在人类
//     手上，权限类故障自愈后再来读比僵死合理。
//
// goroutine 所有权与退出条件（本包唯一 goroutine）：Wait 内联循环、无
// 新 goroutine（ticker + select）；退出路径仅有 ctx 取消与返回裁决——
// "每个 goroutine 有文档化 owner"的纪律在此被结构性地豁免：不做就不
// 会泄漏。
func (m *Manager) Wait(ctx context.Context, sess *Session) (Outcome, error) {
	if sess == nil {
		return Outcome{}, fmt.Errorf("discuss: nil session")
	}
	path := filepath.Join(sess.Dir, "verdict.md")
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	var lastMod time.Time
	var lastChange time.Time
	for {
		select {
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		case <-ticker.C:
		}
		st, err := os.Stat(path)
		if err != nil {
			continue // 不存在 / 读取故障：人类还没写或权限未恢复
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		// 快路径（types.ReplyFinalMarker）：结构化裁决（TUI/API 写入）
		// 带声明"内容完整且最终" → 免静默窗立即消费。人类手工编辑无
		// 标记 → 慢路径静默窗照旧。撕裂自愈：截断的标记落回慢路径。
		final := types.HasReplyFinal(string(content))
		if mt := st.ModTime(); mt != lastMod {
			lastMod = mt
			lastChange = time.Now()
			if final {
				if o, ok := waitOnce(&m.cfg, sess, content, time.Now()); ok {
					return o, nil
				}
			}
			continue // 刚变过：起静默窗口（无标记时）
		}
		if final {
			if o, ok := waitOnce(&m.cfg, sess, content, time.Now()); ok {
				return o, nil
			}
			continue // 声明在但凭据/形态不匹配：与"编辑中间态"同等留观
		}
		if lastChange.IsZero() || time.Since(lastChange) < m.cfg.Quiescence {
			continue // 未收笔
		}
		if o, ok := waitOnce(&m.cfg, sess, content, time.Now()); ok {
			return o, nil
		}
	}
}

// RotateVerdict 是一轮批注处理完后重置 verdict（新 nonce + 新模板）。
//
// 设计文档 11.2 流程 5→3 的循环衔接：批注轮结束时 Agent 从 Blocked 恢复
// 去"响应批注"，随后 agent 侧会重启一次等待——新等待必须读**新一轮的
// nonce**，而旧轮 verdict（人类 příště手动写入可能发生在任意时间）里的
// @approve 由此不可能作为上一轮残留被误接受。新模板的重置动画留给了
// 人类的编辑器刷新；框架只负责把内容换成空正文。
func (m *Manager) RotateVerdict(sess *Session) error {
	n, err := newNonce()
	if err != nil {
		return fmt.Errorf("discuss: rotate nonce: %w", err)
	}
	sess.nonce = n
	if err := os.WriteFile(filepath.Join(sess.Dir, "verdict.md"), []byte(verdictTemplate(sess)), 0o644); err != nil {
		return fmt.Errorf("discuss: rewrite verdict template: %w", err)
	}
	return nil
}
