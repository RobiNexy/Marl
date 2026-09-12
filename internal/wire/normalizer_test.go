package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"marl/internal/types"
)

// ---------------------------------------------------------------------------
// 夹具：能力表桩 + Canonical 片段
// ---------------------------------------------------------------------------

const (
	testEndpoint = "deepseek-main"
	testModel    = "deepseek/chat"
	testRemote   = "deepseek-flash"
	testBucket   = types.AgentID("agent-01")
)

// capsFor 构造一张声明能力表。默认形态照 DeepSeek openai_chat：
// level 控制 + 隐式前缀缓存（两处都必须在测试里显式写出来——默认值猜错
// 会让"档位翻译"这条测试测到另一条分支上）。
func capsFor(has []types.Capability, levels []string) ModelCaps {
	return ModelCaps{
		Has:             has,
		MaxContext:      65536,
		MaxOutput:       8192,
		CacheMode:       CacheImplicitPrefix,
		ThinkingControl: ThinkControlLevel,
		ThinkingLevels:  levels,
	}
}

// capsProvider 是 CapsProvider 的测试桩。
//
// 未知 (model, endpoint) 一律报错而不是回落：能力表回落等于用 A 端点的能力
// 描述去发 B 端点的请求（Catalog.EffectiveCaps 契约）。
type capsProvider struct {
	caps     ModelCaps
	modelID  string
	endpoint string
	err      error
}

func (p capsProvider) EffectiveCaps(modelID, endpoint string) (ModelCaps, error) {
	if p.err != nil {
		return ModelCaps{}, p.err
	}
	if modelID != p.modelID || endpoint != p.endpoint {
		return ModelCaps{}, fmt.Errorf("测试桩：能力表里没有 (%q, %q)", modelID, endpoint)
	}
	return p.caps, nil
}

// frozenSeg / turnSeg 造片段时必须把 Stability 与 Kind 一起写对：
// layoutOpenAIChatMessages 会校验不变量表，写错就是一条明确的错误。
func frozenSeg(kind SegmentKind, text string) Segment {
	return Segment{Kind: kind, Speaker: SpeakerFramework, Content: text, Stability: types.StabilityFrozen}
}

func turnSeg(text string) Segment {
	return Segment{Kind: SegTurn, Speaker: SpeakerHuman, Content: text, Stability: types.StabilityStable}
}

func testBinding() types.Binding {
	return types.Binding{
		RungID:      "r0",
		RungIndex:   0,
		Endpoint:    testEndpoint,
		Model:       testModel,
		CacheBucket: testBucket,
		Wire:        types.WireOpenAIChat,
		CachePrefix: ModelCachePrefix(testModel, testEndpoint),
	}
}

// baseCanonical 是"冻结前缀 + 一个问题"的最小请求：
// system + standing（frozen，共享前缀）+ turn（stable）+ 一份工具表。
func baseCanonical() *CanonicalRequest {
	return &CanonicalRequest{
		Segments: []Segment{
			frozenSeg(SegSystem, "<system>你是 Marl。</system>"),
			frozenSeg(SegStanding, "<standing>偏好：简洁。</standing>"),
			turnSeg("<turn>你好</turn>"),
		},
		Tools: []ToolDef{{
			Name:        "read_file",
			Description: "读文件",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
		Sampling: types.SamplingParams{MaxTokens: 512, TimeoutMs: 30000},
	}
}

func mustNormalizer(t *testing.T, caps ModelCaps, override *NormalizePolicy) *OpenAICompatNormalizer {
	t.Helper()
	n, err := NewOpenAICompatNormalizer(capsProvider{caps: caps, modelID: testModel, endpoint: testEndpoint}, override)
	if err != nil {
		t.Fatalf("构造 Normalizer 失败: %v", err)
	}
	return n
}

func mustBuild(t *testing.T, n *OpenAICompatNormalizer, req *CanonicalRequest) *WireRequest {
	t.Helper()
	wr, _, err := n.BuildRequest(req, testBinding())
	if err != nil {
		t.Fatalf("BuildRequest 失败: %v", err)
	}
	return wr
}

func mustEncode(t *testing.T, req *WireRequest) []byte {
	t.Helper()
	body, err := EncodeOpenAIChatBody(req, testRemote, DefaultBucketField)
	if err != nil {
		t.Fatalf("编码请求体失败: %v", err)
	}
	return body
}

// firstDiff 返回两个字节序列第一个不同的下标（用于把"前缀漂移"报成具体位置）。
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// sameThinking 比较协议形态（BudgetTokens 是 *int，不能直接比结构体）。
func sameThinking(a, b openAIChatThinking) bool {
	if a.Type != b.Type || a.ReasoningEffort != b.ReasoningEffort {
		return false
	}
	switch {
	case a.BudgetTokens == nil && b.BudgetTokens == nil:
		return true
	case a.BudgetTokens == nil || b.BudgetTokens == nil:
		return false
	default:
		return *a.BudgetTokens == *b.BudgetTokens
	}
}

// sameMessage 逐字段比较两条线路消息（WireMessage 含切片，不能直接用 != 比）。
// 缓存前缀的判据只需要这些"会被编码进请求体"的字段。
func sameMessage(a, b WireMessage) bool {
	return a.Role == b.Role &&
		a.Content == b.Content &&
		a.ToolCallID == b.ToolCallID &&
		a.CacheControl == b.CacheControl &&
		len(a.Attachments) == len(b.Attachments) &&
		len(a.ToolCalls) == len(b.ToolCalls)
}

// ---------------------------------------------------------------------------
// TestNormalizerByteStability（13.3 点名要求，与 TestCachePrefix 互为姊妹）
// ---------------------------------------------------------------------------

// TestNormalizerByteStability 守"隐式前缀缓存的地基"：缓存按前缀字节命中，
// 任何非确定性（map 遍历顺序、时间戳、随机 id、"顺手" trim/转义）都会让前缀
// 漂移——**不报错、不告警，只有账单变贵**。
//
// 测试结构是"同一输入的字节相等 + 冻结段的字节相等"，而不是逐字节 golden：
// golden 会在任何人调整字段顺序时报警（那是一次有意的、需评估的改动），
// 而这里的判据在字段顺序变化后仍然表达同一件事。
func TestNormalizerByteStability(t *testing.T) {
	caps := capsFor([]types.Capability{types.CapToolCall, types.CapThinking}, []string{"none", "low", "high", "max"})
	n := mustNormalizer(t, caps, nil)

	t.Run("同一输入 → 逐字节相同的请求体", func(t *testing.T) {
		first := mustEncode(t, mustBuild(t, n, baseCanonical()))
		for i := 0; i < 20; i++ {
			got := mustEncode(t, mustBuild(t, n, baseCanonical()))
			if !bytes.Equal(got, first) {
				t.Fatalf("第 %d 次构建的请求体与第一次不同（首个不同字节在下标 %d）——"+
					"非确定性的请求字节会让隐式前缀缓存永远不命中，而缓存是成本命脉:\n%s\n%s",
					i, firstDiff(got, first), got, first)
			}
		}
	})

	t.Run("不同实例 → 相同字节（无实例内状态泄漏）", func(t *testing.T) {
		other := mustNormalizer(t, caps, nil)
		got, want := mustEncode(t, mustBuild(t, other, baseCanonical())), mustEncode(t, mustBuild(t, n, baseCanonical()))
		if !bytes.Equal(got, want) {
			t.Errorf("两个 Normalizer 实例对同一输入给出了不同字节（首个不同字节在下标 %d）", firstDiff(got, want))
		}
	})

	t.Run("换问题：冻结前缀逐字节不变", func(t *testing.T) {
		// 这正是隐式前缀缓存的命中条件：system + standing 段（frozen）在两次
		// 不同问题之间必须逐字节相同。任何 trim / 重新渲染都会让它失效。
		reqA := baseCanonical()
		reqB := baseCanonical()
		reqB.Segments[2] = turnSeg("<turn>今天天气如何</turn>")

		wrA, wrB := mustBuild(t, n, reqA), mustBuild(t, n, reqB)
		// 多条 system 段会被 MultiSystem 策略折叠进第一条，因此消息序列是
		// [system(system+standing), user(turn)]：除最后一条外都属冻结前缀。
		if len(wrA.Messages) != len(wrB.Messages) || len(wrA.Messages) < 2 {
			t.Fatalf("消息条数异常: %d / %d（夹具已变，请同步本用例的前缀切分）", len(wrA.Messages), len(wrB.Messages))
		}
		if sameMessage(wrA.Messages[len(wrA.Messages)-1], wrB.Messages[len(wrB.Messages)-1]) {
			t.Fatal("夹具问题：两次请求的最后一条消息相同，本用例就没在测\"换问题\"")
		}
		for i := 0; i < len(wrA.Messages)-1; i++ {
			if !sameMessage(wrA.Messages[i], wrB.Messages[i]) {
				t.Errorf("第 %d 条冻结消息在两次请求间不同:\n%+v\n%+v", i, wrA.Messages[i], wrB.Messages[i])
			}
		}

		bodyA, bodyB := mustEncode(t, wrA), mustEncode(t, wrB)
		ia, ib := bytes.Index(bodyA, []byte("<turn>")), bytes.Index(bodyB, []byte("<turn>"))
		if ia < 0 || ib < 0 {
			t.Fatalf("夹具问题：请求体里找不到 <turn> 标注（%d / %d）", ia, ib)
		}
		if !bytes.Equal(bodyA[:ia], bodyB[:ib]) {
			t.Errorf("冻结前缀的字节不同（首个不同字节在下标 %d）——命中率会静默下降:\n%s\n%s",
				firstDiff(bodyA[:ia], bodyB[:ib]), bodyA[:ia], bodyB[:ib])
		}
	})

	t.Run("XML 标注逐字节保留（不被转义）", func(t *testing.T) {
		body := mustEncode(t, mustBuild(t, n, baseCanonical()))
		if !bytes.Contains(body, []byte("<system>你是 Marl。</system>")) {
			t.Errorf("XML 标注在请求体里被改写了（trim/转义都会改缓存前缀字节）:\n%s", body)
		}
		if bytes.Contains(body, []byte(`\u003c`)) {
			t.Errorf("请求体里出现了 HTML 转义（\\u003c）——语义等价，但探测报告与审计要给人看原文:\n%s", body)
		}
	})

	t.Run("调用方改产物不会影响下一次构建", func(t *testing.T) {
		want := mustEncode(t, mustBuild(t, n, baseCanonical()))

		wr := mustBuild(t, n, baseCanonical())
		wr.Messages[0].Content = "被改过的内容"
		wr.Messages = append(wr.Messages, WireMessage{Role: types.WireUser, Content: "塞进来的"})
		wr.Tools[0].Name = "被改过的工具名"

		if got := mustEncode(t, mustBuild(t, n, baseCanonical())); !bytes.Equal(got, want) {
			t.Errorf("改一份产物影响了下一次构建（Normalizer 持有或复用了可变切片）:\n%s\n%s", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// TestNormalizerThinking：档位 / 开关 / 预算三种控制方式的翻译表
// ---------------------------------------------------------------------------

// TestNormalizerThinking 守 10.10 的"能力缺失从来不静默"。
//
// 每一条都同时断言**产出形态**与**降级记录**：只断言产出会让"静默丢掉档位"
// 通过（产出看起来是对的，只是没带参数），而只断言降级会让"记录了降级但
// 参数仍然乱发"通过。两者必须成对检查。
func TestNormalizerThinking(t *testing.T) {
	levelCaps := capsFor([]types.Capability{types.CapThinking}, []string{"none", "low", "high", "max"})
	// 一个没有关闭档位的模型：off 必须记降级而不是硬发 "none"。
	noOffCaps := capsFor([]types.Capability{types.CapThinking}, []string{"low", "high"})
	noThinkingCaps := capsFor([]types.Capability{types.CapToolCall}, nil)
	boolCaps := ModelCaps{
		Has:             []types.Capability{types.CapThinking},
		MaxContext:      65536,
		MaxOutput:       8192,
		CacheMode:       CacheImplicitPrefix,
		ThinkingControl: ThinkControlBool,
	}
	budgetCaps := ModelCaps{
		Has:             []types.Capability{types.CapThinking},
		MaxContext:      65536,
		MaxOutput:       8192,
		CacheMode:       CacheImplicitPrefix,
		ThinkingControl: ThinkControlBudget,
	}

	intp := func(v int) *int { return &v }

	cases := []struct {
		name     string
		spec     types.ThinkingSpec
		caps     ModelCaps
		want     openAIChatThinking
		wantNil  bool
		wantDegr []DegradationKind
	}{
		{
			name:    "不指定档位 → 不发任何思维参数（不指定 ≠ 关闭）",
			spec:    types.ThinkingSpec{},
			caps:    levelCaps,
			wantNil: true,
		},
		{
			name: "off（level 控制）→ 顶层 reasoning_effort=none",
			spec: types.ThinkingSpec{Level: thinkingOffLevel},
			caps: levelCaps,
			want: openAIChatThinking{ReasoningEffort: reasoningEffortOff},
		},
		{
			name:     "off 但模型没有关闭档位 → 不发 + 记降级（不硬发 none）",
			spec:     types.ThinkingSpec{Level: thinkingOffLevel},
			caps:     noOffCaps,
			wantNil:  true,
			wantDegr: []DegradationKind{DegradThinkingLevel},
		},
		{
			name: "档位 high → 原样进顶层 reasoning_effort",
			spec: types.ThinkingSpec{Level: "high"},
			caps: levelCaps,
			want: openAIChatThinking{ReasoningEffort: "high"},
		},
		{
			name:     "档位不在模型档位表 → 不发 + 记降级（不擅自挑最接近的档位）",
			spec:     types.ThinkingSpec{Level: "ultra"},
			caps:     levelCaps,
			wantNil:  true,
			wantDegr: []DegradationKind{DegradThinkingLevel},
		},
		{
			name:     "模型未声明 thinking 能力 → 移除参数 + 记降级",
			spec:     types.ThinkingSpec{Level: "high"},
			caps:     noThinkingCaps,
			wantNil:  true,
			wantDegr: []DegradationKind{DegradThinkingUnavailable},
		},
		{
			name:    "模型未声明 thinking 能力 + off → 静默满足（关闭不需要能力）",
			spec:    types.ThinkingSpec{Level: thinkingOffLevel},
			caps:    noThinkingCaps,
			wantNil: true,
		},
		{
			name:     "level 控制 + 带预算 → 档位生效，预算被剔除并记降级",
			spec:     types.ThinkingSpec{Level: "high", Budget: intp(500)},
			caps:     levelCaps,
			want:     openAIChatThinking{ReasoningEffort: "high"},
			wantDegr: []DegradationKind{DegradParamStripped},
		},
		{
			name: "bool 控制：on → thinking.type=enabled",
			spec: types.ThinkingSpec{Level: thinkingOnLevel},
			caps: boolCaps,
			want: openAIChatThinking{Type: thinkingTypeEnabled},
		},
		{
			name: "bool 控制：off → thinking.type=disabled",
			spec: types.ThinkingSpec{Level: thinkingOffLevel},
			caps: boolCaps,
			want: openAIChatThinking{Type: thinkingTypeDisabled},
		},
		{
			name:     "bool 控制：档位名不是开关词 → 不发 + 记降级",
			spec:     types.ThinkingSpec{Level: "low"},
			caps:     boolCaps,
			wantNil:  true,
			wantDegr: []DegradationKind{DegradThinkingLevel},
		},
		{
			name: "budget 控制：给预算 → thinking.type=enabled + budget_tokens",
			spec: types.ThinkingSpec{Budget: intp(2048)},
			caps: budgetCaps,
			want: openAIChatThinking{Type: thinkingTypeEnabled, BudgetTokens: intp(2048)},
		},
		{
			name: "budget 控制：off → disabled（不需要预算）",
			spec: types.ThinkingSpec{Level: thinkingOffLevel, Budget: intp(2048)},
			caps: budgetCaps,
			want: openAIChatThinking{Type: thinkingTypeDisabled},
		},
		{
			name:    "budget 控制：没给预算 → 不发（没有要求 ≠ 要求了但拿不到）",
			spec:    types.ThinkingSpec{},
			caps:    budgetCaps,
			wantNil: true,
		},
		{
			name:     "budget 控制 + 档位 → 预算生效，档位被忽略并记降级",
			spec:     types.ThinkingSpec{Level: "high", Budget: intp(2048)},
			caps:     budgetCaps,
			want:     openAIChatThinking{Type: thinkingTypeEnabled, BudgetTokens: intp(2048)},
			wantDegr: []DegradationKind{DegradThinkingLevel},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, degr, err := normalizeOpenAIChatThinking(c.spec, c.caps)
			if err != nil {
				t.Fatalf("翻译档位失败: %v", err)
			}
			if c.wantNil {
				if got != nil {
					t.Errorf("期望不发任何思维参数（nil），得到 %#v", got)
				}
			} else {
				form, ok := got.(openAIChatThinking)
				if !ok {
					t.Fatalf("期望协议形态 openAIChatThinking，得到 %T", got)
				}
				if !sameThinking(form, c.want) {
					t.Errorf("形态不符：得到 %+v，期望 %+v", form, c.want)
				}
			}
			assertDegradationKinds(t, degr, c.wantDegr)
			if err := assertOpenAIChatThinking(got); err != nil {
				t.Errorf("本线路的 Assert 拒绝了自家翻译出的形态（两处对协议的理解不一致）: %v", err)
			}
		})
	}

	t.Run("负预算报错（nil 才是未设置）", func(t *testing.T) {
		neg := -1
		if _, _, err := normalizeOpenAIChatThinking(types.ThinkingSpec{Budget: &neg}, budgetCaps); err == nil {
			t.Error("负预算应当报错：它会被厂商当成无意义值，而调用方以为预算生效了")
		}
	})

	t.Run("未定义的 ThinkingControl 报错（这类 models.yaml 应在加载期被拒）", func(t *testing.T) {
		bad := levelCaps
		bad.ThinkingControl = ""
		if _, _, err := normalizeOpenAIChatThinking(types.ThinkingSpec{Level: "low"}, bad); err == nil {
			t.Error("ThinkingControl 零值应当报错：不得被默认成 bool")
		}
	})
}

// assertDegradationKinds 比对降级种类（只看 Kind 与顺序：Reason 是给人读的，
// 逐字断言会让措辞调整变成测试失败）。
func assertDegradationKinds(t *testing.T, got []Degradation, want []DegradationKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("降级记录条数不符：得到 %+v，期望 %v", got, want)
	}
	for i := range want {
		if got[i].Kind != want[i] {
			t.Errorf("第 %d 条降级 Kind=%q，期望 %q（完整记录 %+v）", i, got[i].Kind, want[i], got[i])
		}
		if !got[i].Kind.Valid() || got[i].Reason == "" {
			t.Errorf("第 %d 条降级不满足不变量（Kind.Valid / Reason 非空）: %+v", i, got[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 装配期与产出期的拒绝清单
// ---------------------------------------------------------------------------

// TestNormalizerConstruction 守"配置错误必须在启动期炸"。
//
// 这些取值全部来自配置文件（endpoints.yaml / models.yaml），错了的后果是
// 请求照发、行为悄悄不同——所以它们必须在构造期失败，而不是等第一个任务
// 跑到一半才以"模型答非所问"的形式暴露。
func TestNormalizerConstruction(t *testing.T) {
	good := capsFor([]types.Capability{types.CapThinking}, []string{"none", "low"})

	t.Run("caps 为 nil → 报错（没有能力查询就无从翻译）", func(t *testing.T) {
		if _, err := NewOpenAICompatNormalizer(nil, nil); err == nil {
			t.Error("caps 为 nil 应当报错")
		}
	})

	t.Run("未实现的策略取值 → 构造期报错（不静默降级成默认策略）", func(t *testing.T) {
		cases := []struct {
			name     string
			override NormalizePolicy
		}{
			{"native_prefill 需要 prefix 标记与 /beta 接入点", NormalizePolicy{TailAssistant: TailAssistantNativePrefill}},
			{"interleave_empty 会插入空消息，违反非空不变量", NormalizePolicy{ConsecutiveSame: ConsecutiveSameInterleaveEmpty}},
			{"inline_as_user 会把工具结果伪装成用户输入", NormalizePolicy{ToolResultRole: ToolResultInlineAsUser}},
		}
		for _, c := range cases {
			ov := c.override
			if _, err := NewOpenAICompatNormalizer(capsProvider{caps: good, modelID: testModel, endpoint: testEndpoint}, &ov); err == nil {
				t.Errorf("%s：构造应当报错", c.name)
			}
		}
	})

	t.Run("策略名写错 → 构造期报错（空值才表示用线路默认）", func(t *testing.T) {
		ov := NormalizePolicy{MultiSystem: "concat_alll"}
		if _, err := NewOpenAICompatNormalizer(capsProvider{caps: good, modelID: testModel, endpoint: testEndpoint}, &ov); err == nil {
			t.Error("拼错的策略名应当报错：静默挑一个布局会让请求形状与配置意图不符")
		}
	})

	t.Run("空策略不改变布局（LoadPolicyOverride 的契约）", func(t *testing.T) {
		n := mustNormalizer(t, good, nil)
		empty := NormalizePolicy{}
		got := mustNormalizer(t, good, &empty).Policy()
		want := n.Policy()
		if got.MultiSystem != want.MultiSystem || got.ConsecutiveSame != want.ConsecutiveSame ||
			got.TailAssistant != want.TailAssistant || got.ToolResultRole != want.ToolResultRole {
			t.Errorf("空策略改变了布局：得到 %+v，期望 %+v", got, want)
		}
	})
}

// TestNormalizerBuildRejects 守入参自检：这些错误属于"编译层有 bug"，
// 必须在去程拒绝，而不是发一个厂商侧完全合法、语义却不对的请求。
func TestNormalizerBuildRejects(t *testing.T) {
	caps := capsFor([]types.Capability{types.CapThinking}, []string{"none", "low"})
	n := mustNormalizer(t, caps, nil)

	cases := []struct {
		name   string
		mutate func(req *CanonicalRequest, b *types.Binding)
	}{
		{"Binding 不完整", func(_ *CanonicalRequest, b *types.Binding) { b.Model = "" }},
		{"线路不匹配", func(_ *CanonicalRequest, b *types.Binding) { b.Wire = otherWire }},
		{"CacheBucket 为空", func(_ *CanonicalRequest, b *types.Binding) { b.CacheBucket = "" }},
		{"Segments 为空", func(req *CanonicalRequest, _ *types.Binding) { req.Segments = nil }},
		{"Prefill 是空串", func(req *CanonicalRequest, _ *types.Binding) { empty := ""; req.Prefill = &empty }},
		{"Segment 的 Kind 未定义", func(req *CanonicalRequest, _ *types.Binding) { req.Segments[0].Kind = "" }},
		{"Segment 的 Speaker 未定义", func(req *CanonicalRequest, _ *types.Binding) { req.Segments[0].Speaker = "" }},
		{"Segment 的 Stability 与 Kind 不符", func(req *CanonicalRequest, _ *types.Binding) {
			req.Segments[0].Stability = types.StabilityVolatile
		}},
		{"Segment 既无 Content 也无 ToolCalls", func(req *CanonicalRequest, _ *types.Binding) { req.Segments[2].Content = "" }},
		{"附件未实现（阶段 1）", func(req *CanonicalRequest, _ *types.Binding) {
			req.Segments[2].Attachments = []types.Attachment{{}}
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, b := baseCanonical(), testBinding()
			if c.mutate != nil {
				c.mutate(req, &b)
			}
			if _, _, err := n.BuildRequest(req, b); err == nil {
				t.Error("应当报错")
			}
		})
	}

	t.Run("nil 请求", func(t *testing.T) {
		if _, _, err := n.BuildRequest(nil, testBinding()); err == nil {
			t.Error("nil 请求应当报错（而不是解引用崩掉）")
		}
	})
}
