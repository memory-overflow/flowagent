// 系统提示词在 TurnLLMInput 里只存一份：SystemPrompt 字段存，messages 列表不重复。
//
// 真实调 LLM 时 system 走 messages.role=system（见 OpenAI provider），
// 落库快照是"事后"看的，不应该把同一个 system 在两处都铺一份，
// 否则调试页 dump JSON 时容易误以为"重复两遍"。

package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

func TestBuildLLMTurnInput_SystemPromptNotDuplicated(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "对话级任务规划器: .."},
		{Role: llm.RoleUser, Content: "你好"},
		{Role: llm.RoleAssistant, Content: "你好，请问需要什么帮助？"},
	}

	in := buildLLMTurnInput("openai:hy3", messages, nil, llm.ToolChoiceAuto, llm.DefaultOptions())

	// 1. SystemPrompt 字段必须有内容
	if in.SystemPrompt == "" {
		t.Fatalf("SystemPrompt 不应为空")
	}
	if in.SystemPrompt != "对话级任务规划器: .." {
		t.Fatalf("SystemPrompt 内容错乱: %q", in.SystemPrompt)
	}

	// 2. Messages 里不应包含 role=system 的消息
	for i, m := range in.Messages {
		if m.Role == string(llm.RoleSystem) {
			t.Fatalf("Messages[%d] 不应为 system role（应只存于 SystemPrompt 字段），m=%+v", i, m)
		}
	}

	// 3. Messages 起步应当是 user
	if len(in.Messages) < 1 || in.Messages[0].Role != string(llm.RoleUser) {
		t.Fatalf("Messages 应当从 user 起步，实际=%+v", in.Messages)
	}

	// 4. user / assistant 消息内容照搬，没被截掉
	if in.Messages[0].Content != "你好" {
		t.Fatalf("Messages[0].Content 错乱: %q", in.Messages[0].Content)
	}
	if in.Messages[1].Content != "你好，请问需要什么帮助？" {
		t.Fatalf("Messages[1].Content 错乱: %q", in.Messages[1].Content)
	}
}

func TestBuildLLMTurnInput_FirstSystemWins(t *testing.T) {
	// 多条 system 时（仅取首条到 SystemPrompt 字段，与真实 LLM 行为对齐）
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "第一条 system"},
		{Role: llm.RoleUser, Content: "u"},
		{Role: llm.RoleSystem, Content: "第二条 system（前端注入的 chunk）"},
	}
	in := buildLLMTurnInput("openai:hy3", messages, nil, llm.ToolChoiceAuto, llm.DefaultOptions())

	if in.SystemPrompt != "第一条 system" {
		t.Fatalf("应只取首条 system: %q", in.SystemPrompt)
	}
	// 第二条 role=system 仍进入 Messages（按 messagesToOpenAI 的语义，多条 system 在
	// provider 侧会顺序叠加；我们不让第一条 system 进 Messages，但如果之后又出现
	// 一条 system 仍然按 messages 顺序流入 —— 防止后续拼接时漏 system）。
	for _, m := range in.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "第一条") {
			t.Fatalf("第一条 system 不应进 Messages: m=%+v", m)
		}
	}
}

func TestBuildLLMTurnInput_NoSystemAtAll(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "u"},
		{Role: llm.RoleAssistant, Content: "a"},
	}
	in := buildLLMTurnInput("openai:hy3", messages, nil, llm.ToolChoiceAuto, llm.DefaultOptions())

	if in.SystemPrompt != "" {
		t.Fatalf("没有 system 时 SystemPrompt 应为空: %q", in.SystemPrompt)
	}
	if len(in.Messages) != 2 {
		t.Fatalf("Messages 应当原样保留 2 条: %+v", in.Messages)
	}
}
