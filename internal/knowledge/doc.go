// Package knowledge 实现知识库编译核心：preferences/ → 常驻块
// <standing_orders>（Part 12.2 / 12.3 / 12.4，13.9 阶段 7）。
//
// 常驻块的四条硬约束（Part 12.4）在这里落地：
//
//  1. 上限管的是编译产物，不是源文件——Compile 产物整体估算 1000 est-token；
//  2. 超限硬失败，绝不自动截断——Compile 返回 error 并列出各文件数；
//  3. 编译必须逐字节稳定——文件按名字排序、换行统一 \n、无任何时间戳
//     或计数器；golden test 守护（standing_test.go）；
//  4. 上限的作用是强迫人类保持"自己还敢删"的规模——超限动作是"人来砍"，
//     由 marl knowledge lint 在编辑时暴露。
package knowledge

// MaxStandingTokens 是 <standing_orders> 编译产物的 est-token 硬上限
// （Part 12.3 / 12.4）。est 口径与 types.EstimateTokens 完全同源
// （宁高不低的安全侧）。
const MaxStandingTokens = 1000
