package compress

// SUM 的生成指令与机械校验（Part 3.7 步骤 4-5）。
//
// 校验是机械的：只查"结构是否齐全 + 文件是否存在于工作区"，
// 不查"内容是否属实"——后者按原则 4 由下游机械检查承担，
// 校验器不变成第二个模型。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RobiNexy/Marl/internal/orchestrate"
)

// sumSystemPrompt 是 SUM 生成的 system 段（frozen；七段骨架的权威表述）。
//
// 骨架顺序与 orchestrate.sumSectionList 一一对应（任务/事实/文件/决策/
// 未闭/失败/现场）；改这里必须同步改校验器与 golden 测试。
const sumSystemPrompt = `你是上下文压缩器。输入是一段工作历史（带来源标注的转录）。
把它压缩成一份 Markdown 工作记忆，严格使用以下七个章节标题（顺序固定，编号固定）：

## 1. 任务
当前任务的原始目标与验收口径（保留用户原话中的关键约束）。

## 2. 事实
已经确认成立的事实与读到的关键数据（每条一行，注明来源文件/工具结果）。

## 3. 文件
本任务涉及的工作区文件清单。每行一个条目，格式严格为：
- <workspace相对路径>
只列真实存在且与任务相关的文件；没有则写"无"。

## 4. 决策
已经做出的技术决策与理由（含被否决的备选）。

## 5. 未闭
尚未完成、尚未验证的事项（下一步的入口）。

## 6. 失败
已经失败过的尝试与原因（避免重蹈）。

## 7. 现场
恢复工作所需的现场状态（当前进行到哪一步、关键中间值）。

篇幅纪律：每节 1~3 行，全文不超过 25 行——摘要是工作记忆不是档案，
冗长的摘要会让压缩收益归负（收益判据是 reclaim = (旧-新)/旧）。
语言纪律：摘要语言与输入内容的主要语言一致。

只输出这份 Markdown，不要输出任何解释或围栏。`

// sumValidateMaxRetriesNote 记录重试语义的落点：重试由 Compressor 驱动
// （温度稍高），本文件只提供单次校验。

// pathTokenRe 匹配反引号包裹的路径 token（§3 的权威格式）。
var pathTokenRe = regexp.MustCompile("`([^`\\s]+)`")

// ValidateSUM 实现 orchestrate.Compressor.ValidateSUM（机械校验）。
//
// 检查项（全部聚合报告——一次列全，避免修复者按重试轮次逐个发现）：
//  1. 内容非空；
//  2. 七个章节标题全部存在且顺序不减（标题是持久化判据，见
//     orchestrate 的常量注释）；
//  3. 第 3 节列出的文件路径存在（相对 WorkspaceRoot 解析；拒绝绝对路径
//     与 .. 逃逸——存在性检查不成为路径探测的通道）。
//
// 第 3 节写"无"（或不含任何路径候选）时跳过路径检查——"没有文件"是
// 合法的压缩结论。
//
// 并发：可并发调用（只读 + 文件系统查询，无共享可变状态）。
func (c *SumCompressor) ValidateSUM(_ context.Context, sumContent string) error {
	if strings.TrimSpace(sumContent) == "" {
		return fmt.Errorf("sum validation: content is empty")
	}
	var missing []string
	pos := -1
	for _, header := range orchestrate.SUMSections() {
		idx := strings.Index(sumContent[pos+1:], header)
		if idx < 0 {
			missing = append(missing, fmt.Sprintf("missing section %q", header))
			continue
		}
		pos += 1 + idx // 顺序不减地推进（后一节必须出现在前一节之后）
	}
	body := sectionBody(sumContent, "## 3. 文件", "## 4. 决策")
	paths, saidNone := extractFilePaths(body)
	if !saidNone {
		for _, p := range paths {
			if err := checkPathExists(c.root, p); err != nil {
				missing = append(missing, err.Error())
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("sum validation failed: %s", strings.Join(missing, "; "))
	}
	return nil
}

// sectionBody 截取 start 标题之后、end 标题之前的内容（找不到 end 则到文末；
// 找不到 start 返回空串）。
//
// 并发：纯函数。
func sectionBody(content, start, end string) string {
	i := strings.Index(content, start)
	if i < 0 {
		return ""
	}
	i += len(start)
	if j := strings.Index(content[i:], end); j >= 0 {
		return content[i : i+j]
	}
	return content[i:]
}

// extractFilePaths 从第 3 节正文提取文件路径候选。
//
// 提取规则（按优先级）：
//  1. 反引号包裹的 token（`path`）——指令里声明的权威格式；
//  2. 无反引号候选时，取 "- " 行的剩余部分（模型没完全守格式时的兜底）。
//
// saidNone：正文含"无"且没有候选 → (nil, true)。注意方向：只有
// "明确说无"才跳过存在性检查；"提取不到候选"（模型乱写）不算说无，
// 也不触发检查（没有可检查的对象）——两者都会让校验通过，但后者会在
// 成本报表里以"文件清单为空"的形态可见。
//
// 并发：纯函数。
func extractFilePaths(body string) (paths []string, saidNone bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, false
	}
	for _, m := range pathTokenRe.FindAllStringSubmatch(body, -1) {
		paths = append(paths, m[1])
	}
	if len(paths) == 0 {
		for _, ln := range strings.Split(body, "\n") {
			ln = strings.TrimSpace(ln)
			if !strings.HasPrefix(ln, "- ") {
				continue
			}
			cand := strings.TrimSpace(strings.Trim(strings.TrimPrefix(ln, "- "), "`"))
			if cand != "" && !strings.ContainsAny(cand, " \t") {
				paths = append(paths, cand)
			}
		}
	}
	if len(paths) == 0 && strings.Contains(body, "无") {
		return nil, true
	}
	return paths, false
}

// checkPathExists 校验单个路径：拒绝绝对路径与 .. 逃逸，然后按工作区根
// 解析并要求存在。
//
// 失败：带路径的错误（聚合前的单条形态）。
func checkPathExists(root, p string) error {
	if p == "" {
		return fmt.Errorf("file path is empty")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("file path %q is absolute (workspace-relative required)", p)
	}
	abs := filepath.Join(root, p)
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) && abs != root {
		return fmt.Errorf("file path %q escapes workspace root", p)
	}
	if _, err := os.Stat(abs); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file %q listed in SUM section 3 does not exist", p)
		}
		return fmt.Errorf("file %q: stat: %v", p, err)
	}
	return nil
}
