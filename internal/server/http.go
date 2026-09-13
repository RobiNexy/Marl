package server

// HTTP 传输层：REST JSON + SSE 事件流（阶段 14：GUI 的接入口）。
//
// 薄层纪律：handler 只做 解码 → 调 App 方法 → 编码/状态码；不持业务，
// 不持状态（状态全在 App）。事件流是 audit_events 的轮询拉取（游标
// since），不另建 pub/sub——GUI 看到的与 CLI/status 同源同序。
//
// 安全面：默认只绑 127.0.0.1（GUI 是本机应用）；可选 token（Bearer）。
// 路径参数经 safeInboxName 守卫（穿越拒绝）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/contract"
)

// Handler 返回服务全部路由的 HTTP handler（GUI 的单一接入口）。
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": a.Version})
	})

	// --- 观测 ---
	mux.HandleFunc("GET /api/v1/status", a.h(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{
			"version": a.Version,
			"root":    a.Root,
			"human":   string(a.human),
			"run":     a.CurrentRun(),
			"agents":  a.Agents(),
		})
	}))
	mux.HandleFunc("GET /api/v1/agents", a.h(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"agents": a.Agents()})
	}))
	mux.HandleFunc("GET /api/v1/events", a.hEvents)
	mux.HandleFunc("GET /api/v1/events/since/{seq}", a.h(func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.PathValue("seq"), 10, 64)
		evs, err := a.Events(r.Context(), since, 500)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"events": evs})
	}))
	mux.HandleFunc("POST /api/v1/shutdown", a.h(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"shutting_down": true})
		if a.onShutdown != nil {
			go a.onShutdown()
		}
	}))
	mux.HandleFunc("GET /api/v1/conversation", a.h(func(w http.ResponseWriter, r *http.Request) {
		entries, err := a.Conversation(r.Context(), r.URL.Query().Get("agent"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"entries": entries})
	}))
	mux.HandleFunc("GET /api/v1/conversation/export", a.h(func(w http.ResponseWriter, r *http.Request) {
		md, err := a.ConversationMarkdown(r.Context(), r.URL.Query().Get("agent"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(md))
	}))
	mux.HandleFunc("GET /api/v1/costs", a.h(func(w http.ResponseWriter, r *http.Request) {
		s, err := a.Costs(r.Context(), r.URL.Query().Get("task"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, s)
	}))

	// --- 任务生命周期 ---
	mux.HandleFunc("POST /api/v1/tasks", a.h(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Task string `json:"task"`
		}
		if !readBody(w, r, &body) {
			return
		}
		id, err := a.StartTask(body.Task)
		if err == ErrAlreadyRunning {
			writeJSON(w, 409, map[string]any{"error": "a task is already running"})
			return
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 201, map[string]any{"agent": string(id)})
	}))
	mux.HandleFunc("POST /api/v1/tasks/stop", a.h(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Force bool `json:"force"`
		}
		_ = readBody(w, r, &body)
		if err := a.StopTask(body.Force); err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]any{"stopped": true})
	}))

	// --- 交互 ---
	mux.HandleFunc("POST /api/v1/agents/{id}/messages", a.h(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if !readBody(w, r, &body) {
			return
		}
		if err := a.SendMessage(r.PathValue("id"), body.Text); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 201, map[string]any{"delivered": true})
	}))
	mux.HandleFunc("GET /api/v1/inbox", a.h(func(w http.ResponseWriter, _ *http.Request) {
		items, err := a.Inbox()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
	}))
	mux.HandleFunc("GET /api/v1/inbox/{name}", a.h(func(w http.ResponseWriter, r *http.Request) {
		data, err := a.ReadInbox(r.PathValue("name"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write(data)
	}))
	mux.HandleFunc("POST /api/v1/inbox/gate/{id}", a.h(func(w http.ResponseWriter, r *http.Request) {
		var body contract.GateDecision
		if !readBody(w, r, &body) {
			return
		}
		if err := a.ReplyGate(r.PathValue("id"), body); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"decided": true})
	}))
	mux.HandleFunc("GET /api/v1/discussions", a.h(func(w http.ResponseWriter, _ *http.Request) {
		ds, err := a.Discussions()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"discussions": ds})
	}))
	mux.HandleFunc("GET /api/v1/discussions/{id}/verdict", a.h(func(w http.ResponseWriter, r *http.Request) {
		verdict, _, err := a.lf.DiscussionFile(r.PathValue("id"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		http.ServeFile(w, r, verdict)
	}))
	mux.HandleFunc("POST /api/v1/discussions/{id}/verdict", a.h(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Annotation string `json:"annotation"`
			Approve    bool   `json:"approve"`
		}
		if !readBody(w, r, &body) {
			return
		}
		if err := a.ReplyDiscussion(r.PathValue("id"), body.Annotation, body.Approve); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"replied": true})
	}))

	// --- 配置与知识 ---
	mux.HandleFunc("GET /api/v1/config", a.h(func(w http.ResponseWriter, _ *http.Request) {
		raw, err := a.ConfigRaw()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write(raw)
	}))
	mux.HandleFunc("PUT /api/v1/config", a.h(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := a.WriteConfig(raw); err != nil {
			writeErr(w, 422, err)
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true})
	}))
	mux.HandleFunc("GET /api/v1/profiles", a.h(func(w http.ResponseWriter, _ *http.Request) {
		ps, err := a.Profiles()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"profiles": ps})
	}))
	mux.HandleFunc("GET /api/v1/profiles/{id}", a.h(func(w http.ResponseWriter, r *http.Request) {
		raw, err := a.ProfileRaw(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write(raw)
	}))
	mux.HandleFunc("PUT /api/v1/profiles/{id}", a.h(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := a.WriteProfile(r.PathValue("id"), raw); err != nil {
			writeErr(w, 422, err)
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true})
	}))
	mux.HandleFunc("GET /api/v1/knowledge/lint", a.h(func(w http.ResponseWriter, _ *http.Request) {
		rep, err := a.KnowledgeLint()
		if err != nil {
			writeErr(w, 422, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(rep))
	}))
	mux.HandleFunc("POST /api/v1/knowledge/promote", a.h(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path   string `json:"path"`
			Global string `json:"global"`
		}
		body.Global = r.URL.Query().Get("global")
		if !readBody(w, r, &body) {
			return
		}
		hash, err := a.KnowledgePromote(r.Context(), body.Path, body.Global)
		if err != nil {
			writeErr(w, 422, err)
			return
		}
		writeJSON(w, 200, map[string]any{"commit": hash})
	}))
	mux.HandleFunc("POST /api/v1/knowledge/pull", a.h(func(w http.ResponseWriter, r *http.Request) {
		paths, err := a.KnowledgePull(r.Context(), r.URL.Query().Get("global"))
		if err != nil {
			writeErr(w, 422, err)
			return
		}
		writeJSON(w, 200, map[string]any{"paths": paths})
	}))

	// --- 诊断 ---
	mux.HandleFunc("GET /api/v1/doctor", a.h(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"checks": a.Doctor()})
	}))

	return withCORS(a.withAuth(mux))
}

// h 是 handler 的统一包装（panic → 500；后续 token 鉴权的挂点）。
func (a *App) h(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeErr(w, 500, fmt.Errorf("internal error: %v", rec))
			}
		}()
		fn(w, r)
	}
}

// withAuth 是可选的 Bearer token 校验（token 空 = 关闭——本机模式）。
func (a *App) withAuth(next http.Handler) http.Handler {
	token := os.Getenv("MARL_API_TOKEN")
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withCORS 是本机 GUI 的宽松跨域（页面框架与 API 不同源是常态）。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hEvents 是 SSE 事件流（GET /api/v1/events?since=<seq>）：
// 轮询 audit（游标推进）→ `data: <json>` 帧；15s 心跳注释防中间层断连。
// 退出条件：请求 ctx 结束（客户端断开）。
func (a *App) hEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl.Flush()

	tick := time.NewTicker(700 * time.Millisecond)
	defer tick.Stop()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-tick.C:
			evs, err := a.Events(r.Context(), since, 200)
			if err != nil {
				fmt.Fprintf(w, ": poll error %v\n\n", err)
				fl.Flush()
				continue
			}
			for _, ev := range evs {
				b, jerr := json.Marshal(ev)
				if jerr != nil {
					continue
				}
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
				since = ev.Seq
			}
			if len(evs) > 0 {
				fl.Flush()
			}
		}
	}
}

// ---- 编解码小件 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	msg := strings.TrimSpace(err.Error())
	writeJSON(w, code, map[string]any{"error": msg})
}

// readBody 解码 JSON 体（空体 = 零值结构——GET 语义的 POST 化也顺）。
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		return true
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, err)
		return false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return true
	}
	if err := json.Unmarshal(data, v); err != nil {
		writeErr(w, 400, fmt.Errorf("invalid JSON body: %w", err))
		return false
	}
	return true
}
