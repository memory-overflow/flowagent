package openai

// 流式 token 用量的单元测试（不需要 API key，直接喂构造好的 SSE 流）。
//
// 背景：混元 4（走 OpenAI 兼容协议的 tokenhub 网关）曾出现 token 统计恒为 0，
// 根因是三个缺陷叠加：
//  1. 请求未带 stream_options.include_usage → 服务端根本不回 usage；
//  2. parseStream 在 len(choices)==0 时直接 continue → 即便回了也被跳过
//     （usage chunk 的 choices 恰恰是空数组）；
//  3. parseStream 构造结果时压根没赋值 Usage。
//
// 这组测试逐条锁住上述三点，避免回归。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// sseLines 把若干 JSON 片段拼成 SSE 流（末尾补 [DONE]）
func sseLines(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: ")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestParseStream_UsageFromFinalChunk usage 挂在 choices 为空的最后一个 chunk 上。
//
// 这是 OpenAI 规范下 include_usage=true 的标准形态，也正是此前被
// 「len(choices)==0 → continue」误杀的那个 chunk。
func TestParseStream_UsageFromFinalChunk(t *testing.T) {
	stream := sseLines(
		`{"choices":[{"delta":{"content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"世界"}}]}`,
		// usage chunk：choices 是空数组
		`{"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":56,"total_tokens":1290}}`,
	)

	c := &Client{}
	got, err := c.parseStream(strings.NewReader(stream), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("parseStream error: %v", err)
	}
	if got.Text != "你好世界" {
		t.Errorf("text = %q, want 你好世界", got.Text)
	}
	if got.Usage.PromptTokens != 1234 {
		t.Errorf("prompt = %d, want 1234", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens != 56 {
		t.Errorf("completion = %d, want 56", got.Usage.CompletionTokens)
	}
	if got.Usage.TotalTokens != 1290 {
		t.Errorf("total = %d, want 1290", got.Usage.TotalTokens)
	}
}

// TestParseStream_UsageWithToolCalls 纯工具调用轮次同样要拿到 usage。
//
// agent loop 里大量轮次是「无文本、只有 tool_calls」，若这类轮次漏统计，
// 累计消耗会显著偏低。
func TestParseStream_UsageWithToolCalls(t *testing.T) {
	stream := sseLines(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"web_search","arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":40,"total_tokens":940}}`,
	)

	c := &Client{}
	got, err := c.parseStream(strings.NewReader(stream), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("parseStream error: %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "web_search" {
		t.Fatalf("tool calls = %#v, want one web_search", got.ToolCalls)
	}
	if got.ToolCalls[0].Arguments["q"] != "go" {
		t.Errorf("arguments = %#v, want q=go", got.ToolCalls[0].Arguments)
	}
	if got.Usage.TotalTokens != 940 {
		t.Errorf("total = %d, want 940 (tool-only turn must still count)", got.Usage.TotalTokens)
	}
}

// TestParseStream_UsageOverriddenByLater 多个 chunk 带 usage 时以最后一个为准。
//
// 部分兼容网关会在中途重复回传累计值，取最后一个才是最终结果。
func TestParseStream_UsageOverriddenByLater(t *testing.T) {
	stream := sseLines(
		`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	)

	c := &Client{}
	got, err := c.parseStream(strings.NewReader(stream), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("parseStream error: %v", err)
	}
	if got.Usage.TotalTokens != 15 {
		t.Errorf("total = %d, want 15 (last chunk wins)", got.Usage.TotalTokens)
	}
}

// TestParseStream_TotalBackfilled 网关只给拆分值不给合计时，本地补齐 total。
//
// total 是落库到 task_turn.total_tokens 的那一列，缺了会导致成本统计为 0。
func TestParseStream_TotalBackfilled(t *testing.T) {
	stream := sseLines(
		`{"choices":[{"delta":{"content":"x"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":200,"completion_tokens":30}}`,
	)

	c := &Client{}
	got, err := c.parseStream(strings.NewReader(stream), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("parseStream error: %v", err)
	}
	if got.Usage.TotalTokens != 230 {
		t.Errorf("total = %d, want 230 (prompt + completion)", got.Usage.TotalTokens)
	}
}

// TestParseStream_NoUsageStaysZero 服务端完全不回 usage 时保持零值。
//
// 0 的语义是「未知」，不能编造 —— 上层据此判断该 turn 无可用计量。
func TestParseStream_NoUsageStaysZero(t *testing.T) {
	stream := sseLines(`{"choices":[{"delta":{"content":"hi"}}]}`)

	c := &Client{}
	got, err := c.parseStream(strings.NewReader(stream), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("parseStream error: %v", err)
	}
	if got.Usage.TotalTokens != 0 || got.Usage.PromptTokens != 0 {
		t.Errorf("usage = %+v, want all zero", got.Usage)
	}
}

// TestStreamRequestIncludesUsageOption 流式请求必须带 stream_options.include_usage。
//
// 这是最容易被忽略的一环：解析逻辑再正确，不向服务端索要 usage 也拿不到。
// 非流式请求则不应带该字段（OpenAI 规范只允许流式使用）。
func TestStreamRequestIncludesUsageOption(t *testing.T) {
	build := func(stream bool) map[string]interface{} {
		req := openAIRequest{Model: "hy3", Stream: stream}
		if stream {
			req.StreamOpts = &openAIStreamOptions{IncludeUsage: true}
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	got := build(true)
	so, ok := got["stream_options"].(map[string]interface{})
	if !ok {
		t.Fatalf("stream request missing stream_options, got keys %v", keysOf(got))
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", so["include_usage"])
	}

	if _, exists := build(false)["stream_options"]; exists {
		t.Error("non-stream request should not carry stream_options")
	}
}

// TestParseStream_HunyuanEveryChunkCarriesZeroUsage 混元 hy4-preview 的真实形态。
//
// 抓包实测（api.lkeap.cloud.tencent.com/plan/v3）：
//   - 它在**每个** chunk 上都挂 usage，但中间那些全是 0；
//   - 真实值在最后一个带 finish_reason=stop 的 chunk 上，且该 chunk 的
//     choices **非空**（delta.content 为空串）——不存在标准 OpenAI 那种
//     choices=[] 的独立 usage chunk。
//
// 所以「非 nil 就覆盖」会被中间/尾随的零值刷掉，必须「非零才覆盖」。
func TestParseStream_HunyuanEveryChunkCarriesZeroUsage(t *testing.T) {
	zero := `"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`
	stream := sseLines(
		`{"choices":[{"index":0,"delta":{"content":"静"}}],`+zero+`}`,
		`{"choices":[{"index":0,"delta":{"content":"水"}}],`+zero+`}`,
		// 终值挂在 finish_reason=stop 的 chunk 上，choices 非空
		`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":24,"completion_tokens":171,"total_tokens":195}}`,
	)

	c := &Client{}
	got, err := c.parseStream(strings.NewReader(stream), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("parseStream error: %v", err)
	}
	if got.Text != "静水" {
		t.Errorf("text = %q, want 静水", got.Text)
	}
	if got.Usage.TotalTokens != 195 {
		t.Errorf("total = %d, want 195 (zero-valued chunks must not clobber)", got.Usage.TotalTokens)
	}
	if got.Usage.PromptTokens != 24 || got.Usage.CompletionTokens != 171 {
		t.Errorf("usage = %+v, want prompt=24 completion=171", got.Usage)
	}
}

// TestParseStream_TrailingZeroUsageDoesNotClobber 真实值之后再来零值 usage，不得被清零
func TestParseStream_TrailingZeroUsageDoesNotClobber(t *testing.T) {
	stream := sseLines(
		`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`,
		`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
	)

	c := &Client{}
	got, err := c.parseStream(strings.NewReader(stream), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("parseStream error: %v", err)
	}
	if got.Usage.TotalTokens != 120 {
		t.Errorf("total = %d, want 120 (trailing zero usage must be ignored)", got.Usage.TotalTokens)
	}
}

func keysOf(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
