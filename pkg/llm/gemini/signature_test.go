package gemini

import (
	"encoding/json"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// buildContentsForTest 走真实的消息转换路径
func buildContentsForTest(t *testing.T, msgs []llm.Message) []geminiContent {
	t.Helper()
	_, contents := messagesToContents(msgs)
	return contents
}

// TestParseThoughtSignature 响应里的 thoughtSignature 必须解析到对应的 ToolCall
func TestParseThoughtSignature(t *testing.T) {
	body := `[{"candidates":[{"content":{"role":"model","parts":[
      {"functionCall":{"name":"nav","args":{"url":"https://a.com"}},"thoughtSignature":"SIG_A"},
      {"functionCall":{"name":"click","args":{"ref":"e1"}}}
    ]}}]}]`

	result, err := parseChunksToResult([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(result.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Signature != "SIG_A" {
		t.Errorf("first call signature = %q, want SIG_A", result.ToolCalls[0].Signature)
	}
	// 并行调用的后续 part 本就没有签名，不能凭空造出来
	if result.ToolCalls[1].Signature != "" {
		t.Errorf("second call should have empty signature, got %q", result.ToolCalls[1].Signature)
	}
}

// TestSignatureRoundTrip 签名要原样回到它所属的那个 functionCall part
func TestSignatureRoundTrip(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "open baidu"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "nav", Arguments: map[string]interface{}{"url": "https://baidu.com"}, Signature: "SIG_A"},
			{Name: "snapshot", Signature: ""},
		}},
		{Role: llm.RoleTool, ToolName: "nav", Content: "ok"},
	}

	contents := buildContentsForTest(t, msgs)

	// contents[1] 应是 model 消息，含两个 functionCall part
	model1 := contents[1]
	if model1.Role != "model" {
		t.Fatalf("contents[1].role = %q, want model", model1.Role)
	}
	if len(model1.Parts) != 2 {
		t.Fatalf("expected 2 function call parts, got %d", len(model1.Parts))
	}
	if model1.Parts[0].ThoughtSignature != "SIG_A" {
		t.Errorf("part[0] signature = %q, want SIG_A", model1.Parts[0].ThoughtSignature)
	}
	// 并行调用的第二个 part 不应被塞入 dummy 签名
	if model1.Parts[1].ThoughtSignature != "" {
		t.Errorf("part[1] should stay empty, got %q", model1.Parts[1].ThoughtSignature)
	}
}

// TestDummySignatureForLegacyHistory 历史 turn 没有签名时，
// 首个 functionCall 要用 dummy 兜底，否则旧会话续跑会 400
func TestDummySignatureForLegacyHistory(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		// 模拟本次改动前落库的历史：没有 Signature
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "nav", Arguments: map[string]interface{}{"url": "x"}},
			{Name: "click"},
		}},
		{Role: llm.RoleTool, ToolName: "nav", Content: "ok"},
	}

	contents := buildContentsForTest(t, msgs)
	parts := contents[1].Parts

	if parts[0].ThoughtSignature != dummyThoughtSignature {
		t.Errorf("first part should fall back to dummy, got %q", parts[0].ThoughtSignature)
	}
	if parts[1].ThoughtSignature != "" {
		t.Errorf("non-first part must not get dummy, got %q", parts[1].ThoughtSignature)
	}
}

// TestParallelFunctionResponsesMerged 并行调用的多个 functionResponse
// 必须合并进同一个 user content —— 交错结构会被 Gemini 判 400
func TestParallelFunctionResponsesMerged(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "do two things"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "t1", Signature: "SIG"},
			{Name: "t2"},
		}},
		{Role: llm.RoleTool, ToolName: "t1", Content: "r1"},
		{Role: llm.RoleTool, ToolName: "t2", Content: "r2"},
	}

	contents := buildContentsForTest(t, msgs)

	// 期望结构：user(text) → model(FC1,FC2) → user(FR1,FR2)，共 3 段
	if len(contents) != 3 {
		var roles []string
		for _, c := range contents {
			roles = append(roles, c.Role)
		}
		t.Fatalf("expected 3 contents (user/model/user), got %d: %v", len(contents), roles)
	}
	last := contents[2]
	if last.Role != "user" || len(last.Parts) != 2 {
		t.Fatalf("tool results should merge into one user content with 2 parts, got role=%s parts=%d",
			last.Role, len(last.Parts))
	}
	for i, p := range last.Parts {
		if p.FunctionResponse == nil {
			t.Errorf("part[%d] should be functionResponse", i)
		}
	}
	if last.Parts[0].FunctionResponse.Name != "t1" || last.Parts[1].FunctionResponse.Name != "t2" {
		t.Errorf("function response order broken: %s, %s",
			last.Parts[0].FunctionResponse.Name, last.Parts[1].FunctionResponse.Name)
	}
}

// TestToolResultNotMergedIntoUserText 工具结果不能被塞进真实用户输入的消息里
func TestToolResultNotMergedIntoUserText(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "real user input"},
		{Role: llm.RoleTool, ToolName: "t1", Content: "r1"},
	}
	contents := buildContentsForTest(t, msgs)
	if len(contents) != 2 {
		t.Fatalf("expected 2 separate contents, got %d", len(contents))
	}
	if contents[0].Parts[0].Text != "real user input" {
		t.Errorf("user text content polluted: %+v", contents[0].Parts)
	}
}

// TestIsFunctionResponseContent 合并判定的边界
func TestIsFunctionResponseContent(t *testing.T) {
	fr := &geminiFunctionResponse{Name: "t"}
	cases := []struct {
		name string
		c    geminiContent
		want bool
	}{
		{"pure function responses", geminiContent{Role: "user", Parts: []geminiPart{{FunctionResponse: fr}}}, true},
		{"user text", geminiContent{Role: "user", Parts: []geminiPart{{Text: "hi"}}}, false},
		{"model role", geminiContent{Role: "model", Parts: []geminiPart{{FunctionResponse: fr}}}, false},
		{"empty parts", geminiContent{Role: "user"}, false},
		{"mixed", geminiContent{Role: "user", Parts: []geminiPart{{FunctionResponse: fr}, {Text: "x"}}}, false},
	}
	for _, c := range cases {
		if got := isFunctionResponseContent(c.c); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestSignatureSerializedAsCamelCase Gemini REST 要求字段名为 thoughtSignature
func TestSignatureSerializedAsCamelCase(t *testing.T) {
	b, err := json.Marshal(geminiPart{
		FunctionCall:     &geminiFunctionCall{Name: "n"},
		ThoughtSignature: "S",
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	if _, ok := m["thoughtSignature"]; !ok {
		t.Errorf("field must serialize as thoughtSignature, got %s", b)
	}
	// 空签名不应出现在 payload 里（并行调用的后续 part）
	b2, _ := json.Marshal(geminiPart{FunctionCall: &geminiFunctionCall{Name: "n"}})
	var m2 map[string]interface{}
	json.Unmarshal(b2, &m2)
	if _, ok := m2["thoughtSignature"]; ok {
		t.Errorf("empty signature should be omitted, got %s", b2)
	}
}
