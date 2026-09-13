package tui

// 键位契约（单一定义点；帮助面板由此自动生成——键位表与帮助永不漂移）。

// keyHelp 是一条键位说明（帮助面板的行）。
type keyHelp struct {
	key  string
	desc string
}

// focusKeys 是焦点切换键（Tab 循环的顺序 = 视觉顺序：树→工作区→侧栏→输入）。
var focusKeys = []keyHelp{
	{"Tab", "切换焦点（树→工作区→侧栏→输入）"},
	{"1/2/3", "直达 树/工作区/侧栏"},
	{"↑↓/Enter", "树中选择/聚焦该 agent"},
	{"a", "行动中心（待你决策的事项）"},
	{"i 或 /", "输入消息（/ 另可唤起命令）"},
	{"Esc", "离开输入回到面板"},
	{"s", "收起/展开侧栏"},
	{"t", "展开/折叠思维链"},
	{"f", "对话自动跟随开关"},
	{"?", "帮助"},
	{"q", "退出（任务保持后台）"},
}

// tabKeys 是工作区切换序列（g 后按的键）。
var tabKeys = map[string]string{
	"t": "对话",
	"e": "事件",
	"i": "检视",
}

// commands 是斜杠命令注册表（命令面板的数据源；与 help 同源——单一
// 定义点纪律）。
type command struct {
	name string // 含斜杠
	args string // 参数提示（可空）
	desc string
}

var commands = []command{
	{"/start", "<任务描述>", "启动任务（运行中被 409 拒绝）"},
	{"/stop", "[force]", "停止任务"},
	{"/say", "<agent> <文本>", "定向插话（下一轮编排可见）"},
	{"/tree", "", "焦点切到 agent 树"},
	{"/events", "", "工作区切到事件流"},
	{"/inspect", "", "工作区切到检视"},
	{"/cost", "", "焦点切到侧栏成本"},
	{"/doctor", "", "环境自检"},
	{"/help", "", "键位与命令帮助"},
	{"/quit", "", "退出 TUI（任务保持后台）"},
}

// parseSlash 把输入折算成命令名 + 参数（不以 / 开头 → 非命令）。
// 返回 ok=false 表示这不是命令（按普通消息发送）。
func parseSlash(text string) (name, args string, ok bool) {
	if len(text) == 0 || text[0] != '/' {
		return "", "", false
	}
	rest := text[1:]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' {
			return "/" + rest[:i], trimLeading(rest[i:]), true
		}
	}
	return "/" + rest, "", true
}

// trimLeading 去前导空白（parseSlash 的参数整理）。
func trimLeading(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			return s[i:]
		}
	}
	return ""
}
