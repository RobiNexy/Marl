//go:build ignore

package skill

import (
	"strings"
)

// allLineNumbers 返回 old 在 text 中的每次出现所在行号（1-based）。
// 行号 = 1 + 出现偏移前的换行数。
func allLineNumbers(text, old string) []int {
	var locs []int
	for off := 0; ; {
		rel := strings.Index(text[off:], old)
		if rel < 0 {
			break
		}
		hit := off + rel
		locs = append(locs, 1+strings.Count(text[:hit], "\n"))
		off = hit + 1
	}
	return locs
}

// lineCol 返回字节偏移对应的 1-based 行号与列号（列按字符数计，多字节安全）。
func lineCol(text string, byteOff int) (int, int) {
	if byteOff > len(text) {
		byteOff = len(text)
	}
	if byteOff < 0 {
		byteOff = 0
	}
	start := 0
	if i := strings.LastIndexByte(text[:byteOff], '\n'); i >= 0 {
		start = i + 1
	}
	col := len([]rune(text[start:byteOff])) + 1
	line := 1 + strings.Count(text[:start], "\n")
	return line, col
}

// bestEffortSnippet 无匹配时生成最接近位置的 3-5 行片段（Part 4.4.5 关键设计：
// 减少试错循环）。[推断] 用字符 bigram Dice 系数滑窗相似度：对"忘了缩进/
// 抄错一行"这类典型错误足以指向正确区域；不引入完整 diff 依赖。
func bestEffortSnippet(text, old string) string {
	docLines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	oldLines := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	wsize := len(oldLines)
	if wsize > len(docLines) {
		wsize = len(docLines)
	}
	if wsize <= 0 {
		return ""
	}
	norm := func(ls []string) string {
		var b strings.Builder
		for _, l := range ls {
			b.WriteString(strings.TrimSpace(l))
			b.WriteByte('\n')
		}
		return b.String()
	}
	target := norm(oldLines)
	bestIdx, bestScore := 0, -1.0
	for i := 0; i+wsize <= len(docLines); i++ {
		score := bigramDice(target, norm(docLines[i:i+wsize]))
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	from := bestIdx - 1
	if from < 0 {
		from = 0
	}
	to := bestIdx + wsize + 1
	if to > len(docLines) {
		to = len(docLines)
	}
	return "...\n" + strings.Join(docLines[from:to], "\n") + "\n..."
}

func bigramDice(a, b string) float64 {
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	set := make(map[string]int, len(a))
	for i := 0; i+2 <= len(a); i++ {
		set[a[i:i+2]]++
	}
	inter := 0
	for i := 0; i+2 <= len(b); i++ {
		g := b[i : i+2]
		if set[g] > 0 {
			set[g]--
			inter++
		}
	}
	return 2.0 * float64(inter) / float64(len(a)-1+len(b)-1)
}
