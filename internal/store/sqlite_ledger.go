package store

// SQLiteStore 对 Ledger 与 AuditStore 的实现（13.6 阶段 4）。
//
// 两张表（schema 见 sqlite.go 的 sqliteSchema）：
//
//	ledger_entries —— 每次调用的账（task/rung/call_type 三个查询维度）
//	model_switch   —— 换模型的缓存失效审计（Part 7.7）
//	audit_events   —— 审计事件（原则 1 的副产品，开放集合）

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/RobiNexy/Marl/internal/types"
)

// ledgerRecord 实现 store.Ledger.Record（RecordOrchestration/RecordDiscussion
// 是它的强制类别包装）。
func (s *SQLiteStore) Record(ctx context.Context, entry *LedgerEntry) error {
	if s.closed() {
		return fmt.Errorf("ledger record: %w", ErrClosed)
	}
	if entry == nil {
		return fmt.Errorf("ledger record: %w: nil entry", ErrInvalid)
	}
	switch {
	case entry.TaskID == "":
		return fmt.Errorf("ledger record: %w: empty task id", ErrInvalid)
	case entry.AgentID == "":
		return fmt.Errorf("ledger record: %w: empty agent id", ErrInvalid)
	case entry.Rung == "":
		return fmt.Errorf("ledger record: %w: empty rung", ErrInvalid)
	case entry.Currency == "":
		return fmt.Errorf("ledger record: %w: empty currency (cost without currency is uninterpretable)", ErrInvalid)
	case !entry.CallType.Valid():
		return fmt.Errorf("ledger record: %w: call type %q invalid (zero value would vanish from all report columns)", ErrInvalid, entry.CallType)
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC() // 所有权契约：就地回填
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ledger_entries
			(task_id, agent_id, rung, call_type, prompt_tokens, completion_tokens,
			 reasoning_tokens, cache_write, cache_read, image_tokens, cost, currency, ts)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(entry.TaskID), string(entry.AgentID), string(entry.Rung), string(entry.CallType),
		entry.TokenUsage.PromptTokens, entry.TokenUsage.CompletionTokens,
		entry.TokenUsage.ReasoningTokens, entry.TokenUsage.CacheWriteTokens,
		entry.TokenUsage.CacheReadTokens, entry.TokenUsage.ImageTokens,
		entry.Cost, entry.Currency, entry.Timestamp.UnixMilli())
	if err != nil {
		return fmt.Errorf("ledger record: insert: %w", err)
	}
	return nil
}

// RecordOrchestration 实现 store.Ledger.RecordOrchestration（强制类别覆盖）。
func (s *SQLiteStore) RecordOrchestration(ctx context.Context, entry *LedgerEntry) error {
	if entry != nil {
		entry.CallType = CallOrchestration // 覆盖是契约，不是副作用
	}
	return s.Record(ctx, entry)
}

// RecordDiscussion 实现 store.Ledger.RecordDiscussion（强制类别覆盖）。
func (s *SQLiteStore) RecordDiscussion(ctx context.Context, entry *LedgerEntry) error {
	if entry != nil {
		entry.CallType = CallDiscussion
	}
	return s.Record(ctx, entry)
}

// TaskSummary 实现 store.Ledger.TaskSummary。
//
// 聚合两个维度（ByLevel / ByCategory），并保证 TotalTokens/TotalCost 等于
// 分项之和（TaskCostSummary 的自证不变量）。
//
// 状态语义（ADR-0027）：账本只见账目、不见任务终态（没有任务表），因此
// Status 恒为 TaskRunning——报表的"状态"行在任务管理落地前由调用方覆盖。
// Duration 用账目时间跨度（首条到末条），它低估真实时长（只有发生调用的
// 时间段被计入），报表注明口径。
//
// 失败：taskID 为空 → ErrInvalid；无记录 → ErrNotFound（"没有记录"与
// "花了 0 元"是不同的结论，见接口契约）。
func (s *SQLiteStore) TaskSummary(ctx context.Context, taskID types.TaskID) (*TaskCostSummary, error) {
	if s.closed() {
		return nil, fmt.Errorf("ledger summary: %w", ErrClosed)
	}
	if taskID == "" {
		return nil, fmt.Errorf("ledger summary: %w: empty task id", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rung, call_type, prompt_tokens, completion_tokens, reasoning_tokens,
		        cache_write, cache_read, image_tokens, cost, currency, ts
		 FROM ledger_entries WHERE task_id = ? ORDER BY ts ASC`, string(taskID))
	if err != nil {
		return nil, fmt.Errorf("ledger summary: query: %w", err)
	}
	defer rows.Close()

	sum := &TaskCostSummary{
		TaskID:     taskID,
		Status:     types.TaskRunning, // 见函数注释（ADR-0027）
		ByLevel:    map[types.RungID]*LevelSummary{},
		ByCategory: map[CostCategory]*LevelSummary{},
	}
	var currency string
	var firstTS, lastTS int64
	count := 0
	for rows.Next() {
		var rung, callType string
		var u TokenUsageRow
		var cost float64
		var cur string
		var ts int64
		if err := rows.Scan(&rung, &callType, &u.PromptTokens, &u.CompletionTokens, &u.ReasoningTokens,
			&u.CacheWriteTokens, &u.CacheReadTokens, &u.ImageTokens, &cost, &cur, &ts); err != nil {
			return nil, fmt.Errorf("ledger summary: scan: %w", err)
		}
		count++
		if currency == "" {
			currency = cur
		}
		if firstTS == 0 || ts < firstTS {
			firstTS = ts
		}
		if ts > lastTS {
			lastTS = ts
		}
		tokens := int64(u.PromptTokens + u.CompletionTokens)
		// 按阶梯分项。
		lv := sum.ByLevel[types.RungID(rung)]
		if lv == nil {
			lv = &LevelSummary{}
			sum.ByLevel[types.RungID(rung)] = lv
		}
		accLevel(lv, u, tokens, cost)
		// 按类别分项。
		cv := sum.ByCategory[CostCategory(callType)]
		if cv == nil {
			cv = &LevelSummary{}
			sum.ByCategory[CostCategory(callType)] = cv
		}
		accLevel(cv, u, tokens, cost)
		sum.TotalTokens += tokens
		sum.TotalCost += cost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger summary: rows: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("ledger summary: %w: task %q has no entries", ErrNotFound, taskID)
	}
	sum.Currency = currency
	if lastTS >= firstTS {
		sum.Duration = time.Duration(lastTS-firstTS) * time.Millisecond
	}
	return sum, nil
}

// TokenUsageRow 是聚合扫描用的裸计数行（避免与 types.TokenUsage 混淆）。
type TokenUsageRow = struct {
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	CacheWriteTokens int
	CacheReadTokens  int
	ImageTokens      int
}

func accLevel(lv *LevelSummary, u TokenUsageRow, tokens int64, cost float64) {
	lv.Calls++
	lv.Tokens += tokens
	lv.ReasoningTokens += int64(u.ReasoningTokens)
	lv.CacheRead += int64(u.CacheReadTokens)
	lv.CacheWrite += int64(u.CacheWriteTokens)
	lv.Cost += cost
}

// RecordModelSwitch 实现 store.Ledger.RecordModelSwitch（Part 7.7：审计、不硬拦截）。
//
// 调用纪律在接口契约里：只有换 model_id 或 adapter 才调用本方法。
func (s *SQLiteStore) RecordModelSwitch(ctx context.Context, ev *ModelSwitchEvent) error {
	if s.closed() {
		return fmt.Errorf("model switch: %w", ErrClosed)
	}
	if ev == nil {
		return fmt.Errorf("model switch: %w: nil event", ErrInvalid)
	}
	if ev.AgentID == "" || ev.FromModel == "" || ev.ToModel == "" {
		return fmt.Errorf("model switch: %w: agent/from/to are required", ErrInvalid)
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_switch
			(agent_id, task_id, from_model, to_model, reason, cache_hits_before, cache_writes_before, at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		string(ev.AgentID), string(ev.TaskID), ev.FromModel, ev.ToModel, ev.Reason,
		ev.CacheHitsBefore, ev.CacheWritesBefore, ev.At.UnixMilli())
	if err != nil {
		return fmt.Errorf("model switch: insert: %w", err)
	}
	return nil
}

// QueryModelSwitch 读取换模型审计（报表的"升级记录"输入；store.Ledger
// 接口之外的只读查询，属于本实现的附加能力）。
func (s *SQLiteStore) QueryModelSwitch(ctx context.Context, taskID types.TaskID) ([]*ModelSwitchEvent, error) {
	if s.closed() {
		return nil, fmt.Errorf("model switch query: %w", ErrClosed)
	}
	q := `SELECT agent_id, task_id, from_model, to_model, reason, cache_hits_before, cache_writes_before, at
	      FROM model_switch`
	args := []any{}
	if taskID != "" {
		q += ` WHERE task_id = ?`
		args = append(args, string(taskID))
	}
	q += ` ORDER BY at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("model switch query: %w", err)
	}
	defer rows.Close()
	var out []*ModelSwitchEvent
	for rows.Next() {
		ev := &ModelSwitchEvent{}
		var agentID, task string
		var at int64
		if err := rows.Scan(&agentID, &task, &ev.FromModel, &ev.ToModel, &ev.Reason,
			&ev.CacheHitsBefore, &ev.CacheWritesBefore, &at); err != nil {
			return nil, fmt.Errorf("model switch query: scan: %w", err)
		}
		ev.AgentID = types.AgentID(agentID)
		ev.TaskID = types.TaskID(task)
		ev.At = time.UnixMilli(at).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// AuditStore
// ---------------------------------------------------------------------------

// AuditSQLite 让 SQLiteStore 满足 AuditStore。
//
// 之所以是薄包装而不是让 SQLiteStore 直接实现：MessageLog.Append 与
// AuditStore.Append 同名不同义（前者追加真相、后者追加旁路证据），
// Go 的方法集不允许同名共存；包装层把两个语义分流到两个类型上。
type AuditSQLite struct{ *SQLiteStore }

// 编译期断言：包装类型满足 AuditStore。
var _ AuditStore = AuditSQLite{}

// Append 实现 store.AuditStore.Append（审计写入不因业务失败回滚——见接口契约）。
func (a AuditSQLite) Append(ctx context.Context, ev *AuditEvent) error {
	return a.SQLiteStore.appendAudit(ctx, ev)
}

// Query 实现 store.AuditStore.Query（按 Seq 升序）。
func (a AuditSQLite) Query(ctx context.Context, filter AuditFilter) ([]*AuditEvent, error) {
	return a.SQLiteStore.queryAudit(ctx, filter)
}

// appendAudit 是审计追加的实现（由 AuditSQLite 包装导出）。
func (s *SQLiteStore) appendAudit(ctx context.Context, ev *AuditEvent) error {
	if s.closed() {
		return fmt.Errorf("audit append: %w", ErrClosed)
	}
	if ev == nil {
		return fmt.Errorf("audit append: %w: nil event", ErrInvalid)
	}
	if ev.AgentID == "" || ev.Action == "" {
		return fmt.Errorf("audit append: %w: agent id and action are required", ErrInvalid)
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	var payload []byte
	if ev.Payload != nil {
		b, err := json.Marshal(ev.Payload)
		if err != nil {
			// 审计的 payload 必须可序列化（OpResult.AuditPayload 契约）。
			// 序列化失败在 Append 时暴露比落盘时暴露好——报错而不是记一条
			// 空载荷的"哑事件"。
			return fmt.Errorf("audit append: marshal payload: %w", err)
		}
		payload = b
	}
	// Seq 全局递增（单表自增）。
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_events (agent_id, action, target, payload, ts) VALUES (?,?,?,?,?)`,
		string(ev.AgentID), ev.Action, ev.Target, string(payload), ev.Timestamp.UnixMilli())
	if err != nil {
		return fmt.Errorf("audit append: insert: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		ev.Seq = id
	}
	return nil
}

// queryAudit 是审计查询的实现（由 AuditSQLite 包装导出）。
func (s *SQLiteStore) queryAudit(ctx context.Context, filter AuditFilter) ([]*AuditEvent, error) {
	if s.closed() {
		return nil, fmt.Errorf("audit query: %w", ErrClosed)
	}
	if filter.Limit < 0 {
		return nil, fmt.Errorf("audit query: %w: negative limit", ErrInvalid)
	}
	q := `SELECT id, agent_id, action, target, payload, ts FROM audit_events WHERE 1=1`
	args := []any{}
	if filter.AgentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, string(filter.AgentID))
	}
	if filter.AfterSeq > 0 {
		q += ` AND id > ?`
		args = append(args, filter.AfterSeq)
	}
	if filter.Action != "" {
		q += ` AND action = ?`
		args = append(args, filter.Action)
	}
	if !filter.FromTime.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, filter.FromTime.UnixMilli())
	}
	if !filter.ToTime.IsZero() {
		q += ` AND ts <= ?`
		args = append(args, filter.ToTime.UnixMilli())
	}
	q += ` ORDER BY id ASC`
	limit := filter.Limit
	if limit == 0 {
		limit = 1000 // 实现默认上限（契约：0 = 用默认，不是"不限"）
	}
	q += fmt.Sprintf(` LIMIT %d`, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("audit query: %w", err)
	}
	defer rows.Close()
	var out []*AuditEvent
	for rows.Next() {
		var agentID, action, target string
		var payload []byte
		var ts int64
		ev := &AuditEvent{}
		if err := rows.Scan(&ev.Seq, &agentID, &action, &target, &payload, &ts); err != nil {
			return nil, fmt.Errorf("audit query: scan: %w", err)
		}
		ev.AgentID = types.AgentID(agentID)
		ev.Action = action
		ev.Target = target
		ev.Timestamp = time.UnixMilli(ts).UTC()
		if len(payload) > 0 {
			var m map[string]any
			if err := json.Unmarshal(payload, &m); err == nil {
				ev.Payload = m
			} else {
				ev.Payload = string(payload) // 非对象载荷按原文返回
			}
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
