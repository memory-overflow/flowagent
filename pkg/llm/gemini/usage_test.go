package gemini

import "testing"

// Gemini 流式响应的 usageMetadata 是「截至当前的累计值」，
// 多个 chunk 都带时必须取最后一个而非累加 —— 累加会让 token 统计翻倍。
func TestParseChunksToResult_UsageIsCumulativeNotSummed(t *testing.T) {
	body := []byte(`[
	  {"candidates":[{"content":{"parts":[{"text":"hel"}]}}],
	   "usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"totalTokenCount":105}},
	  {"candidates":[{"content":{"parts":[{"text":"lo"}]}}],
	   "usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":12,"totalTokenCount":112}}
	]`)

	result, err := parseChunksToResult(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Text != "hello" {
		t.Errorf("text = %q, want %q", result.Text, "hello")
	}
	// 取最后一个 chunk 的值，不是 105+112
	if result.Usage.TotalTokens != 112 {
		t.Errorf("total = %d, want 112 (last chunk, not summed)", result.Usage.TotalTokens)
	}
	if result.Usage.PromptTokens != 100 {
		t.Errorf("prompt = %d, want 100", result.Usage.PromptTokens)
	}
	if result.Usage.CompletionTokens != 12 {
		t.Errorf("completion = %d, want 12", result.Usage.CompletionTokens)
	}
}

// 尾部 chunk 不带 usage 时，不能把已解析到的有效值清零
func TestParseChunksToResult_TrailingChunkWithoutUsage(t *testing.T) {
	body := []byte(`[
	  {"candidates":[{"content":{"parts":[{"text":"a"}]}}],
	   "usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":3,"totalTokenCount":53}},
	  {"candidates":[{"content":{"parts":[{"text":"b"}]}}]}
	]`)

	result, err := parseChunksToResult(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Usage.TotalTokens != 53 {
		t.Errorf("total = %d, want 53 (kept from earlier chunk)", result.Usage.TotalTokens)
	}
}

// 思考 token 计入输出侧，保证 prompt + completion == total
func TestParseChunksToResult_ThoughtsTokensCountedAsOutput(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]}}],
	  "usageMetadata":{"promptTokenCount":80,"candidatesTokenCount":20,
	                   "thoughtsTokenCount":30,"totalTokenCount":130}}`)

	result, err := parseChunksToResult(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Usage.CompletionTokens != 50 { // 20 + 30
		t.Errorf("completion = %d, want 50 (candidates + thoughts)", result.Usage.CompletionTokens)
	}
	if got := result.Usage.PromptTokens + result.Usage.CompletionTokens; got != result.Usage.TotalTokens {
		t.Errorf("prompt+completion = %d, want == total %d", got, result.Usage.TotalTokens)
	}
}

// provider 不返回 usage 时应为零值，不报错
func TestParseChunksToResult_NoUsage(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)

	result, err := parseChunksToResult(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Usage.TotalTokens != 0 {
		t.Errorf("total = %d, want 0", result.Usage.TotalTokens)
	}
	if result.Text != "hi" {
		t.Errorf("text = %q, want %q", result.Text, "hi")
	}
}

// 同时返回文本和工具调用时两者都要保留
func TestParseChunksToResult_TextAndToolCallCoexist(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[
	    {"text":"让我查一下"},
	    {"functionCall":{"name":"search","args":{"q":"go"}}}
	  ]}}],
	  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":8,"totalTokenCount":18}}`)

	result, err := parseChunksToResult(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Text != "让我查一下" {
		t.Errorf("text = %q", result.Text)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "search" {
		t.Errorf("tool calls = %+v, want 1 call named search", result.ToolCalls)
	}
	if result.Usage.TotalTokens != 18 {
		t.Errorf("total = %d, want 18", result.Usage.TotalTokens)
	}
}
