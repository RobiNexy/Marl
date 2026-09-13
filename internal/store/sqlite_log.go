package store

// SQLite 版 MessageLog 实现（13.4 阶段 2 的 sqlite_log.go）。
//
// 真相之源纪律（Part 3.2）：整张表不存在任何 UPDATE / DELETE 路径。
// 不写这两条语句，是比"记得别写"更可靠的执行不可变性的方式。
//
// Seq 分配：BEGIN IMMEDIATE 事务里 SELECT MAX(seq)+1 → INSERT，
// 同一写锁窗口完成；UNIQUE(agent_id, seq) 是最后防线（见 sqlite.go）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// logCols 是带列名查询的单一来源（防止列序与 decodeLogRow 漂移）。
const logCols = `id, agent_id, seq, role, content, prov, source_ids, meta, audience, token_est, token_actual, created_at`

// jsonIDs 序列化 SourceIDs（nil → "[]"，保持列里永远是合法 JSON）。
func jsonIDs(ids []types.MessageID) (string, error) {
	if ids == nil {
		return "[]", nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Append 实现 store.MessageLog.Append。
//
// 前置校验按 MessageLog 契约的三条硬规则展开：
//   - entry 必须过 Validate()（Epoch/Role/Prov/Audience 零值即拒绝）；
//   - entry 的 ID / Seq / CreatedAt 必须仍为零值（构造期条目的契约）；
//   - 通过后 APPEND 会**就地回填**这三项（entry 此后即冻结）。
func (s *SQLiteStore) Append(ctx context.Context, entry *types.LogEntry) (types.MessageID, error) {
	if s.closed() {
		return "", fmt.Errorf("log append: %w", ErrClosed)
	}
	if entry == nil {
		return "", fmt.Errorf("log append: %w: nil entry", ErrInvalid)
	}
	if vErr := entry.Validate(); vErr != nil {
		return "", fmt.Errorf("log append: %w: %v", ErrInvalid, vErr)
	}
	if entry.ID != "" || entry.Seq != 0 || !entry.CreatedAt.IsZero() {
		return "", fmt.Errorf("log append: %w: entry already carries ID/Seq/CreatedAt (constructor must leave them zero)",
			ErrInvalid)
	}

	idStr, err := NewMessageID()
	if err != nil {
		return "", fmt.Errorf("log append: ulid: %w", err)
	}
	srcJSON, err := jsonIDs(entry.SourceIDs)
	if err != nil {
		return "", fmt.Errorf("log append: source ids: %w", err)
	}
	var metaJSON, actualJSON *string
	if entry.Meta != nil {
		// JSON 键序不确定——但 Meta 只服务审计可读性，不参与请求前缀的字节
		// （Part 3.2：Meta 不进上下文渲染），因此无缓存影响。
		b, bErr := json.Marshal(entry.Meta)
		if bErr != nil {
			return "", fmt.Errorf("log append: meta: %w", bErr)
		}
		metaJSON = new(string)
		*metaJSON = string(b)
	}
	if entry.TokenActual != nil {
		b, bErr := json.Marshal(entry.TokenActual)
		if bErr != nil {
			return "", fmt.Errorf("log append: token usage: %w", bErr)
		}
		actualJSON = new(string)
		*actualJSON = string(b)
	}
	createdAt := time.Now().UTC()

	// BEGIN IMMEDIATE 让"读 MAX → 插入"与任何并发写互斥：显式声明事务意图，
	// 不依赖连接池默认隔离级别（它是"恰好串行"的巧合，不是契约）。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("log append: begin: %w", err)
	}
	defer tx.Rollback() // Commit 后调用无害：ErrTxBroken / "已提交"时忽略

	var nextSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM log_entries WHERE agent_id = ?`,
		string(entry.AgentID)).Scan(&nextSeq); err != nil {
		return "", fmt.Errorf("log append: next seq: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO log_entries
			(id, agent_id, seq, role, content, prov, source_ids, meta, audience,
			 token_est, token_actual, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		idStr, string(entry.AgentID), nextSeq,
		string(entry.Role), entry.Content, string(entry.Prov),
		srcJSON, metaJSON, string(entry.Audience),
		entry.TokenEst, actualJSON, createdAt.UnixMilli()); err != nil {
		return "", fmt.Errorf("log append: insert agent=%s role=%s: %w", entry.AgentID, entry.Role, err)
	}
	if txErr := tx.Commit(); txErr != nil {
		return "", fmt.Errorf("log append: commit: %w", txErr)
	}
	// Commit 成功后的 deferred Rollback 必然返回 sql.ErrTxnDone 类错误，
	// 无害且无法避免（database/sql 没有"提交后取消 defer"的 API）——忽略即可。

	// 就地回填（MessageLog 契约：传入的 entry 是输出参数，Append 后即冻结）。
	entry.ID = types.MessageID(idStr)
	entry.Seq = nextSeq
	entry.CreatedAt = createdAt
	return types.MessageID(idStr), nil
}

// Get 实现 store.MessageLog.Get。失败：不存在 → ErrNotFound。
func (s *SQLiteStore) Get(ctx context.Context, id types.MessageID) (*types.LogEntry, error) {
	if id == "" {
		return nil, fmt.Errorf("log get: %w: empty id", ErrInvalid)
	}
	e, err := s.scanOne(ctx,
		`SELECT `+logCols+` FROM log_entries WHERE id = ?`, string(id))
	if err != nil {
		return nil, fmt.Errorf("log get %s: %w", id, err)
	}
	return e, nil
}

// GetBySeq 实现 store.MessageLog.GetBySeq。
// 失败：不存在 → ErrNotFound；seq == 0 → ErrInvalid（0 是哨兵不是合法 Seq）。
func (s *SQLiteStore) GetBySeq(ctx context.Context, agentID types.AgentID, seq int64) (*types.LogEntry, error) {
	if agentID == "" {
		return nil, fmt.Errorf("log getBySeq: %w: empty agentID", ErrInvalid)
	}
	if seq == 0 {
		return nil, fmt.Errorf("log getBySeq: %w: seq is sentinel 0", ErrInvalid)
	}
	e, err := s.scanOne(ctx,
		`SELECT `+logCols+` FROM log_entries WHERE agent_id = ? AND seq = ?`,
		string(agentID), seq)
	if err != nil {
		return nil, fmt.Errorf("log getBySeq %s/%d: %w", agentID, seq, err)
	}
	return e, nil
}

// Range 实现 store.MessageLog.Range（含端点、按 Seq 升序）。
// 失败：from > to 或 from == 0 → ErrInvalid（fail fast，见接口契约）。
func (s *SQLiteStore) Range(ctx context.Context, agentID types.AgentID, from, to int64) ([]*types.LogEntry, error) {
	if agentID == "" {
		return nil, fmt.Errorf("log range: %w: empty agentID", ErrInvalid)
	}
	if from == 0 || from > to {
		return nil, fmt.Errorf("log range: %w: [%d, %d]", ErrInvalid, from, to)
	}
	list, err := s.scanList(ctx,
		`SELECT `+logCols+` FROM log_entries WHERE agent_id = ? AND seq BETWEEN ? AND ?
		 ORDER BY seq ASC`,
		string(agentID), from, to)
	if err != nil {
		return nil, fmt.Errorf("log range %s [%d,%d]: %w", agentID, from, to, err)
	}
	return list, nil
}

// Latest 实现 store.MessageLog.Latest（倒序取 limit 条后升序还给调用方）。
// 失败：limit < 0 → ErrInvalid；limit == 0 → 空切片。
func (s *SQLiteStore) Latest(ctx context.Context, agentID types.AgentID, limit int) ([]*types.LogEntry, error) {
	if agentID == "" {
		return nil, fmt.Errorf("log latest: %w: empty agentID", ErrInvalid)
	}
	if limit < 0 {
		return nil, fmt.Errorf("log latest: %w: negative limit", ErrInvalid)
	}
	if limit == 0 {
		return []*types.LogEntry{}, nil
	}
	list, err := s.scanList(ctx,
		// 子查询内先倒序取最近 N 条，外层按 seq 升序返回（MessageLog.Latest
		// 就被这个排序），子查询不需要 ORDER BY 満足语义——外层排序定了次序。
		`SELECT `+logCols+` FROM (
			SELECT `+logCols+` FROM log_entries
			WHERE agent_id = ? ORDER BY seq DESC LIMIT ?
		) ORDER BY seq ASC`,
		string(agentID), limit)
	if err != nil {
		return nil, fmt.Errorf("log latest %s %d: %w", agentID, limit, err)
	}
	return list, nil
}

// LastSeq 实现 store.MessageLog.LastSeq。
// 结果：无记录时返回 0（哨兵，"尚无记录"──Append 从 1 起分配）。
func (s *SQLiteStore) LastSeq(ctx context.Context, agentID types.AgentID) (int64, error) {
	if agentID == "" {
		return 0, fmt.Errorf("log lastSeq: %w: empty agentID", ErrInvalid)
	}
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM log_entries WHERE agent_id = ?`, string(agentID)).Scan(&seq); err != nil {
		return 0, fmt.Errorf("log lastSeq %s: %w", agentID, err)
	}
	// 无行时 MAX 返回 NULL → seq.Int64 == 0，恰是"尚无记录"的哨兵口径，
	// 与 Append 从 1 起分配的约定互补（去重两个分支的必要性为零）。
	return seq.Int64, nil
}

// TotalTokens 实现 store.MessageLog.TotalTokens（TokenEst 的累计，Watchdog 预算口径）。
func (s *SQLiteStore) TotalTokens(ctx context.Context, agentID types.AgentID) (int64, error) {
	if agentID == "" {
		return 0, fmt.Errorf("log totalTokens: %w: empty agentID", ErrInvalid)
	}
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(token_est) FROM log_entries WHERE agent_id = ?`, string(agentID)).Scan(&total); err != nil {
		return 0, fmt.Errorf("log totalTokens %s: %w", agentID, err)
	}
	return total.Int64, nil
}

// ---------------------------------------------------------------------------
// 行扫描与翻译
// ---------------------------------------------------------------------------

// scanOne 取单行；无行 → ErrNotFound。
func (s *SQLiteStore) scanOne(ctx context.Context, query string, args ...any) (*types.LogEntry, error) {
	if s.closed() {
		return nil, fmt.Errorf("log get: %w", ErrClosed)
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	e, err := decodeLogRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// scanList 取多行；无行本身是正常状态（返回 nil 切片 + nil error）。
func (s *SQLiteStore) scanList(ctx context.Context, query string, args ...any) ([]*types.LogEntry, error) {
	if s.closed() {
		return nil, fmt.Errorf("log list: %w", ErrClosed)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*types.LogEntry
	for rows.Next() {
		e, rowErr := decodeLogRow(rows.Scan)
		if rowErr != nil {
			return nil, rowErr
		}
		out = append(out, e)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	return out, nil
}

// decodeLogRow 把一行 log_entries 翻译回 *LogEntry（与 logCols 列序绑定）。
func decodeLogRow(scan func(dest ...any) error) (*types.LogEntry, error) {
	var (
		e          types.LogEntry
		srcJSON    string
		metaJSON   sql.NullString
		actualJSON sql.NullString
		createdMS  int64
	)
	if err := scan(&e.ID, &e.AgentID, &e.Seq, &e.Role, &e.Content, &e.Prov,
		&srcJSON, &metaJSON, &e.Audience, &e.TokenEst, &actualJSON, &createdMS); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(srcJSON), &e.SourceIDs); err != nil {
		return nil, fmt.Errorf("decode source_ids %s: %w", e.ID, err)
	}
	if metaJSON.Valid {
		e.Meta = map[string]any{}
		if err := json.Unmarshal([]byte(metaJSON.String), &e.Meta); err != nil {
			return nil, fmt.Errorf("decode meta %s: %w", e.ID, err)
		}
	}
	if actualJSON.Valid {
		ta := new(types.TokenUsage)
		if err := json.Unmarshal([]byte(actualJSON.String), ta); err != nil {
			return nil, fmt.Errorf("decode token usage %s: %w", e.ID, err)
		}
		e.TokenActual = ta
	}
	// SQLite 只存毫秒精度；CreatedAt 只落库不渲染（Part 3.2），足够。
	e.CreatedAt = time.UnixMilli(createdMS).UTC()
	return &e, nil
}
