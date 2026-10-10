package claude

import (
	"reflect"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestMessagesToAnthropic_SystemAndBasic 验证 system 合并到顶层字段、user/assistant 交替正确。
func TestMessagesToAnthropic_SystemAndBasic(t *testing.T) {
	system, msgs := messagesToAnthropic([]llm.Message{
		{Role: llm.RoleSystem, Content: "s1"},
		{Role: llm.RoleSystem, Content: "s2"},
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	})

	if system != "s1\n\ns2" {
		t.Errorf("system = %q, want %q", system, "s1\n\ns2")
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hi" {
		t.Errorf("msgs[0] = %+v, want user/hi", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "hello" {
		t.Errorf("msgs[1] = %+v, want assistant/hello", msgs[1])
	}
}

// TestMessagesToAnthropic_ToolRoundTrip 验证 tool_use → tool_result 配对：
// assistant 的工具调用生成 toolu_<seq> id，后续 tool 消息按 name 引用正确的 id，
// 并行调用的多个结果合并进同一个 user 消息。
func TestMessagesToAnthropic_ToolRoundTrip(t *testing.T) {
	_, msgs := messagesToAnthropic([]llm.Message{
		{Role: llm.RoleUser, Content: "查天气和查股票"},
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{Name: "get_weather", Arguments: map[string]interface{}{"city": "深圳"}},
				{Name: "get_stock", Arguments: map[string]interface{}{"code": "00700"}},
			},
		},
		{Role: llm.RoleTool, ToolName: "get_weather", Content: "晴"},
		{Role: llm.RoleTool, ToolName: "get_stock", Content: "涨"},
	})

	// [user, assistant(tool_use x2), user(tool_result x2)]
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3", len(msgs))
	}

	// assistant 消息：content 是两个 tool_use 块
	assistantBlocks, ok := msgs[1].Content.([]claudeContentBlock)
	if !ok || len(assistantBlocks) != 2 {
		t.Fatalf("msgs[1].Content = %#v, want 2 tool_use blocks", msgs[1].Content)
	}
	if assistantBlocks[0].Type != "tool_use" || assistantBlocks[0].Name != "get_weather" {
		t.Errorf("assistantBlocks[0] = %+v, want tool_use get_weather", assistantBlocks[0])
	}
	if assistantBlocks[1].Type != "tool_use" || assistantBlocks[1].Name != "get_stock" {
		t.Errorf("assistantBlocks[1] = %+v, want tool_use get_stock", assistantBlocks[1])
	}
	// 两个 id 必须不同且非空
	if assistantBlocks[0].ID == "" || assistantBlocks[0].ID == assistantBlocks[1].ID {
		t.Errorf("tool_use ids = %q / %q, want distinct non-empty", assistantBlocks[0].ID, assistantBlocks[1].ID)
	}

	// 第三个消息：合并的 tool_result 块，且 tool_use_id 回指正确
	resultBlocks, ok := msgs[2].Content.([]claudeContentBlock)
	if !ok || len(resultBlocks) != 2 {
		t.Fatalf("msgs[2].Content = %#v, want 2 tool_result blocks", msgs[2].Content)
	}
	if msgs[2].Role != "user" {
		t.Errorf("msgs[2].Role = %q, want user", msgs[2].Role)
	}
	if resultBlocks[0].Type != "tool_result" || resultBlocks[0].ToolUseID != assistantBlocks[0].ID || resultBlocks[0].Content != "晴" {
		t.Errorf("resultBlocks[0] = %+v, want tool_result id=%q content=晴", resultBlocks[0], assistantBlocks[0].ID)
	}
	if resultBlocks[1].Type != "tool_result" || resultBlocks[1].ToolUseID != assistantBlocks[1].ID || resultBlocks[1].Content != "涨" {
		t.Errorf("resultBlocks[1] = %+v, want tool_result id=%q content=涨", resultBlocks[1], assistantBlocks[1].ID)
	}
}

// TestMessagesToAnthropic_NilArgs 验证 nil 参数被归一为空 map，避免 tool_use.input 序列化成 null。
func TestMessagesToAnthropic_NilArgs(t *testing.T) {
	_, msgs := messagesToAnthropic([]llm.Message{
		{Role: llm.RoleUser, Content: "x"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "f", Arguments: nil}}},
		{Role: llm.RoleTool, ToolName: "f", Content: "ok"},
	})

	blocks, ok := msgs[1].Content.([]claudeContentBlock)
	if !ok || len(blocks) != 1 {
		t.Fatalf("msgs[1].Content = %#v", msgs[1].Content)
	}
	if blocks[0].Input == nil {
		t.Errorf("blocks[0].Input = nil, want empty map")
	}
	if !reflect.DeepEqual(blocks[0].Input, map[string]interface{}{}) {
		t.Errorf("blocks[0].Input = %#v, want empty map", blocks[0].Input)
	}
}

// TestMessagesToAnthropic_AssistantTextWithTools 验证 assistant 同时带解释文本和工具调用时，
// 文本作为首个 text 块放在 tool_use 之前。
func TestMessagesToAnthropic_AssistantTextWithTools(t *testing.T) {
	_, msgs := messagesToAnthropic([]llm.Message{
		{Role: llm.RoleUser, Content: "x"},
		{
			Role:    llm.RoleAssistant,
			Content: "我来查一下",
			ToolCalls: []llm.ToolCall{
				{Name: "f", Arguments: map[string]interface{}{"a": 1}},
			},
		},
		{Role: llm.RoleTool, ToolName: "f", Content: "ok"},
	})

	blocks, ok := msgs[1].Content.([]claudeContentBlock)
	if !ok || len(blocks) != 2 {
		t.Fatalf("msgs[1].Content = %#v, want [text, tool_use]", msgs[1].Content)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "我来查一下" {
		t.Errorf("blocks[0] = %+v, want text 我来查一下", blocks[0])
	}
	if blocks[1].Type != "tool_use" {
		t.Errorf("blocks[1] = %+v, want tool_use", blocks[1])
	}
}

// TestClaudeUserContent_Multimodal 验证多模态 content：无图保持纯字符串，有图转 text+image 块数组。
func TestClaudeUserContent_Multimodal(t *testing.T) {
	plain := claudeUserContent(llm.Message{Role: llm.RoleUser, Content: "hi"})
	if s, ok := plain.(string); !ok || s != "hi" {
		t.Fatalf("无图 content 应为 string, got %#v", plain)
	}

	c := claudeUserContent(llm.Message{
		Role:    llm.RoleUser,
		Content: "看这张图",
		Images:  []llm.ImagePart{{MimeType: "image/png", Data: "aGVsbG8="}},
	})
	blocks, ok := c.([]claudeContentBlock)
	if !ok || len(blocks) != 2 {
		t.Fatalf("有图 content 应为 2 个 block, got %#v", c)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "看这张图" {
		t.Fatalf("blocks[0] 异常: %#v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Source == nil {
		t.Fatalf("blocks[1] 应为 image 块: %#v", blocks[1])
	}
	src := blocks[1].Source
	if src.Type != "base64" || src.MediaType != "image/png" || src.Data != "aGVsbG8=" {
		t.Fatalf("source 异常: %#v", src)
	}
}
