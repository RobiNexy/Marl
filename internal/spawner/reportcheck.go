package spawner

// report 的机械检查（Part 9.6，原则 4：不采信 Agent 自述）。
//
// 检查是纯 grep 级的，不依赖 LLM 配合：
//   - 声称 success 但 writable 范围内有 TODO(agent) → 降级 partial；
//   - 声称 success 但"声称改了文件"而框架记录的改动为空 → 降级 failed；
//   - report 提到的路径不存在 → 记告警（Blockers 之外单独返回），不降级。
//
// "声称改了文件"的机械判据是保守的关键词表（写/修改/创建/删除 +
// wrote/modified/created/updated/deleted）——它是启发式 [推断]，误报方向
// 是"把纯阅读任务的总结误判为改文件声明"，因此关键词要求与路径样 token
// 同时出现；漏报方向（真改了却没说）由 TODO 扫描兜底。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"marl/internal/proto"
)

// changeClaimRe 匹配"声称改了文件"的句子（中英动词 + 路径样 token）。
var changeClaimRe = regexp.MustCompile(
	`(写入|修改|创建|删除|更新|wrote|modified|created|updated|deleted)`) // verb
var pathLikeRe = regexp.MustCompile(`[\w\-./]+\.[a-zA-Z0-9]{1,6}\b`) // path-like token

// TODORe 匹配未完成标记（Part 9.6 的第一行检查）。
var TODORe = regexp.MustCompile(`TODO\((?:agent|human)\)`)

// ReportCheckerConfig 是机械检查的参数。
//
// 零值契约：MaxScanFiles 零值 = 不扫描（显式关闭 TODO 检查，合法但要在
// 装配注释里说明理由）；负值非法。
type ReportCheckerConfig struct {
	// Root 是工作区根（writable 模式串的解释基准）。
	Root string
	// MaxScanFiles 限制 TODO 扫描的文件数（大工作区的保险丝）。0 = 关闭扫描。
	MaxScanFiles int
}

// fsReportChecker 是 proto.ReportChecker 的文件系统实现。
type fsReportChecker struct {
	cfg ReportCheckerConfig
}

// NewReportChecker 构造机械检查器。
//
// 失败：Root 为空（没有基准的路径检查等于没有检查）。
func NewReportChecker(cfg ReportCheckerConfig) (proto.ReportChecker, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("spawner: report checker requires workspace root")
	}
	if cfg.MaxScanFiles < 0 {
		return nil, fmt.Errorf("spawner: report checker MaxScanFiles must be >= 0")
	}
	return &fsReportChecker{cfg: cfg}, nil
}

// Check 实现 proto.ReportChecker（契约见接口与 Part 9.6）。
//
// 失败：report 为 nil → ErrInvalid 语义错误；IO 故障 → error（检查器自己
// 的故障不是"检查未通过"）。
func (c *fsReportChecker) Check(report *proto.ChildReport, writablePaths []string) (*proto.ChildReport, error) {
	if report == nil {
		return nil, fmt.Errorf("spawner: nil report")
	}
	if !report.Status.Valid() {
		return nil, fmt.Errorf("spawner: report status %q invalid", report.Status)
	}
	out := *report // 拷贝后降级（原 report 不被修改——调用方可能还要审计原文）
	out.Blockers = append([]string(nil), report.Blockers...)

	if report.Status != proto.ReportSuccess {
		return &out, nil // 只有 success 需要检查（partial/failed 是自认的）
	}

	// 检查 1：TODO(agent) / TODO(human) 扫描（writable 范围）。
	if c.cfg.MaxScanFiles > 0 {
		blockers, err := c.scanTODOs(writablePaths)
		if err != nil {
			return nil, fmt.Errorf("spawner: TODO scan: %w", err)
		}
		if len(blockers) > 0 {
			out.Status = proto.ReportPartial // 降级：success → partial
			out.Blockers = append(out.Blockers, blockers...)
		}
	}

	// 检查 2：声称改了文件但框架记录的改动为空 → failed。
	if len(out.FilesChanged) == 0 && claimsFileChanges(report.Report) {
		out.Status = proto.ReportFailed
		out.Blockers = append(out.Blockers,
			"report 声称修改了文件，但框架没有记录到任何成功的 file_write")
	}
	return &out, nil
}

// scanTODOs 在 writable 模式覆盖的文件里找 TODO(agent)/TODO(human)。
//
// 失败：glob 语法错误 / IO 故障（不是"发现 TODO"——那是正常返回值）。
func (c *fsReportChecker) scanTODOs(writablePaths []string) ([]string, error) {
	var blockers []string
	scanned := 0
	for _, pattern := range writablePaths {
		matches, err := filepath.Glob(filepath.Join(c.cfg.Root, pattern))
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", pattern, err)
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || info.IsDir() {
				continue
			}
			if scanned >= c.cfg.MaxScanFiles {
				return blockers, nil // 保险丝：超出限额的部分不扫（记录在案）
			}
			scanned++
			raw, err := os.ReadFile(m)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", m, err)
			}
			if TODORe.Match(raw) {
				rel, rerr := filepath.Rel(c.cfg.Root, m)
				if rerr != nil {
					rel = m
				}
				blockers = append(blockers, fmt.Sprintf("发现未完成标记：%s", rel))
			}
		}
	}
	return blockers, nil
}

// claimsFileChanges 判断 report 文本是否声称修改了文件（动词 + 路径样
// token 同时出现的行，且**不带否定语境**）。
//
// 否定语境（真机实测的教训）：'nothing modified' / 'no files changed' /
// '未修改' 这类否定句会被动词表误判为改文件声明，把纯阅读任务的成功
// report 错降为 failed。含否定 token 的行不算声明——宁可漏报（TODO 扫描
// 与 FilesChanged 兜底）也不误伤诚实的只读任务。
//
// 并发：纯函数。
func claimsFileChanges(report string) bool {
	for _, ln := range strings.Split(report, "\n") {
		lower := strings.ToLower(ln)
		if !changeClaimRe.MatchString(lower) || !pathLikeRe.MatchString(lower) {
			continue
		}
		if negationRe.MatchString(lower) {
			continue
		}
		return true
	}
	return false
}

// negationRe 匹配否定语境（中英；覆盖"无改动/未修改/no files/nothing"）。
var negationRe = regexp.MustCompile(
	`(nothing|no files|not |never|without|没有|未|无改动|无文件|不涉及)`)
