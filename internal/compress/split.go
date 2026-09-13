package compress

// 禁切区扫描（Part 3.5 的"吸附到禁切区外"的输入）。
//
// 阶段 3 范围（13.5 "不做"清单）：代码块与引用块；XML 标注块延后——
// orchestrate.NoSplitZoneScanner 接口已就位，补齐时只改本文件。

import (
	"strings"

	"github.com/RobiNexy/Marl/internal/orchestrate"
)

// ZoneScanner 实现 orchestrate.NoSplitZoneScanner。
//
// 零值可用：ZoneScanner{} 即可工作（无状态纯函数）——它是少数零值合法
// 的类型，因为扫描规则不依赖任何配置。
type ZoneScanner struct{}

// ScanNoSplitZones 扫描 content 中的禁切区（行号 1-based 闭区间）。
//
// 规则：
//   - 代码块：``` 围栏行开启/关闭；区 = 开启行到关闭行（含两行围栏）。
//     未关闭的围栏延伸到文末（fail-closed：半个代码块更不能切）；
//   - 引用块：连续的以 ">" 起始（允许前导空白）的行。
//
// 未识别的 ZoneKind 不会由本扫描器产出（它只产两种）；消费侧对未识别
// 种类的 fail-closed 语义见 orchestrate.ZoneKind 契约。
//
// 并发：纯函数。
func (ZoneScanner) ScanNoSplitZones(content string) []orchestrate.NoSplitZone {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var zones []orchestrate.NoSplitZone
	inFence := false
	fenceStart := 0
	// 经典 for 循环：引用块要向后吞并连续行，range 的循环变量是副本，
	// 改它跳不过行（Go 语义，不是风格问题）。
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(trimmed, "```"):
			if !inFence {
				inFence, fenceStart = true, i+1
			} else {
				zones = append(zones, orchestrate.NoSplitZone{Kind: orchestrate.ZoneCodeFence, Start: fenceStart, End: i + 1})
				inFence = false
			}
		case !inFence && strings.HasPrefix(trimmed, ">"):
			// 引用块：向后吞并连续引用行。
			end := i + 1
			for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), ">") {
				end++
			}
			zones = append(zones, orchestrate.NoSplitZone{Kind: orchestrate.ZoneBlockquote, Start: i + 1, End: end})
			i = end - 1 // 跳过已吞并行
		}
	}
	if inFence {
		// 未关闭的围栏：延伸到文末（fail-closed）。
		zones = append(zones, orchestrate.NoSplitZone{Kind: orchestrate.ZoneCodeFence, Start: fenceStart, End: len(lines)})
	}
	return zones
}
