package skill

// 完整技能清单（Part 4.2，共 20 个）。
// 这些名字是稳定的外部契约：一旦进入工具表就不能随意改名（会破缓存前缀）。
const (
	// 文件类
	SkillListDir         = "list_dir"
	SkillFileRead        = "file_read"
	SkillFileSearch      = "file_search"
	SkillFileWrite       = "file_write"
	SkillFileEdit        = "file_edit"
	SkillRestoreSnapshot = "restore_snapshot"

	// 命令类
	SkillShellExec  = "shell_exec"
	SkillShellSpawn = "shell_spawn"
	SkillShellKill  = "shell_kill"

	// 环境类
	SkillGetEnv      = "get_env"
	SkillListPrompts = "list_prompts"

	// 网络类
	SkillWebFetch = "web_fetch"

	// 文献类
	SkillArxivSearch = "arxiv_search"
	SkillArxivFetch  = "arxiv_fetch"

	// 编排类
	SkillSplitMessage    = "split_message"
	SkillExcludeMessage  = "exclude_message"
	SkillRestoreMessage  = "restore_message"
	SkillReorderMessage  = "reorder_message"
	SkillAnnotateMessage = "annotate_message"
	SkillPinMessage      = "pin_message"
)

// SkillCount 是 Part 4.2 规定的技能总数。它是硬约束而非描述：
// skillNameList 的长度与之不符时**编译失败**（见下方断言）。
const SkillCount = 20

// skillNameList 是 20 个技能名的稳定顺序清单（工具表生成、测试、文档共用）。
//
// 顺序即契约：它决定工具表（冻结前缀）里的排列，也就是跨 Agent 共享缓存的
// 字节序列。插入、删除、重排都会让**所有** Agent 的缓存前缀失效（全项目
// 一次冷启动），因此每次改动都必须显式评估缓存成本，不能"顺手加一个"。
//
// 它是私有数组而非导出切片——原因见 SkillNames()。用数组（而非切片）是为了
// 让长度成为常量，从而能在编译期做数量断言。
var skillNameList = [...]string{
	SkillListDir, SkillFileRead, SkillFileSearch, SkillFileWrite, SkillFileEdit, SkillRestoreSnapshot,
	SkillShellExec, SkillShellSpawn, SkillShellKill,
	SkillGetEnv, SkillListPrompts,
	SkillWebFetch,
	SkillArxivSearch, SkillArxivFetch,
	SkillSplitMessage, SkillExcludeMessage, SkillRestoreMessage, SkillReorderMessage, SkillAnnotateMessage, SkillPinMessage,
}

// 编译期不变量：清单长度必须恰好等于 SkillCount。
//
// 这是本文件唯一能**静态**保证"没有漏登记技能"的手段。漏登记的运行时症状是：
// 某个技能永远不出现在工具表里（模型不知道它存在），而框架不报任何错，
// 只能靠人盯着看——这是最容易被忽略的一类契约破坏。
//
// 原理：len(数组) 是常量表达式，长度不符时该下标越界，直接编译失败。
var _ = [1]struct{}{}[SkillCount-len(skillNameList)]

// SkillNames 返回技能名清单的**副本**（稳定顺序）。
//
// 返回副本而非导出切片，防的是"就地改写全局状态"这类静默故障：一个可变的
// 包级 []string 允许任何 import 本包的代码（测试、插件、未来的技能注册表）
// 写 SkillNames[3] = "..."，编译器不会察觉；而症状会是"某些 Agent 的缓存
// 突然集体失效"——排查时几乎必然先怀疑缓存层，而不是一个被改写的全局切片。
//
// [权衡: 每次调用分配一次切片的代价，在"工具表每进程只生成一次（启动期）"
// 的场景下可忽略；换来的是"顺序不可被意外改写"这一可执行保证。]
func SkillNames() []string {
	out := make([]string, len(skillNameList))
	copy(out, skillNameList[:])
	return out
}

// FrameworkMetaSkill 是框架注入的元工具（不属于 20 个技能，读运行时而非外部世界）。
const (
	// SkillListMyChildren 读 AgentRuntime.Children，不遍历 Log（Part 8.1）。
	SkillListMyChildren = "list_my_children"
)
