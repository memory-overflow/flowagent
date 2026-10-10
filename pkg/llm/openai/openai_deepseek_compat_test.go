package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// 这组用例盯的是任务 4e3bdf04 的现象：deepseek 首轮正常（拿到 2 个工具调用），
// 第二轮把工具结果回放回去就 400（code 20024 invalid params），重试 3 次后任务判死。
// 逐字段实测的结论是「请求里只要出现 assistant 携带 tool_calls 就被拒」，
// 因此兼容档必须做两件事：tool_choice 恒为 auto、工具调用历史摊平成纯文本。

// TestDeepSeekCompatForcesAuto deepseek 不再首发 required，省掉那次必然被拒的往返。
func TestDeepSeekCompatForcesAuto(t *testing.T) {
	var choices []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		_ = json.Unmarshal(body, &req)
		tc, _ := req["tool_choice"].(string)
		choices = append(choices, tc)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":12}}`)
	}))
	defer srv.Close()
	// 清掉运行时探测缓存，确保「只发一次」是兼容档的功劳
	toolChoiceUnsupported.Delete(toolChoiceCacheKey(Config{BaseURL: srv.URL, Model: "deepseek/deepseek-v4-pro"}))

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "deepseek/deepseek-v4-pro"})
	if _, err := c.ChatWithTools(context.Background(),
		[]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		[]llm.Tool{{Name: "submit_plan"}}, llm.ToolChoiceAny, llm.DefaultOptions()); err != nil {
		t.Fatalf("deepseek 兼容档下不应报错: %v", err)
	}
	if len(choices) != 1 || choices[0] != "auto" {
		t.Fatalf("deepseek 应直接发一次 auto，实际 %v", choices)
	}
}

// TestDeepSeekCompatFlattensHistoryOnTheWire 线上报文里不能出现 tool_calls / tool 角色。
func TestDeepSeekCompatFlattensHistoryOnTheWire(t *testing.T) {
	var got openAIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[{"id":"c1","type":"function","function":{"name":"submit_plan","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":20}}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "DeepSeek-V4-Pro"})
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "帮我看看这个仓库"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "fetch__fetch", Arguments: map[string]interface{}{"url": "https://x"}},
			{Name: "fetch__fetch", Arguments: map[string]interface{}{"url": "https://y"}},
		}},
		{Role: llm.RoleTool, ToolName: "fetch__fetch", Content: "README 内容 A"},
		{Role: llm.RoleTool, ToolName: "fetch__fetch", Content: "README 内容 B"},
	}
	res, err := c.ChatWithTools(context.Background(), msgs,
		[]llm.Tool{{Name: "fetch__fetch"}}, llm.ToolChoiceAny, llm.DefaultOptions())
	if err != nil {
		t.Fatalf("摊平后应正常返回: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "submit_plan" {
		t.Fatalf("响应侧仍应能拿到原生 tool_calls: %+v", res.ToolCalls)
	}

	for _, m := range got.Messages {
		if len(m.ToolCalls) > 0 {
			t.Fatalf("报文里仍有 assistant 携带 tool_calls（deepseek 会 400）: %+v", m)
		}
		if m.Role == "tool" {
			t.Fatalf("报文里仍有 role=tool 的消息: %+v", m)
		}
	}
	// 摊平后：system + 一条合并的 user（原用户问题 + 调用记录）
	if len(got.Messages) != 2 || got.Messages[1].Role != "user" {
		t.Fatalf("期望摊平成 system + 1 条 user，实际 %d 条: %+v", len(got.Messages), got.Messages)
	}
	flat, _ := got.Messages[1].Content.(string)
	for _, want := range []string{"帮我看看这个仓库", "已完成的工具调用", "fetch__fetch", "README 内容 A", "README 内容 B"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("摊平文本缺少 %q: %s", want, flat)
		}
	}
	if !strings.Contains(flat, "不要重复调用") {
		t.Fatalf("摊平文本应提示模型不要重复调用: %s", flat)
	}
}

// TestFlattenToolHistory 摊平的边界：孤儿工具结果、无工具调用时保持原样。
func TestFlattenToolHistory(t *testing.T) {
	// 1. 没有工具调用的消息原样返回
	plain := []llm.Message{
		{Role: llm.RoleUser, Content: "a"},
		{Role: llm.RoleAssistant, Content: "b"},
	}
	got := flattenToolHistory(plain)
	if len(got) != 2 || got[0].Content != "a" || got[1].Content != "b" {
		t.Fatalf("无工具调用时不应改写消息: %+v", got)
	}

	// 2. 调用多于结果 / 结果多于调用都要保住内容
	got = flattenToolHistory([]llm.Message{
		{Role: llm.RoleUser, Content: "u"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "t1", Arguments: map[string]interface{}{"k": "v"}},
			{Name: "t2", Arguments: map[string]interface{}{}},
		}},
		{Role: llm.RoleTool, ToolName: "t1", Content: "r1"},
	})
	if len(got) != 1 || got[0].Role != llm.RoleUser {
		t.Fatalf("期望合并为一条 user: %+v", got)
	}
	if !strings.Contains(got[0].Content, "r1") || !strings.Contains(got[0].Content, "（无结果）") {
		t.Fatalf("缺结果的调用要显式标注，实际: %s", got[0].Content)
	}

	// 3. 孤儿工具结果不能丢
	got = flattenToolHistory([]llm.Message{
		{Role: llm.RoleUser, Content: "u"},
		{Role: llm.RoleAssistant, Content: "thinking"},
		{Role: llm.RoleTool, ToolName: "t", Content: "orphan"},
	})
	if len(got) != 3 || !strings.Contains(got[2].Content, "orphan") {
		t.Fatalf("孤儿工具结果应转成文本保留: %+v", got)
	}
}

// TestDeepSeekCompatModelMatch 兼容档按模型名命中，不串到别的模型。
func TestDeepSeekCompatModelMatch(t *testing.T) {
	cases := map[string]bool{
		"deepseek/deepseek-v4-pro": true,
		"DeepSeek-V3":              true,
		"deepseek-chat":            true,
		"hy4-preview":              false,
		"gpt-5.6-sol":              false,
	}
	for model, want := range cases {
		if got := deepSeekCompat(model); got != want {
			t.Fatalf("deepSeekCompat(%q) = %v, want %v", model, got, want)
		}
	}
}
