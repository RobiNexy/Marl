package store

// SQLiteStore 是 MessageLog 与 ViewStore 的 SQLite 实现（13.4 阶段 2）。
//
// 一个进程一个 supervisor.db 文件，两张表：
//
//	log_entries —— Message Log（真相之源，只追加，见 Part 3.2）
//	views       —— ContextView（可变投影 JSON blob，见 Part 3.3）
//
// 分层定位：本实现是 store.OpenSQLite 的产出，只做"SQL ↔ 结构"的翻译；
// 增删排序/审计事件等编排语义不在这里。
//
// 并发：database/sql 连接池 + SQLite 的串行写；同一 Agent 的 Seq 分配在
// BEGIN IMMEDIATE 事务里"读 MAX+1 → 插入"，由 SQLite 的写锁保证原子
// （无空洞、无重号——MessageLog.Append 的硬契约）。
//
// 关闭：Close 关闭连接池；关闭后的任何操作返回 ErrClosed（store/errors.go 的哨兵）。

import (
	"database/sql"
	"fmt"
	"sync/atomic"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（无 cgo）
)

// sqliteSchema 是 supervisor.db 的建表语句（版本化的单一来源）。
//
// 逐列设计理由：
//   - source_ids / meta / token_actual 存 JSON 文本：目的是"引用结构与
//     人类可读并重"（design doc Part 13.2 的"SQLite 里 SELECT 肉眼可查"）；
//   - created_at 用 unix 毫秒整数：不用 TEXT 日期（比较慢）也不用 REAL
//     （精度损失在排序上不可接受——Seq 之外 created_at 仅作辅助排序）；
//   - UNIQUE (agent_id, seq)：Seq 分配的原子性由它最后兜底，越界的
//     重复插入直接报 UNIQUE 错误而不是出现两条同（agent, seq）的真相。
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS log_entries (
	id          TEXT PRIMARY KEY,
	agent_id    TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	role        TEXT NOT NULL,
	content     TEXT NOT NULL DEFAULT '',
	prov        TEXT NOT NULL,
	source_ids  TEXT NOT NULL DEFAULT '[]',
	meta        TEXT,
	audience    TEXT NOT NULL,
	token_est   INTEGER NOT NULL DEFAULT 0,
	token_actual TEXT,
	created_at  INTEGER NOT NULL,
	UNIQUE (agent_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_log_agent_seq ON log_entries (agent_id, seq);
CREATE INDEX IF NOT EXISTS idx_log_role ON log_entries (agent_id, role);

CREATE TABLE IF NOT EXISTS views (
	agent_id  TEXT PRIMARY KEY,
	data      TEXT NOT NULL,
	saved_at  INTEGER NOT NULL
);
`

// OpenSQLite 打开（或创建）supervisor.db。
//
// 失败：无法打开 / 迁移失败 → 底层错误（fail fast）。
// （Log 写不进去意味着整个框架的"真相之源"不存在——降级运行不是容错，
// 是把数据完整性问题伪装成正常状态。）
func OpenSQLite(path string) (*SQLiteStore, error) {
	// ?_pragma=busy_timeout：并发写时的等待上限；modernc 驱动以这个参数名
	// 区分于 connection-string，切到 mattn 驱动时值与名会重审。
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &SQLiteStore{db: db}, nil
}

// SQLiteStore 同时实现 store.MessageLog 与 store.ViewStore
// （阶段 2 两个接口共用一个文件，见表结构注释）。
type SQLiteStore struct {
	db *sql.DB
	// shutdown 是只增置位：Close 后所有操作返回 ErrClosed。
	shutdown atomic.Bool
}

// Close 关闭连接池。刚关后的操作返回 ErrClosed，双关本身也返回 ErrClosed。
func (s *SQLiteStore) Close() error {
	if s.shutdown.Swap(true) {
		return fmt.Errorf("store: close twice: %w", ErrClosed)
	}
	return s.db.Close()
}

func (s *SQLiteStore) closed() bool { return s.shutdown.Load() }
