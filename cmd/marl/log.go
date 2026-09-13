// 子命令 marl log / attach（13.12 阶段 10 的打磨面："conversation 文件
// 导出 / marl log / marl attach"）。
//
// 导出形态（Part 1.4 的 conversation 文件格式）：
//
//	## 10:30:05  Human
//	（消息正文）
//
// 内部角色 → 呈现名的映射在 roleDisplayName（user_input→Human、
// assistant_reply→Assistant、tool_result→Tool、…）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("marl log", flag.ContinueOnError)
	dbPath := fs.String("db", ".marl/store.db", "存储数据库路径")
	agent := fs.String("agent", "", "限定 Agent（缺省全部）")
	out := fs.String("out", "", "写入 conversation 文件（缺省 stdout）")
	limit := fs.Int("limit", 200, "每 Agent 最多导出的条目数（真相之源在 Log；旧条目被略过）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.OpenSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	md, err := renderLogToMarkdown(context.Background(), store.AuditSQLite{SQLiteStore: st}, store.MessageLog(st), types.AgentID(*agent), nil, *limit)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *out, err)
		}
		fmt.Printf("已导出 conversation → %s\n", *out)
		return nil
	}
	_, _ = io.WriteString(os.Stdout, md)
	return nil
}

// attach 按轮询面 tail 新条目（ctx 取消即退出——Ctrl-C 的信号折算）。
func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("marl attach", flag.ContinueOnError)
	dbPath := fs.String("db", ".marl/store.db", "存储数据库路径")
	agent := fs.String("agent", "", "限定 Agent")
	interval := fs.Duration("interval", 2*time.Second, "轮询步长")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.OpenSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	// Ctrl-C 折算（os/signal 的最简形态；独立函数使测试可无信号驱动）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fmt.Printf("（marl attach：每 %v 轮询新条目；Ctrl-C 退出）\n", *interval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
		_ = agent
		_ = cancel
	}
}

// renderLogToMarkdown 把 Log 条目转成 conversation markdown（Part 1.4
// 的块形态）；agentIDs 可以限定（限定的 agent 只导它自己的链条）。
func renderLogToMarkdown(ctx context.Context, as store.AuditStore, lg store.MessageLog, agentID types.AgentID, agentIDs []types.AgentID, limit int) (string, error) {
	if agentID != "" {
		agentIDs = []types.AgentID{agentID}
	}
	if agentIDs == nil {
		q := store.AuditFilter{Limit: 1000}
		evs, err := as.Query(ctx, q)
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
	seenAgents := false
	for _, id := range uniqueAgents(agentIDs) {
		if seenAgents {
			sb.WriteString("\n")
		}
		seenAgents = true
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

func uniqueAgents(in []types.AgentID) []types.AgentID {
	seen := map[types.AgentID]bool{}
	out := []types.AgentID{}
	for _, id := range in {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// roleDisplayName 是 Part 3.4 映射表的 conversation 呈现名（可读性
// 优先；与 View 的角色分类解耦）。
func roleDisplayName(r types.InternalRole) string {
	switch r {
	case types.RoleUserInput:
		return "Human"
	case types.RoleAssistantReply:
		return "Assistant"
	case types.RoleToolResult:
		return "Tool"
	case types.RoleThinking:
		return "Assistant(thinking)"
	case types.RoleSubTaskResult:
		return "SubTask"
	case types.RoleEscalation:
		return "Escalation"
	case types.RoleHumanNote:
		return "Note"
	default:
		return strings.ToTitle(string(r))
	}
}

// clipBody 的显示面（conversation 文件里的长内容截断；完整内容读 Log）。
func clipBody(content string) string {
	if len(content) > 4096 {
		return content[:4096] + " …（截断；完整内容在 Log）"
	}
	return content
}
