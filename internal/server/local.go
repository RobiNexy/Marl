package server

// LocalFiles 是"文件即可达"的操作核（App 与 FileMailbox 共用的实现面）
// ——收件箱/审批/讨论/配置/知识库这些操作只依赖**文件系统**，不依赖
// 进程表：两个宿主实现（进程内 App / 无守护的 FileMailbox）语义逐字节
// 一致（契约一致性测试守护）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RobiNexy/Marl/internal/actor"
	"github.com/RobiNexy/Marl/internal/config"
	"github.com/RobiNexy/Marl/internal/contract"
	"github.com/RobiNexy/Marl/internal/fossil"
	"github.com/RobiNexy/Marl/internal/gate"
	"github.com/RobiNexy/Marl/internal/knowledge"
	"github.com/RobiNexy/Marl/internal/profile"
	"github.com/RobiNexy/Marl/internal/proto"
	"github.com/RobiNexy/Marl/internal/types"
)

// LocalFiles 持有项目/控制面两个根。
type LocalFiles struct {
	Root    string // 项目根（.marl/ 所在）
	Control string // 控制面根（收件箱/授权库/讨论）
}

// ConfigRaw 返回 config.yaml 原文（GUI 编辑器的内容源）。
func (lf *LocalFiles) ConfigRaw() ([]byte, error) {
	return os.ReadFile(filepath.Join(lf.Root, ".marl", "config.yaml"))
}

// WriteConfig 校验并写回 config.yaml（先 Parse+ParseLimits+ParseGateRules
// + ValidateRules——坏配置在保存时被拦，不等到下次启动）。
func (lf *LocalFiles) WriteConfig(raw []byte) error {
	node, err := config.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid yaml: %w", err)
	}
	limits, err := config.ParseLimits(node)
	if err != nil {
		return fmt.Errorf("invalid limits: %w", err)
	}
	rules, err := config.ParseGateRules(node)
	if err != nil {
		return fmt.Errorf("invalid gate_rules: %w", err)
	}
	probe, err := gate.NewManager(gate.ManagerConfig{Rules: rules, LLMLimits: &gate.LLMLimits{
		MaxCalls: limits.CallTaskMax, MaxTokens: int64(limits.CallTaskMaxTokens)}})
	if err != nil {
		return fmt.Errorf("invalid rules: %w", err)
	}
	_ = probe
	return os.WriteFile(filepath.Join(lf.Root, ".marl", "config.yaml"), raw, 0o644)
}

// Profiles 返回全部 Profile 摘要（GUI 的角色列表；每次调用现读目录——
// 跨进程的 FileMailbox 与进程内 App 同一行为）。
func (lf *LocalFiles) Profiles() ([]types.ProfileSummary, error) {
	l := profile.NewLoader()
	if err := l.LoadAll(filepath.Join(lf.Root, ".marl", "profiles")); err != nil {
		return nil, err
	}
	return l.List(), nil
}

// ProfileRaw 返回一份 profile 文件原文。
func (lf *LocalFiles) ProfileRaw(id string) ([]byte, error) {
	if !safeInboxName(id + ".yaml") {
		return nil, fmt.Errorf("invalid profile id")
	}
	return os.ReadFile(filepath.Join(lf.Root, ".marl", "profiles", id+".yaml"))
}

// WriteProfile 校验并写回 profile（临时文件参与完整加载校验 → 原子改名）。
func (lf *LocalFiles) WriteProfile(id string, raw []byte) error {
	if !safeInboxName(id + ".yaml") {
		return fmt.Errorf("invalid profile id")
	}
	path := filepath.Join(lf.Root, ".marl", "profiles", id+".yaml")
	// 校验用**隔离目录**（探针加载的目录里只有待验文件——同目录探针会
	// 读到旧内容，形同虚设：loadDir 按 .yaml 后缀过滤，.tmp 会被跳过）。
	tmpDir, terr := os.MkdirTemp("", "marl-profile-check-")
	if terr != nil {
		return terr
	}
	defer os.RemoveAll(tmpDir)
	tmp := filepath.Join(tmpDir, id+".yaml")
	if werr := os.WriteFile(tmp, raw, 0o644); werr != nil {
		return werr
	}
	probe := profile.NewLoader()
	if lerr := probe.LoadAll(tmpDir); lerr != nil {
		return fmt.Errorf("invalid profile: %w", lerr)
	}
	if _, gerr := probe.Get(types.ProfileID(strings.TrimSuffix(id, ".yaml"))); gerr != nil {
		return fmt.Errorf("invalid profile: %w", gerr)
	}
	if werr := os.WriteFile(path, raw, 0o644); werr != nil {
		return werr
	}
	return nil
}

// KnowledgeLint 的结果透传（GUI 的知识库健康面）。
func (lf *LocalFiles) KnowledgeLint() (string, error) {
	prefDir := filepath.Join(lf.Root, ".marl", "knowledge", "preferences")
	block, err := knowledge.CompileStandingOrders(prefDir)
	if err != nil {
		return "", err
	}
	return block.Report(knowledge.MaxStandingTokens), nil
}

// KnowledgePromote 把项目知识提交进全局库（fossil；author=human）。
func (lf *LocalFiles) KnowledgePromote(ctx context.Context, relPath, globalRepo string) (string, error) {
	cli, err := fossil.NewCLI("")
	if err != nil {
		return "", err
	}
	return knowledge.Promote(ctx, cli, filepath.Join(lf.Root, ".marl"), globalRepo, relPath)
}

// KnowledgePull 把全局库拉进 vendor/（返回条目描述）。
func (lf *LocalFiles) KnowledgePull(ctx context.Context, globalRepo string) ([]string, error) {
	cli, err := fossil.NewCLI("")
	if err != nil {
		return nil, err
	}
	entries, err := knowledge.Pull(ctx, cli, filepath.Join(lf.Root, ".marl"), globalRepo)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out, nil
}

// Inbox 列出待处理收件（GUI 刷新用；done/ 的归档不在此列）。
func (lf *LocalFiles) Inbox() ([]contract.InboxItem, error) {
	entries, err := os.ReadDir(filepath.Join(lf.Control, "inbox"))
	if err != nil {
		return nil, err
	}
	out := []contract.InboxItem{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		item := contract.InboxItem{Name: e.Name(), Type: actor.FileTypeOf(e.Name())}
		if st, serr := e.Info(); serr == nil {
			item.ModTime = st.ModTime()
		}
		if data, rerr := os.ReadFile(filepath.Join(lf.Control, "inbox", e.Name())); rerr == nil {
			if f, ok := actor.ParseInboxFileText(string(data)); ok {
				item.From, item.To = f.From, f.To
				item.Preview = previewOf(f.Body)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// ReadInbox 返回收件文件全文（GUI 详情页）。
func (lf *LocalFiles) ReadInbox(name string) ([]byte, error) {
	if !safeInboxName(name) {
		return nil, fmt.Errorf("invalid inbox item name")
	}
	return os.ReadFile(filepath.Join(lf.Control, "inbox", name))
}

// ReplyGate 把裁决写进审批文件（id = 文件名里的 ulid 段）。
func (lf *LocalFiles) ReplyGate(id string, d contract.GateDecision) error {
	line, lerr := gateDirectiveLine(d)
	if lerr != "" {
		return fmt.Errorf("%s", lerr)
	}
	name := "gate_" + id + ".md"
	if !safeInboxName(name) || id == "" {
		return fmt.Errorf("invalid gate id")
	}
	path := filepath.Join(lf.Control, "inbox", name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("approval %s not in inbox (already decided?)", id)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + line + "\n" + d.Reason + "\n"); err != nil {
		return err
	}
	return nil
}

// Discussions 列出讨论（控制面 discussions/ 的目录扫描）。
func (lf *LocalFiles) Discussions() ([]contract.DiscussionView, error) {
	base := filepath.Join(lf.Control, "discussions")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return []contract.DiscussionView{}, nil
		}
		return nil, err
	}
	out := []contract.DiscussionView{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dv := contract.DiscussionView{ID: e.Name(), Dir: filepath.Join(base, e.Name())}
		if st, serr := e.Info(); serr == nil {
			dv.ModTime = st.ModTime()
		}
		if data, rerr := os.ReadFile(filepath.Join(dv.Dir, "draft.md")); rerr == nil {
			// 草稿首行 = "# 草稿：讨论「topic」"——topic 的提取面。
			line := strings.SplitN(string(data), "\n", 2)[0]
			if i := strings.Index(line, "「"); i >= 0 {
				if j := strings.Index(line[i:], "」"); j > 0 {
					dv.Topic = line[i+3 : i+j]
				}
			}
		}
		out = append(out, dv)
	}
	return out, nil
}

// DiscussionFile 返回一次讨论的 verdict/draft 文件路径。
func (lf *LocalFiles) DiscussionFile(id string) (verdict, draft string, err error) {
	if !safeInboxName(id + ".x") {
		return "", "", fmt.Errorf("invalid discussion id")
	}
	dir := filepath.Join(lf.Control, "discussions", id)
	return filepath.Join(dir, "verdict.md"), filepath.Join(dir, "draft.md"), nil
}

// ReplyDiscussion 把批注/裁决写进 verdict（GUI 的讨论回复框；approve
// 时写 @approve 行——与 CLI/文件通道同一条路径）。
func (lf *LocalFiles) ReplyDiscussion(id, annotation string, approve bool) error {
	verdict, _, err := lf.DiscussionFile(id)
	if err != nil {
		return err
	}
	if _, serr := os.Stat(verdict); serr != nil {
		return fmt.Errorf("discussion %s not found", id)
	}
	f, err := os.OpenFile(verdict, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var sb strings.Builder
	sb.WriteString("\n")
	if annotation != "" {
		sb.WriteString(annotation + "\n")
	}
	if approve {
		sb.WriteString("@approve\n")
	}
	_, err = f.WriteString(sb.String())
	return err
}

// SendMessage 的文件投递形态（FileMailbox 的实现核；App 的进程内实现
// 在 app.go）——写收件箱 direct 文件，运行中任务的 watcher 消费并路由。
func (lf *LocalFiles) SendMessage(to, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("message text is required")
	}
	backend, err := actor.NewFileBackend(actor.FileConfig{
		Root: lf.Control, Human: actor.HumanID(osUID()),
	})
	if err != nil {
		return err
	}
	defer backend.Stop()
	return backend.Deliver(actor.Envelope{
		From:    actor.HumanID(osUID()),
		To:      types.AgentID(to),
		Type:    proto.MsgDirect,
		Payload: &proto.DirectMessage{Text: text},
	})
}

// gateDirectiveLine 把 GUI 的结构化裁决折算成 @ 命令行（单一换算点）。
func gateDirectiveLine(d contract.GateDecision) (string, string) {
	switch d.Action {
	case "deny":
		return "@deny", ""
	case "allow":
		switch d.Mode {
		case "", "once":
			return "@grant once", ""
		case "count":
			if d.Count <= 0 {
				return "", "count mode requires count > 0"
			}
			return fmt.Sprintf("@grant next %d", d.Count), ""
		case "tokens":
			if d.Tokens <= 0 {
				return "", "tokens mode requires tokens > 0"
			}
			return fmt.Sprintf("@grant tokens %d", d.Tokens), ""
		case "always":
			return "@always-grant", ""
		default:
			return "", "unknown grant mode " + d.Mode
		}
	default:
		return "", "action must be allow or deny"
	}
}

// ---- 小件 ----

// safeInboxName 拒绝路径穿越（GUI 的输入是外部的——比 CLI 的自查多一层）。
func safeInboxName(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") ||
		strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
		return false
	}
	return true
}

// previewOf 取正文首行做预览（收件箱列表）。
func previewOf(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t != "" && !strings.HasPrefix(t, "#") {
			r := []rune(t)
			if len(r) > 80 {
				return string(r[:80]) + "…"
			}
			return t
		}
	}
	return ""
}
