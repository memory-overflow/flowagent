package contextmgr

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// ============================================================
// 上限解析
// ============================================================

func TestResolveContextLimit_ConfiguredWins(t *testing.T) {
	// 显式配置优先于内置表：运维改了配置就该生效，不能被代码里的表盖掉
	if got := ResolveContextLimit(200000, "hy4-preview"); got != 200000 {
		t.Fatalf("configured should win, got %d", got)
	}
}

func TestResolveContextLimit_LookupTable(t *testing.T) {
	cases := map[string]int{
		// hy4-preview 官方口径：窗口 1024k，但最大输入只有 960k
		"hy4-preview":            983040,
		"hy3-preview":            196608,
		"gemini-3.1-pro-preview": 1048576,
		"gpt-5.6-sol":            922000,
		"gpt-5.6-terra":          922000,
		"gpt-5.6-luna":           922000,
		"gpt-6-astra":            922000,
		"glm-5.3-flash":          1048576,
		"glm-4.6":                204800,
		// Anthropic 只标窗口与输出，输入上限按 窗口-输出 推算
		"claude-opus-5":    917504, // 1M - 128K
		"claude-sonnet-5":  917504,
		"claude-opus-4-8":  393216, // 500K - 128K
		"claude-haiku-4-5": 139264, // 200K - 64K
	}
	for name, want := range cases {
		if got := ResolveContextLimit(0, name); got != want {
			t.Errorf("%s: want %d, got %d", name, want, got)
		}
	}
}

func TestResolveContextLimit_PrefixMatchWithDateSuffix(t *testing.T) {
	// 厂商常挂日期后缀，精确匹配会全部落空
	if got := ResolveContextLimit(0, "gpt-5.6-sol-2026-08-01"); got != 922000 {
		t.Fatalf("date-suffixed model should match by prefix, got %d", got)
	}
	if got := ResolveContextLimit(0, "hy4-preview-0828"); got != 983040 {
		t.Fatalf("hy4-preview-0828 should match hy4-preview, got %d", got)
	}
}

func TestResolveContextLimit_LongestPrefixWins(t *testing.T) {
	// glm-5.3-flash 与 glm-5.3 都是前缀，必须取更长的那个。
	// 二者当前值相同，所以直接断言 lookup 命中的键长度语义：
	// 构造一个只有长键能区分的场景 —— glm-4.6 (204800) 不应被 glm-5.3 干扰。
	if got := ResolveContextLimit(0, "glm-4.6"); got != 204800 {
		t.Fatalf("glm-4.6 want 204800, got %d", got)
	}
	// glm-4.6v 是 128K 的多模态版，与 glm-4.6 的 200K 不同。
	// 若被更短的 glm-4.6 抢先命中，会把 128K 模型当成 200K 用而直接溢出。
	if got := ResolveContextLimit(0, "glm-4.6v"); got != 131072 {
		t.Fatalf("glm-4.6v want 131072, got %d", got)
	}
	if got := ResolveContextLimit(0, "glm-4.6v-flash"); got != 131072 {
		t.Fatalf("glm-4.6v-flash want 131072, got %d", got)
	}
}

func TestResolveContextLimit_UnknownFallsBackConservative(t *testing.T) {
	// 未知模型必须回落到保守值，而非乐观的 1M —— 宁可早压，不可溢出
	if got := ResolveContextLimit(0, "brand-new-model-x"); got != DefaultContextLimit {
		t.Fatalf("unknown model want %d, got %d", DefaultContextLimit, got)
	}
	if got := ResolveContextLimit(0, ""); got != DefaultContextLimit {
		t.Fatalf("empty model want %d, got %d", DefaultContextLimit, got)
	}
}

func TestResolveContextLimit_CaseInsensitive(t *testing.T) {
	if got := ResolveContextLimit(0, "HY4-Preview"); got != 983040 {
		t.Fatalf("lookup should be case-insensitive, got %d", got)
	}
}

// ============================================================
// 水位阈值
// ============================================================

func TestNewThresholds(t *testing.T) {
	th := NewThresholds(1000000)
	if th.Trigger != 800000 {
		t.Errorf("trigger want 800000, got %d", th.Trigger)
	}
	if th.Target != 300000 {
		t.Errorf("target want 300000, got %d", th.Target)
	}
	if th.Summary != 100000 {
		t.Errorf("summary want 100000, got %d", th.Summary)
	}
	if th.CompactCall != 600000 {
		t.Errorf("compact call budget want 600000, got %d", th.CompactCall)
	}
}

// TestNewThresholds_CompactCallLeavesRoomForOutput 锁住「压缩调用自身不超限」
// 这条硬约束：输入预算 + 摘要输出必须留出余量，否则压缩调用会被 provider 拒绝。
func TestNewThresholds_CompactCallLeavesRoomForOutput(t *testing.T) {
	for _, limit := range []int{32768, 128000, 262144, 1048576} {
		th := NewThresholds(limit)
		if th.CompactCall+th.Summary > limit {
			t.Errorf("limit %d: compact input %d + summary output %d exceeds the window",
				limit, th.CompactCall, th.Summary)
		}
		// 至少留 20% 给 tiktoken 的计数误差
		if th.CompactCall+th.Summary > int(float64(limit)*0.8) {
			t.Errorf("limit %d: only %d%% headroom left, too tight for counting error",
				limit, (limit-th.CompactCall-th.Summary)*100/limit)
		}
	}
}

func TestNewThresholds_ZeroLimitFallsBack(t *testing.T) {
	// 上限为 0 时不能算出全 0 阈值 —— 那会导致「任何输入都判超限」
	th := NewThresholds(0)
	if th.Limit != DefaultContextLimit {
		t.Fatalf("zero limit should fall back, got %d", th.Limit)
	}
	if th.Trigger <= 0 {
		t.Fatalf("trigger must be positive, got %d", th.Trigger)
	}
}

// ============================================================
// Token 计数
// ============================================================

func TestCountText_Basic(t *testing.T) {
	c := GetCounter()
	if c.CountText("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
	if n := c.CountText("hello world"); n <= 0 || n > 10 {
		t.Errorf("unexpected token count for short english: %d", n)
	}
}

func TestCountText_ChineseIsCheaperThanOneTokenPerChar(t *testing.T) {
	// o200k_base 对中文压缩率明显优于 cl100k_base。
	// 这条断言锁住「没有误切回 cl100k」：14 个中文字符不该超过 14 token。
	c := GetCounter()
	s := "这是一段中文测试文本用于计量"
	n := c.CountText(s)
	if n > len([]rune(s)) {
		t.Errorf("chinese should compress to <= 1 token/char under o200k, got %d for %d chars",
			n, len([]rune(s)))
	}
}

func TestCountMessages_IncludesStructuralOverhead(t *testing.T) {
	c := GetCounter()
	// 同样的正文，拆成两条消息应比一条贵 —— 结构开销必须被计入，
	// 否则长历史（几百条消息）的水位会被系统性低估
	one := []llm.Message{{Role: llm.RoleUser, Content: "aaaa bbbb"}}
	two := []llm.Message{
		{Role: llm.RoleUser, Content: "aaaa"},
		{Role: llm.RoleUser, Content: "bbbb"},
	}
	if c.CountMessages(two) <= c.CountMessages(one) {
		t.Error("two messages should cost more than one due to per-message overhead")
	}
}

func TestCountMessages_EmptyIsZero(t *testing.T) {
	if GetCounter().CountMessages(nil) != 0 {
		t.Error("nil messages should be 0")
	}
}

func TestCountMessages_CountsToolCallArguments(t *testing.T) {
	c := GetCounter()
	bare := []llm.Message{{Role: llm.RoleAssistant, Content: ""}}
	withCall := []llm.Message{{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{
			Name: "write_file",
			Arguments: map[string]interface{}{
				"path":    "a/b/c.md",
				"content": "some fairly long content here to be counted",
			},
		}},
	}}
	if c.CountMessages(withCall) <= c.CountMessages(bare) {
		t.Error("tool call arguments must be counted")
	}
}

func TestCountMessages_CountsSignature(t *testing.T) {
	// Gemini 3 的思维签名是不透明 base64，体量可观且随每条历史回放，
	// 漏算会低估水位
	c := GetCounter()
	without := []llm.Message{{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{Name: "f"}},
	}}
	with := []llm.Message{{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{
			Name:      "f",
			Signature: strings.Repeat("YWJjZGVm", 50),
		}},
	}}
	if c.CountMessages(with) <= c.CountMessages(without) {
		t.Error("signature must be counted")
	}
}

func TestCountMessages_CountsImages(t *testing.T) {
	c := GetCounter()
	noImg := []llm.Message{{Role: llm.RoleUser, Content: "look"}}
	withImg := []llm.Message{{
		Role:    llm.RoleUser,
		Content: "look",
		Images:  []llm.ImagePart{{MimeType: "image/png", Data: "iVBOR"}},
	}}
	got := c.CountMessages(withImg) - c.CountMessages(noImg)
	if got < tokensPerImage {
		t.Errorf("image should add >= %d tokens, got %d", tokensPerImage, got)
	}
}

func TestCountTools_SchemaCounted(t *testing.T) {
	// 工具 schema 是容易被忽略的大头，必须计入
	c := GetCounter()
	if c.CountTools(nil) != 0 {
		t.Error("nil tools should be 0")
	}
	tools := []llm.Tool{{
		Name:        "deep_search",
		Description: "对一个主题做深度检索并返回结论",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query":   map[string]interface{}{"type": "string", "description": "检索主题"},
				"summary": map[string]interface{}{"type": "string", "description": "本次调用的目的"},
			},
			"required": []string{"query", "summary"},
		},
	}}
	if n := c.CountTools(tools); n < 20 {
		t.Errorf("tool schema seems under-counted: %d", n)
	}
}

// ============================================================
// 摘要截断
// ============================================================

func TestTrimToTokens_ShortTextUnchanged(t *testing.T) {
	c := GetCounter()
	s := "短文本"
	if got := c.TrimToTokens(s, 1000); got != s {
		t.Errorf("short text should be unchanged, got %q", got)
	}
}

func TestTrimToTokens_EnforcesBudget(t *testing.T) {
	// LLM 不一定遵守「不超过 N token」的指令，需要硬性兜底。
	// 截断后必须真的落在预算内（含追加的省略标记）。
	c := GetCounter()
	long := strings.Repeat("这是一段很长的中文摘要内容，需要被截断。", 200)
	const budget = 100
	got := c.TrimToTokens(long, budget)
	if got == long {
		t.Fatal("long text should be trimmed")
	}
	// 允许省略标记本身带来的少量超出，但不能失控
	if n := c.CountText(got); n > budget+20 {
		t.Errorf("trimmed text still too long: %d tokens (budget %d)", n, budget)
	}
}

func TestTrimToTokens_ZeroBudget(t *testing.T) {
	if got := GetCounter().TrimToTokens("abc", 0); got != "" {
		t.Errorf("zero budget should yield empty, got %q", got)
	}
}

// ============================================================
// 启发式估算（降级路径）
// ============================================================

func TestEstimateTokens_NeverZeroForNonEmpty(t *testing.T) {
	// 短字符串被估成 0 会让水位计算失真
	for _, s := range []string{"a", "中", "!", "ab"} {
		if estimateTokens(s) <= 0 {
			t.Errorf("estimate for %q should be > 0", s)
		}
	}
	if estimateTokens("") != 0 {
		t.Error("empty string should estimate 0")
	}
}

func TestEstimateTokens_CloseToTiktokenForChinese(t *testing.T) {
	c := GetCounter()
	if c.Degraded() {
		t.Skip("tiktoken unavailable, cannot compare")
	}
	s := strings.Repeat("这是中文测试文本", 50)
	exact := c.CountText(s)
	est := estimateTokens(s)
	// 启发式声明的误差是 ±20%，放宽到 ±40% 作为回归下限：
	// 超出说明系数明显跑偏，需要重新校准
	ratio := float64(est) / float64(exact)
	if ratio < 0.6 || ratio > 1.4 {
		t.Errorf("heuristic drifted too far: est=%d exact=%d ratio=%.2f", est, exact, ratio)
	}
}

func TestIsCJK(t *testing.T) {
	for _, r := range []rune{'中', '文', 'あ', 'ア', '한', '，', '。'} {
		if !isCJK(r) {
			t.Errorf("%c should be CJK", r)
		}
	}
	for _, r := range []rune{'a', 'Z', '1', ' ', '{'} {
		if isCJK(r) {
			t.Errorf("%c should not be CJK", r)
		}
	}
}
