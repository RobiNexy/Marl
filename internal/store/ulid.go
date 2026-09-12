package store

// ULID 生成（Part 3.2：MessageID 必须是 ULID，按生成时间可排序）。
//
// 阶段 0/1 的"不引入第三方依赖"约束延续到阶段 2：标准 ULID 的字节布局
// （48 位 Unix 毫秒时间戳 + 80 位随机数 = 128 bit，编码成 26 个 Crockford
// Base32 字符）用 ~50 行即可逐字符对齐规范（ulid spec v0.1.0，
// github.com/ulid/spec，2026-09 核对；时间戳 48 bit 占最高位，因此首字符
// 只有 3 个有效位且恒为零——这也是"按生成时间可排序"的来源）。
// 这里只做生成侧：ID 的解析尚无消费点，不做 YAGNI 部件。
//
// 并发与排序保证：单进程内的同毫秒单调被刻意放弃——ID 只在跨 Agent /
// 跨进程的场景参与排序，句内排序键是 Seq（Part 3.2 与 SQLite 的
// (agent_id, seq) 唯一约束）。转而为随机数提供 crypto/rand 全宽熵，
// 碰撞概率在生成速率远低于随机空间时不可见。

import (
	"crypto/rand"
	"time"
)

// Crockford 的 Base32 字母表（不含 I/L/O/U——ulid spec 的编码字符集）。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const ulidLen = 26 // 128 bit → 25 组 5 bit + 余 3 bit（末组补位）

// NewMessageID 生成一条新的消息 ID（ULID 形态）。
//
// 失败：crypto/rand 不可用（熵源故障）→ 原样返回 error。调用方不得为
// 进程随机源准备"回退算法"——回退到弱随机（时间戳+计数）意味着碰撞
// 概率骤升，而 ID 冲突造成的后果是**数据串联**（两条真相被识别为同一条）。
// 熵源故障的正确处置是报错，不是降级。
func NewMessageID() (string, error) {
	var raw [16]byte
	ms := time.Now().UnixMilli() // [版本依赖: Go 1.1+] time.UnixMilli（1.17+ 语义未变）
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", err
	}
	return encodeULID(raw), nil
}

// encodeULID 把 128 bit 的 ULID 值编码成 26 个 Crockford 字符。
//
// 位布局（ulid spec）：128 bit = 3 bit + 25×5 bit 的**非均匀**分组——
// 首字符只有 3 个有效位（128 bit 的最高 3 位），其余 25 个字符各占 5 bit、
// 从**低位**对齐。验证向量：全零值 → 26 个 '0'；时间戳 1469918176385 的
// 前 10 字符为 "01ARYZ6S41"（两处断言都在测试里）。
//
// 方法：先把 16 字节的缓冲左移 3 bit（制造首字符的 3-bit 边界），
// 然后对剩余字节做逐次剥 5 bit 的循环。
func encodeULID(raw [16]byte) string {
	// 左移 3 bit：bit127..125 撑出首字符；pad 高位补零。
	var shifted [16]byte
	for i := 0; i < 16; i++ {
		lo := byte(0)
		if i < 15 {
			lo = raw[i+1] >> 5
		}
		shifted[i] = raw[i]<<3 | lo
	}
	out := make([]byte, ulidLen)
	out[0] = crockford[raw[0]>>5&0b111]
	for i := 0; i < 25; i++ {
		out[i+1] = crockford[takeTop5(&shifted)]
	}
	return string(out)
}

// takeTop5 取 raw 的最高 5 bit 并将 raw 左移 5 bit（就地修改）。
//
// 并发：接收者独占（由 encodeULID 的栈数组保证，不逃逸）；未导出是默认态
// （导出无消费意图），测试经 encodeULID 的已知向量覆盖。
func takeTop5(raw *[16]byte) byte {
	v := raw[0] >> 3
	for i := 0; i < 15; i++ {
		raw[i] = raw[i]<<5 | raw[i+1]>>3
	}
	raw[15] <<= 5
	return v
}
