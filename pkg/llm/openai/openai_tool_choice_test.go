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

// 这两个用例盯的是同一类线上事故：网关拒绝了我们的请求参数（tool_choice），
// 而 openai client 把 400 的 JSON 错误体当成 SSE 解析 —— 解析不到任何 data 行，
// 于是返回「空结果 + nil error」。上层据此把 turn 记为成功（output={}、token 全 0），
// 故障完全不可见（task 869d9dd5：deepseek 规划期静默空返回）。

// sseStream 构造一段最小可用的流式响应（含 usage chunk + [DONE]）。
func sseStream(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	write := func(s string) {
		_, _ = io.WriteString(w, s)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]interface{}{"content": content}},
		},
	}
	b, _ := json.Marshal(chunk)
	write("data: " + string(b) + "\n\n")
	write("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
	write("data: [DONE]\n\n")
}

// TestStreamNonStatusOK_SurfacesError 流式下网关返回 400 必须报错，不能静默空成功。
func TestStreamNonStatusOK_SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid params","code":"20024"}}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	tools := []llm.Tool{{Name: "submit_plan"}}
	res, err := c.ChatWithToolsStream(context.Background(),
		[]llm.Message{{Role: llm.RoleUser, Content: "hi"}}, tools, llm.ToolChoiceNone, llm.DefaultOptions(),
		func(llm.StreamDelta) {})
	if err == nil {
		t.Fatalf("流式 400 必须返回错误，实际拿到结果: %+v", res)
	}
	// 错误里要带上状态码与网关原文，否则排查时只能看到「空结果」
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid params") {
		t.Fatalf("错误信息缺少状态码/网关原文: %v", err)
	}
}

// TestToolChoiceRequiredDowngrade required 被拒时降级 auto 重试，并记住结论。
//
// 模型名刻意不含 deepseek：名字带 deepseek 的模型走兼容档直接发 auto
// （见 TestDeepSeekCompatForcesAutoAndFlattensHistory），不会首发 required。
func TestToolChoiceRequiredDowngrade(t *testing.T) {
	const model = "strict-gateway-model"
	var choices []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		_ = json.Unmarshal(body, &req)
		tc, _ := req["tool_choice"].(string)
		choices = append(choices, tc)

		if tc == "required" {
			// 模拟 lkeap deepseek：只认 auto
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"id":"x","error":{"message":"invalid params","code":"20024"}}`)
			return
		}
		if stream, _ := req["stream"].(bool); stream {
			sseStream(t, w, "ok")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer srv.Close()
	// 每个用例独立：清掉进程级缓存，避免用例之间互相影响
	toolChoiceUnsupported.Delete(toolChoiceCacheKey(Config{BaseURL: srv.URL, Model: model}))

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: model})
	tools := []llm.Tool{{Name: "submit_plan"}}
	ctx := context.Background()
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "hi"}}

	// 第一次：required 被拒 → 自动降级 auto 并成功
	res, err := c.ChatWithTools(ctx, msgs, tools, llm.ToolChoiceAny, llm.DefaultOptions())
	if err != nil {
		t.Fatalf("required 被拒后应降级重试成功，实际报错: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("降级重试结果异常: %q", res.Text)
	}
	if len(choices) != 2 || choices[0] != "required" || choices[1] != "auto" {
		t.Fatalf("期望 [required, auto]，实际 %v", choices)
	}

	// 第二次：命中缓存，直接发 auto（不再浪费一次 400）
	choices = nil
	if _, err := c.ChatWithTools(ctx, msgs, tools, llm.ToolChoiceAny, llm.DefaultOptions()); err != nil {
		t.Fatalf("命中缓存后不应报错: %v", err)
	}
	if len(choices) != 1 || choices[0] != "auto" {
		t.Fatalf("缓存命中后应只发一次 auto，实际 %v", choices)
	}

	// 不支持的模型不该影响其他模型：换 model 仍以 required 首发
	other := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "hy4-preview"})
	choices = nil
	if _, err := other.ChatWithTools(ctx, msgs, tools, llm.ToolChoiceAny, llm.DefaultOptions()); err != nil {
		t.Fatalf("其他模型不应受影响: %v", err)
	}
	if len(choices) != 2 || choices[0] != "required" {
		t.Fatalf("其他模型应仍首发 required，实际 %v", choices)
	}
	toolChoiceUnsupported.Delete(toolChoiceCacheKey(Config{BaseURL: srv.URL, Model: "hy4-preview"}))

	// 流式路径同样要降级（规划期正是流式调用）
	streamModel := "stream-strict-model"
	toolChoiceUnsupported.Delete(toolChoiceCacheKey(Config{BaseURL: srv.URL, Model: streamModel}))
	sc := New(Config{BaseURL: srv.URL, APIKey: "k", Model: streamModel})
	choices = nil
	res2, err := sc.ChatWithToolsStream(ctx, msgs, tools, llm.ToolChoiceAny, llm.DefaultOptions(),
		func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("流式下 required 被拒后应降级重试成功，实际报错: %v", err)
	}
	if res2.Text != "ok" {
		t.Fatalf("流式降级重试结果异常: %q", res2.Text)
	}
	if len(choices) != 2 || choices[0] != "required" || choices[1] != "auto" {
		t.Fatalf("流式下期望 [required, auto]，实际 %v", choices)
	}
	toolChoiceUnsupported.Delete(toolChoiceCacheKey(Config{BaseURL: srv.URL, Model: streamModel}))
}
