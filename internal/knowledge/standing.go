package knowledge

// preferences/ 在每个 Agent 的 namespace 里是 read-only（Part 12.2 写权限
// 表："这里装的是你的长期意志"），但**框架**是编译者——编译必须总量受控、
// 逐字节稳定。本文件是编译器与 lint 的唯一实现；Agent 侧经
// agent.Config.StandingOrders 消费编译产物（不 import 本包）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"marl/internal/types"
)

// StandingBlock 是 preferences/ 的编译产物。
//
// Content 是注入 CanonicalRequest 的整块文本（含 <standing_orders> 包裹）；
// Files 是逐文件的 token 报告（lint 的输出形态，Part 12.4 ② 的示例格式）。
type StandingBlock struct {
	Content string
	Tokens  int
	Files   []FileTokens
}

// FileTokens 是一个源文件的 token 报告。
type FileTokens struct {
	Name   string // 相对 preferences/ 的文件名
	Tokens int
}

// excludedFileReadme 在编译前被显式排除的文件（init 骨架的空说明文件；
// [权衡: README.md 也属于 .md，但它装的是目录维护说明而非偏好指令，
// 把它编进常驻块会污染全项目的缓存前缀。内容性的与他类说明放置约定
// 在 doc 注释里声明]。）
const excludedFileReadme = "README.md"

// ReportMarkdown 把编译报告渲染成 lint 的输出形态（Part 12.4 ②）：
//
//	preferences/ 编译后 1247 est-token，超出上限 1000
//	  code-style.md         512
//	  ...
//	请精简至 1000 以内
//
// 超限/不超限都返回文本（CLI 决定展示与否——编译成功时报告也是人类
// 关心"还剩多少预算"的输入）。
func (b *StandingBlock) Report(limit int) string {
	var sb strings.Builder
	status := fmt.Sprintf("preferences/ 编译后 %d est-token", b.Tokens)
	if b.Tokens > limit {
		status += fmt.Sprintf("，超出上限 %d", limit)
	}
	sb.WriteString(status)
	sb.WriteString("\n")
	for _, f := range b.Files {
		fmt.Fprintf(&sb, "  %-24s %d\n", f.Name, f.Tokens)
	}
	if b.Tokens > limit {
		fmt.Fprintf(&sb, "请精简至 %d 以内\n", limit)
	}
	return sb.String()
}

// CompileStandingOrders 读 dir（…/.marl/knowledge/preferences）编译常驻块。
//
// 编译形态（逐字节稳定的全部来源）：
//
//	<standing_orders>          ← 硬边界（Part 12.4 的表述）
//	## <文件名>
//	 <归一化后的文件全文>
//	...
//	</standing_orders>
//
// 归一化 = 换行统一 \n、去尾部空白、文末恰好一个换行。文件名排序、
// 空目录合法（返回 nil 块——没有偏好时该段不注入，编译侧空段剔除）。
//
// 失败：
//   - 目录不可读；
//   - 编译产物超 MaxStandingTokens（Part 12.4 ②：硬失败 + 逐文件报告，
//     不自动截断——截断掉的可能正是那条安全约束）。
func CompileStandingOrders(dir string) (*StandingBlock, error) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("standing orders: read %s: %w", dir, err)
	}
	var mdFiles []string
	for _, e := range names {
		if e.IsDir() {
			continue
		}
		if e.Name() == excludedFileReadme || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		mdFiles = append(mdFiles, e.Name())
	}
	sort.Strings(mdFiles) // 文件名排序 = 内容排序（Part 12.4 ③：无时间戳无计数器）

	block := &StandingBlock{}
	var body strings.Builder
	for _, name := range mdFiles {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("standing orders: read %s: %w", name, err)
		}
		norm := normalize(src)
		ft := FileTokens{Name: name, Tokens: types.EstimateTokens(norm)}
		block.Files = append(block.Files, ft)
		fmt.Fprintf(&body, "## %s\n\n%s\n", name, norm)
	}
	content := ""
	if len(mdFiles) > 0 {
		content = "<standing_orders>\n\n" + body.String() + "</standing_orders>"
	}
	block.Content = content
	block.Tokens = types.EstimateTokens(content)
	if block.Tokens > MaxStandingTokens {
		return nil, fmt.Errorf("standing orders: 编译后 %d est-token，超出上限 %d（超限硬失败：截断常驻指令是最坏结果）\n%s",
			block.Tokens, MaxStandingTokens, strings.TrimRight(block.Report(MaxStandingTokens), "\n"))
	}
	return block, nil
}

// normalize 统一 CRLF → LF、去全部尾部空白行、文末恰好一个 \n
// （Part 12.4 ③ 的字节稳定前提：Windows 编辑器保存的文件与 POSIX 的
// 编译产物必须逐字节一致）。
func normalize(src []byte) string {
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	s = strings.TrimRight(s, " \n")
	return s
}
