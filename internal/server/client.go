package server

// HTTPClient：契约的 HTTP 实现（连 marl serve 的 REST/SSE）。
//
// 用面：daemon 在跑时的一切外部宿主（CLI / TUI / IDE 插件——Go 语言
// 形态的；GUI 前端直接说 HTTP，不需要本包）。与 App 的契约一致性由
// 场景对拍测试守护（app_test.go 的同一场景驱动两个实现断言同结果）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/store"
	"github.com/RobiNexy/Marl/internal/types"
)

// HTTPClient 是契约的 HTTP 实现。
type HTTPClient struct {
	base string // 如 http://127.0.0.1:8731
	hc   *http.Client
	key  string // 可选 Bearer（MARL_API_TOKEN 的客户端面）
}

// NewHTTPClient 构造（addr 如 "127.0.0.1:8731"；超时 = 命令级 30s——
// Events/长操作用短轮询而非长请求）。
func NewHTTPClient(addr string) *HTTPClient {
	return &HTTPClient{
		base: "http://" + strings.TrimPrefix(addr, "http://"),
		hc:   &http.Client{Timeout: 30 * time.Second},
		key:  os.Getenv("MARL_API_TOKEN"),
	}
}

// do 是请求的统一通路（编码/鉴权/解码/错误提取）。
func (c *HTTPClient) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("http %d: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncateForErr(data))
	}
	if out == nil {
		return nil
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func truncateForErr(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ---- contract.Interaction 的 HTTP 面 ----

// StartTask 启动任务（409 → ErrAlreadyRunning）。
func (c *HTTPClient) StartTask(task string) (types.AgentID, error) {
	var out struct {
		Agent string `json:"agent"`
	}
	if err := c.do("POST", "/api/v1/tasks", map[string]string{"task": task}, &out); err != nil {
		return "", err
	}
	return types.AgentID(out.Agent), nil
}

// StopTask 停止当前任务。
func (c *HTTPClient) StopTask(force bool) error {
	return c.do("POST", "/api/v1/tasks/stop", map[string]bool{"force": force}, nil)
}

// CurrentRun 读运行态。
func (c *HTTPClient) CurrentRun() contract.RunStatus {
	var out struct {
		Run contract.RunStatus `json:"run"`
	}
	if err := c.do("GET", "/api/v1/status", nil, &out); err != nil {
		return contract.RunStatus{} // 服务不可达 = "未知"，不是"运行中"
	}
	return out.Run
}

// Agents 读监督树。
func (c *HTTPClient) Agents() []contract.AgentView {
	var out struct {
		Agents []contract.AgentView `json:"agents"`
	}
	if err := c.do("GET", "/api/v1/agents", nil, &out); err != nil {
		return []contract.AgentView{}
	}
	return out.Agents
}

// Events 游标拉事件（轮询面；SSE 的流式形态是 GUI 前端直连的）。
func (c *HTTPClient) Events(ctx context.Context, since int64, limit int) ([]*contract.Event, error) {
	var out struct {
		Events []*contract.Event `json:"events"`
	}
	if err := c.do("GET", fmt.Sprintf("/api/v1/events/since/%d", since), nil, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// Conversation 读对话。
func (c *HTTPClient) Conversation(ctx context.Context, agentID string) ([]*types.LogEntry, error) {
	var out struct {
		Entries []*types.LogEntry `json:"entries"`
	}
	if err := c.do("GET", "/api/v1/conversation?agent="+agentID, nil, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// ConversationMarkdown 导出。
func (c *HTTPClient) ConversationMarkdown(ctx context.Context, agentID string) (string, error) {
	req, err := http.NewRequest("GET", c.base+"/api/v1/conversation/export?agent="+agentID, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Costs 读账单。
func (c *HTTPClient) Costs(ctx context.Context, taskID string) (*store.TaskCostSummary, error) {
	var out store.TaskCostSummary
	if err := c.do("GET", "/api/v1/costs?task="+taskID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendMessage 插话（走 daemon 的进程内投递——say 命令的 daemon 形态）。
func (c *HTTPClient) SendMessage(to, text string) error {
	return c.do("POST", "/api/v1/agents/"+to+"/messages", map[string]string{"text": text}, nil)
}

// Inbox 列收件箱。
func (c *HTTPClient) Inbox() ([]contract.InboxItem, error) {
	var out struct {
		Items []contract.InboxItem `json:"items"`
	}
	if err := c.do("GET", "/api/v1/inbox", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ReadInbox 读收件文件。
func (c *HTTPClient) ReadInbox(name string) ([]byte, error) {
	resp, err := c.hc.Get(c.base + "/api/v1/inbox/" + name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<22))
}

// ReplyGate 回审批（daemon 写收件箱文件——与手编同一通道）。
func (c *HTTPClient) ReplyGate(id string, d contract.GateDecision) error {
	return c.do("POST", "/api/v1/inbox/gate/"+id, d, nil)
}

// Discussions 列讨论。
func (c *HTTPClient) Discussions() ([]contract.DiscussionView, error) {
	var out struct {
		Discussions []contract.DiscussionView `json:"discussions"`
	}
	if err := c.do("GET", "/api/v1/discussions", nil, &out); err != nil {
		return nil, err
	}
	return out.Discussions, nil
}

// ReplyDiscussion 回讨论。
func (c *HTTPClient) ReplyDiscussion(id, annotation string, approve bool) error {
	return c.do("POST", "/api/v1/discussions/"+id+"/verdict",
		map[string]any{"annotation": annotation, "approve": approve}, nil)
}

// ConfigRaw 读配置原文。
func (c *HTTPClient) ConfigRaw() ([]byte, error) {
	resp, err := c.hc.Get(c.base + "/api/v1/config")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// WriteConfig 写配置（服务端验证）。
func (c *HTTPClient) WriteConfig(raw []byte) error {
	req, err := http.NewRequest("PUT", c.base+"/api/v1/config", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/yaml")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncateForErr(data))
	}
	return nil
}

// Profiles 列角色。
func (c *HTTPClient) Profiles() ([]types.ProfileSummary, error) {
	var out struct {
		Profiles []types.ProfileSummary `json:"profiles"`
	}
	if err := c.do("GET", "/api/v1/profiles", nil, &out); err != nil {
		return nil, err
	}
	return out.Profiles, nil
}

// ProfileRaw 读角色原文。
func (c *HTTPClient) ProfileRaw(id string) ([]byte, error) {
	resp, err := c.hc.Get(c.base + "/api/v1/profiles/" + id)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// WriteProfile 写角色。
func (c *HTTPClient) WriteProfile(id string, raw []byte) error {
	req, err := http.NewRequest("PUT", c.base+"/api/v1/profiles/"+id, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/yaml")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncateForErr(data))
	}
	return nil
}

// KnowledgeLint 知识库检查。
func (c *HTTPClient) KnowledgeLint() (string, error) {
	resp, err := c.hc.Get(c.base + "/api/v1/knowledge/lint")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(data), err
}

// KnowledgePromote 提升知识。
func (c *HTTPClient) KnowledgePromote(ctx context.Context, relPath, globalRepo string) (string, error) {
	var out struct {
		Commit string `json:"commit"`
	}
	if err := c.do("POST", "/api/v1/knowledge/promote?global="+globalRepo,
		map[string]string{"path": relPath}, &out); err != nil {
		return "", err
	}
	return out.Commit, nil
}

// KnowledgePull 拉取知识。
func (c *HTTPClient) KnowledgePull(ctx context.Context, globalRepo string) ([]string, error) {
	var out struct {
		Paths []string `json:"paths"`
	}
	if err := c.do("POST", "/api/v1/knowledge/pull?global="+globalRepo, nil, &out); err != nil {
		return nil, err
	}
	return out.Paths, nil
}

// Doctor 自检。
func (c *HTTPClient) Doctor() []contract.CheckResult {
	var out struct {
		Checks []contract.CheckResult `json:"checks"`
	}
	if err := c.do("GET", "/api/v1/doctor", nil, &out); err != nil {
		return []contract.CheckResult{}
	}
	return out.Checks
}

// Close 无状态。
func (c *HTTPClient) Close() error { return nil }

// 编译期断言：HTTPClient 满足宿主契约。
var _ contract.Interaction = (*HTTPClient)(nil)

// InteractionClient 是"会探测守护进程的客户端"的最小面（resolveDaemon
// 的返回类型；HTTPClient 实现——未来 TUI/IDE 插件的 Go 客户端同形）。
type InteractionClient interface {
	contract.Interaction
	Shutdown() error
}

// Shutdown 请求守护进程退出（无进行中任务时的 marl stop 形态；daemon
// 未配关停钩子时为无害 no-op）。
func (c *HTTPClient) Shutdown() error {
	return c.do("POST", "/api/v1/shutdown", nil, nil)
}
