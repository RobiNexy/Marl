// Package profile 实现 Profile 加载、继承合并与提示词索引（Part 6 / 6.12，
// 13.9 阶段 7）。
//
// 职责边界：本包只做"配置文件 → 校验过的运行时 Profile"这一步变换；它们
// 的消费方（Router / Spawner）自己去解读 Requirement。提示词文件不解析、不
// 拆分（Part 6.1：提示词的力量在人类精心编写的文本里，不在框架的数据结构里）。
//
// 解析器：复用 internal/config 的受限 YAML 子集（项目刻意不引 gopkg.in/yaml.v3；
// 见 config 包注释的取舍说明）。子集不支持的形态（流式列表、锚点、块标量）
// 会在加载期直接报错——配置笔误必须变成报错，不能变成语义漂移。
//
// 契约层（失败模式全集）：
//
//	加载失败（LoadAll）→ error：文件不可读 / YAML 非法 / id 缺失 / 能力名未知
//	引用失败（Get）    → ErrNotFound（哨兵，调用方 errors.Is 判定）
//	继承失败           → 父不存在 / 继承环：加载期报错，不留半配置
//
// 并发：Loader 的读写经 RWMutex；配置加载完成后才被运行时使用。
package profile
