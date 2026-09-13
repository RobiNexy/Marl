package server

// 对话导出（Part 1.4 的 markdown 形态；GUI 的"导出"按钮与 CLI 的
// marl log 共用同一实现——单一渲染点）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// RenderConversationMarkdown 把全部（或指定）Agent 的对话渲染成
// conversation markdown（Log 条目按 Agent 分节；节内按 seq 升序）。
//
// limit 是每 Agent 的最多导出条数（旧条目被略过——真相之源在 Log，
// 导出是投影）。agentID 为空 = 全部 Agent（从审计还原名单）。
func RenderConversationMarkdown(ctx context.Context, st *store.SQLiteStore, agentID types.AgentID, limit int) (string, error) {
	as := store.AuditSQLite{SQLiteStore: st}
	lg := store.MessageLog(st)
	if limit <= 0 {
		limit = 200
	}
	var agentIDs []types.AgentID
	if agentID != "" {
		agentIDs = []types.AgentID{agentID}
	}
	if agentIDs == nil {
		evs, err := as.Query(ctx, store.AuditFilter{Limit: 1000})
		if err != nil {
			return "", err
		}
		for _, ev := range evs {
			if ev.AgentID != "watchdog" {
				agentIDs = append(agentIDs, ev.AgentID)
			}
		}
	}
	var sb strings.Builder
	seen := false
	for _, id := range uniqueAgentsOf(agentIDs) {
		if seen {
			sb.WriteString("\n")
		}
		seen = true
		fmt.Fprintf(&sb, "### Agent %s\n", id)
		lastSeq, err := lg.LastSeq(ctx, id)
		if err != nil {
			continue
		}
		from := int64(1)
		if lastSeq > int64(limit) {
			from = lastSeq - int64(limit) + 1
		}
		entries, err := lg.Range(ctx, id, from, lastSeq)
		if err != nil {
			continue
		}
		for _, e := range entries {
			fmt.Fprintf(&sb, "## %s  %s\n%s\n", e.CreatedAt.Format("15:04:05"), roleDisplayName(e.Role), clipBody(e.Content))
		}
	}
	return sb.String(), nil
}

// uniqueAgentsOf 去重保序（导出的分节顺序稳定）。
func uniqueAgentsOf(ids []types.AgentID) []types.AgentID {
	seen := map[types.AgentID]bool{}
	out := []types.AgentID{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// roleDisplayName 是角色的呈现名（导出可读性）。
func roleDisplayName(r types.InternalRole) string {
	switch r {
	case types.RoleUserInput:
		return "Human"
	case types.RoleAssistantReply:
		return "Assistant"
	case types.RoleToolResult:
		return "Tool"
	case types.RoleHumanNote:
		return "Human"
	case types.RoleEscalation:
		return "Escalation"
	case types.RoleSubTaskResult:
		return "SubTask"
	default:
		return string(r)
	}
}

// clipBody 截断超长内容（导出的可读性面；日志里的行内换行保留）。
func clipBody(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 12 {
		return strings.Join(lines[:12], "\n") + "\n… (truncated)"
	}
	return s
}
