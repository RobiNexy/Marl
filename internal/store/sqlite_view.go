package store

// SQLite 版 ViewStore 实现（13.4 阶段 2 的 sqlite_view.go）。
//
// 投影纪律（store/doc.go）：View 的写入失败**不需要**恢复流程，
// 这里拒绝做任何"感觉像 Log"的机制（无版本链、无迁移日志）。
// 载体是一个 JSON blob（views.data 列）：ViewItem 的数组天然序列化，
// 增量字段（EstimatedTokens 等）随 blob 一起走，无需逐列建表——
// View 的形态在编排阶段（13.5）还会大改，逐列建表是过早承重。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"marl/internal/types"
)

// LoadView 读回某 Agent 最近保存的 View；无记录时返回**带 AgentID 的空 View**
// （ViewStore 契约：空 ≠ 零值，调用方拿到即可直接使用）。
func (s *SQLiteStore) LoadView(ctx context.Context, agentID types.AgentID) (*types.ContextView, error) {
	if s.closed() {
		return nil, fmt.Errorf("view load: %w", ErrClosed)
	}
	if agentID == "" {
		return nil, fmt.Errorf("view load: %w: empty agentID", ErrInvalid)
	}
	var data string
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM views WHERE agent_id = ?`, string(agentID)).Scan(&data)
	switch {
	case err == nil:
		view := new(types.ContextView)
		if err := json.Unmarshal([]byte(data), view); err != nil {
			return nil, fmt.Errorf("view load %s: decode: %w", agentID, err)
		}
		if view.AgentID != agentID {
			return nil, fmt.Errorf("view load %s: decoded agent_id %q: blob content mismatch (corrupt store)",
				agentID, view.AgentID)
		}
		return view, nil
	case errors.Is(err, sql.ErrNoRows):
		return &types.ContextView{AgentID: agentID}, nil
	default:
		return nil, fmt.Errorf("view load %s: %w", agentID, err)
	}
}

// SaveView 全量覆盖式落盘（ViewStore 契约：覆盖而非合并；合并属上层）。
func (s *SQLiteStore) SaveView(ctx context.Context, view *types.ContextView) error {
	if s.closed() {
		return fmt.Errorf("view save: %w", ErrClosed)
	}
	if view == nil || view.AgentID == "" {
		return fmt.Errorf("view save: %w: nil view or empty agentID", ErrInvalid)
	}
	blob, err := json.Marshal(view)
	if err != nil {
		return fmt.Errorf("view save %s: encode: %w", view.AgentID, err)
	}
	// UPSERT：同一 agent 只留最新（views.agent_id 主键承载"上次保存"语义）。
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO views (agent_id, data, saved_at) VALUES (?, ?, ?)
		 ON CONFLICT(agent_id) DO UPDATE SET data = excluded.data, saved_at = excluded.saved_at`,
		string(view.AgentID), string(blob), time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("view save %s: upsert: %w", view.AgentID, err)
	}
	return nil
}
