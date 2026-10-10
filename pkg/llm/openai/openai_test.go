package openai

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// 真实调用测试：需要 OPENAI_API_KEY 环境变量，未设置则跳过。
// 默认指向腾讯混元 tokenhub 网关（hy3 模型），可用 env 覆盖。
func testClient(t *testing.T) *Client {
	t.Helper()
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set, skip real call")
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://openai.example.com/v1"
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "hy3"
	}
	return New(Config{BaseURL: baseURL, APIKey: apiKey, Model: model})
}

// TestStreamChat 验证流式文本：逐 token 回调、完整文本拼接一致。
func TestStreamChat(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "用一句话介绍你自己"},
	}

	var chunks []string
	var total int
	result, err := c.ChatWithToolsStream(ctx, messages, nil, "", llm.DefaultOptions(),
		func(d llm.StreamDelta) {
			chunks = append(chunks, d.Text)
			total += len(d.Text)
		})
	if err != nil {
		t.Fatalf("stream chat error: %v", err)
	}

	joined := strings.Join(chunks, "")
	if joined != result.Text {
		t.Fatalf("chunk 拼接 != 完整文本:\njoined=%q\nfull=%q", joined, result.Text)
	}
	if result.Text == "" {
		t.Fatalf("empty result text")
	}
	t.Logf("收到 %d 个 delta，累计 %d 字符", len(chunks), total)
	t.Logf("完整文本: %s", result.Text)
}

// TestStreamTools 验证流式工具调用：arguments 分片按 index 累积正确。
func TestStreamTools(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "帮我查一下北京的天气"},
	}
	tools := []llm.Tool{
		{
			Name:        "get_weather",
			Description: "查询指定城市的天气",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"city": map[string]interface{}{"type": "string", "description": "城市名"},
				},
				"required": []interface{}{"city"},
			},
		},
	}

	var chunks []string
	result, err := c.ChatWithToolsStream(ctx, messages, tools, llm.ToolChoiceAuto, llm.DefaultOptions(),
		func(d llm.StreamDelta) {
			if d.Text != "" {
				chunks = append(chunks, d.Text)
			}
		})
	if err != nil {
		t.Fatalf("stream tools error: %v", err)
	}

	if len(result.ToolCalls) == 0 {
		t.Fatalf("expected tool calls, got none (text=%q)", result.Text)
	}
	tc := result.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Fatalf("unexpected tool name: %s", tc.Name)
	}
	city, ok := tc.Arguments["city"].(string)
	if !ok || city == "" {
		t.Fatalf("arguments 未正确累积: %v", tc.Arguments)
	}
	t.Logf("工具调用: %s(%v)，文本增量 %d 段", tc.Name, tc.Arguments, len(chunks))
}

// TestNonStreamTools 验证非流式工具调用（对照组）。
func TestNonStreamTools(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "帮我查一下北京的天气"},
	}
	tools := []llm.Tool{
		{
			Name:        "get_weather",
			Description: "查询指定城市的天气",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"city": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"city"},
			},
		},
	}

	result, err := c.ChatWithTools(ctx, messages, tools, llm.ToolChoiceAuto, llm.DefaultOptions())
	if err != nil {
		t.Fatalf("non-stream tools error: %v", err)
	}
	if len(result.ToolCalls) == 0 {
		t.Fatalf("expected tool calls, got none (text=%q)", result.Text)
	}
	t.Logf("非流式工具调用: %s(%v)", result.ToolCalls[0].Name, result.ToolCalls[0].Arguments)
	if result.Usage.TotalTokens <= 0 {
		t.Logf("warning: usage 为 0（provider 未返回）")
	} else {
		t.Logf("usage: prompt=%d completion=%d total=%d",
			result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
	}
}

// TestMessagesToOpenAI 验证 tool_call_id 关联与消息转换（纯逻辑，无需网络）。
func TestMessagesToOpenAI(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "get_weather", Arguments: map[string]interface{}{"city": "北京"}},
			{Name: "get_weather", Arguments: map[string]interface{}{"city": "上海"}},
		}},
		{Role: llm.RoleTool, ToolName: "get_weather", Content: "晴"},
		{Role: llm.RoleTool, ToolName: "get_weather", Content: "雨"},
	}

	out := messagesToOpenAI(messages)
	if len(out) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(out))
	}
	// assistant 的 tool_calls 生成了两个 id
	assistant := out[2]
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool_calls, got %d", len(assistant.ToolCalls))
	}
	id0, id1 := assistant.ToolCalls[0].ID, assistant.ToolCalls[1].ID
	if id0 == "" || id1 == "" || id0 == id1 {
		t.Fatalf("tool_call id 异常: %q %q", id0, id1)
	}
	// tool 消息按序消费 id
	if out[3].ToolCallID != id0 {
		t.Fatalf("tool[0] id 不匹配: %q != %q", out[3].ToolCallID, id0)
	}
	if out[4].ToolCallID != id1 {
		t.Fatalf("tool[1] id 不匹配: %q != %q", out[4].ToolCallID, id1)
	}
	// arguments 序列化为 JSON 字符串
	if assistant.ToolCalls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("arguments 序列化异常: %q", assistant.ToolCalls[0].Function.Arguments)
	}
}

// TestOpenAIContent_Multimodal 验证多模态 content：无图保持纯字符串，有图转 text+image_url 数组。
func TestOpenAIContent_Multimodal(t *testing.T) {
	plain := openAIContent(llm.Message{Role: llm.RoleUser, Content: "hi"})
	if s, ok := plain.(string); !ok || s != "hi" {
		t.Fatalf("无图消息 content 应为纯字符串, got %#v", plain)
	}

	c := openAIContent(llm.Message{
		Role:    llm.RoleUser,
		Content: "看这张图",
		Images:  []llm.ImagePart{{MimeType: "image/png", Data: "aGVsbG8="}},
	})
	parts, ok := c.([]openAIContentPart)
	if !ok {
		t.Fatalf("有图消息 content 应为 []openAIContentPart, got %T", c)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "看这张图" {
		t.Fatalf("parts[0] 应为 text 块, got %#v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("parts[1] 应为 image_url 块, got %#v", parts[1])
	}
	if want := "data:image/png;base64,aGVsbG8="; parts[1].ImageURL.URL != want {
		t.Fatalf("image url = %q, want %q", parts[1].ImageURL.URL, want)
	}
}
