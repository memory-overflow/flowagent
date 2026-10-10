package azureopenai

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

// ============================================================
// endpoint 构造
// ============================================================

// TestEndpoint URL 必须是 <base>/deployments/<model>/chat/completions?api-version=<ver>。
// 与 openai provider 的关键差异：模型名在路径里，且强制带 api-version。
func TestEndpoint(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"standard",
			Config{
				BaseURL:    "https://gateway.example.com/llm-service/azure/public/openai",
				Model:      "gpt-5.6-sol",
				APIVersion: "2025-01-01-preview",
			},
			"https://gateway.example.com/llm-service/azure/public/openai/deployments/gpt-5.6-sol/chat/completions?api-version=2025-01-01-preview",
		},
		{
			// 配错尾斜杠很常见，不能拼出 //deployments
			"trailing slash trimmed",
			Config{
				BaseURL:    "https://host/openai/",
				Model:      "gpt-5.6-terra",
				APIVersion: "2025-01-01-preview",
			},
			"https://host/openai/deployments/gpt-5.6-terra/chat/completions?api-version=2025-01-01-preview",
		},
	}
	for _, c := range cases {
		got := New(c.cfg).endpoint()
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// TestNew_DefaultAPIVersion 未配置 api_version 时必须回落到默认值，
// 否则拼出的 URL 会带 api-version= 空值而被网关拒绝。
func TestNew_DefaultAPIVersion(t *testing.T) {
	c := New(Config{BaseURL: "https://h/openai", Model: "gpt-5.6-luna"})
	if !strings.Contains(c.endpoint(), "api-version="+DefaultAPIVersion) {
		t.Errorf("want default api-version in endpoint, got %s", c.endpoint())
	}
}

func TestName(t *testing.T) {
	if got := New(Config{Model: "gpt-5.6-sol"}).Name(); got != "azure:gpt-5.6-sol" {
		t.Errorf("Name() = %q, want azure:gpt-5.6-sol", got)
	}
}

// ============================================================
// 请求体裁剪 —— 本 provider 的核心约束
// ============================================================

// TestBuildRequest_DropsSamplingParams gpt-5.6 系列对 temperature / top_p 报 400，
// 因此请求体里绝不能出现这两个字段。
//
// 这是本 provider 存在的主要理由之一 —— 上层统一用 llm.DefaultOptions()
// （temperature=0.7 / top_p=1.0），若原样下发则每一次调用都会失败。
func TestBuildRequest_DropsSamplingParams(t *testing.T) {
	req := buildRequest(
		[]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		nil, llm.ToolChoiceAuto,
		llm.Options{Temperature: 0.7, TopP: 1.0, TopK: 32, MaxTokens: 100},
		false,
	)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	for _, banned := range []string{"temperature", "top_p", "top_k"} {
		if strings.Contains(body, banned) {
			t.Errorf("request body must not contain %q, got: %s", banned, body)
		}
	}
	// 输出上限必须走新字段名
	if !strings.Contains(body, `"max_completion_tokens":100`) {
		t.Errorf("want max_completion_tokens=100, got: %s", body)
	}
	if strings.Contains(body, `"max_tokens"`) {
		t.Errorf("must use max_completion_tokens, not max_tokens: %s", body)
	}
	// deployment 已在 URL 路径里，请求体不该再带 model
	if strings.Contains(body, `"model"`) {
		t.Errorf("request body must not contain model field: %s", body)
	}
}

// TestBuildRequest_StreamUsage 流式必须显式要 usage，
// 否则拿不到 token 消耗，task_turn.total_tokens 会恒为 0。
func TestBuildRequest_StreamUsage(t *testing.T) {
	streamed := buildRequest([]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		nil, llm.ToolChoiceAuto, llm.Options{}, true)
	if streamed.StreamOpts == nil || !streamed.StreamOpts.IncludeUsage {
		t.Error("stream request must set stream_options.include_usage=true")
	}

	// 非流式不该带 stream_options
	blocking := buildRequest([]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		nil, llm.ToolChoiceAuto, llm.Options{}, false)
	if blocking.StreamOpts != nil {
		t.Error("non-stream request must not set stream_options")
	}
}

// TestBuildRequest_ToolChoiceMapping llm.ToolChoice → Azure tool_choice 的映射。
// ToolChoiceAny 必须映射成 "required"（规划期强制 submit_plan 依赖这个）。
func TestBuildRequest_ToolChoiceMapping(t *testing.T) {
	tools := []llm.Tool{{Name: "f", Parameters: map[string]interface{}{"type": "object"}}}
	cases := map[llm.ToolChoice]string{
		llm.ToolChoiceAny:  "required",
		llm.ToolChoiceNone: "none",
		llm.ToolChoiceAuto: "auto",
		llm.ToolChoice(""): "auto",
	}
	for in, want := range cases {
		req := buildRequest([]llm.Message{{Role: llm.RoleUser, Content: "x"}},
			tools, in, llm.Options{}, false)
		if req.ToolChoice != want {
			t.Errorf("choice %q → %v, want %q", in, req.ToolChoice, want)
		}
	}

	// 无工具时不下发 tool_choice
	noTools := buildRequest([]llm.Message{{Role: llm.RoleUser, Content: "x"}},
		nil, llm.ToolChoiceAny, llm.Options{}, false)
	if noTools.ToolChoice != nil {
		t.Errorf("no tools → tool_choice should be nil, got %v", noTools.ToolChoice)
	}
}

// ============================================================
// 消息转换与 tool_call_id 配对
// ============================================================

// TestMessagesToAzure_ToolCallIDPairing llm.Message 只有 ToolName，
// 协议却要求 tool 消息用 tool_call_id 关联。验证按序配对正确。
func TestMessagesToAzure_ToolCallIDPairing(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "q"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "search", Arguments: map[string]interface{}{"q": "a"}},
			{Name: "fetch", Arguments: map[string]interface{}{"u": "b"}},
		}},
		{Role: llm.RoleTool, ToolName: "search", Content: "r1"},
		{Role: llm.RoleTool, ToolName: "fetch", Content: "r2"},
	}
	out := messagesToAzure(msgs)
	if len(out) != 5 {
		t.Fatalf("want 5 messages, got %d", len(out))
	}

	asst := out[2]
	if len(asst.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(asst.ToolCalls))
	}
	searchID, fetchID := asst.ToolCalls[0].ID, asst.ToolCalls[1].ID
	if searchID == "" || fetchID == "" || searchID == fetchID {
		t.Fatalf("tool call ids must be non-empty and distinct: %q / %q", searchID, fetchID)
	}
	if out[3].ToolCallID != searchID {
		t.Errorf("search result id = %q, want %q", out[3].ToolCallID, searchID)
	}
	if out[4].ToolCallID != fetchID {
		t.Errorf("fetch result id = %q, want %q", out[4].ToolCallID, fetchID)
	}
	// arguments 必须是 JSON 字符串而非对象
	if asst.ToolCalls[0].Function.Arguments != `{"q":"a"}` {
		t.Errorf("arguments = %q, want JSON string", asst.ToolCalls[0].Function.Arguments)
	}
}

// TestMessagesToAzure_SameToolTwice 同一工具连续调用两次时，
// 两条结果必须按 FIFO 配到各自的 id，不能都配到第一个。
func TestMessagesToAzure_SameToolTwice(t *testing.T) {
	out := messagesToAzure([]llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Name: "sh"}, {Name: "sh"},
		}},
		{Role: llm.RoleTool, ToolName: "sh", Content: "first"},
		{Role: llm.RoleTool, ToolName: "sh", Content: "second"},
	})
	id0, id1 := out[0].ToolCalls[0].ID, out[0].ToolCalls[1].ID
	if out[1].ToolCallID != id0 {
		t.Errorf("first result id = %q, want %q", out[1].ToolCallID, id0)
	}
	if out[2].ToolCallID != id1 {
		t.Errorf("second result id = %q, want %q", out[2].ToolCallID, id1)
	}
}

// TestMarshalParseArgs args 在 map 与 JSON 字符串之间往返
func TestMarshalParseArgs(t *testing.T) {
	if got := marshalArgs(nil); got != "{}" {
		t.Errorf("marshalArgs(nil) = %q, want {}", got)
	}
	round := parseArgs(marshalArgs(map[string]interface{}{"city": "北京"}))
	if round["city"] != "北京" {
		t.Errorf("roundtrip lost data: %v", round)
	}
	// 非法 JSON / 空串不能 panic，返回空 map
	for _, bad := range []string{"", "   ", "not json", "{broken"} {
		if got := parseArgs(bad); got == nil || len(got) != 0 {
			t.Errorf("parseArgs(%q) = %v, want empty map", bad, got)
		}
	}
}

// ============================================================
// 响应解析
// ============================================================

// TestParseResponse_TextAndUsage 真实响应体（含 Azure 特有的
// content_filter_results / annotations 等额外字段）应被正确解析。
func TestParseResponse_TextAndUsage(t *testing.T) {
	body := []byte(`{"choices":[{"content_filter_results":{},"finish_reason":"stop","index":0,
"message":{"annotations":[],"content":"Hello! How can I help?","refusal":null,"role":"assistant"}}],
"model":"gpt-5.6-sol-2026-07-09","usage":{"completion_tokens":11,"prompt_tokens":7,"total_tokens":18}}`)

	got, err := parseResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "Hello! How can I help?" {
		t.Errorf("Text = %q", got.Text)
	}
	if got.Usage.PromptTokens != 7 || got.Usage.CompletionTokens != 11 || got.Usage.TotalTokens != 18 {
		t.Errorf("Usage = %+v", got.Usage)
	}
}

// TestParseResponse_ToolCalls content 为 null 且带 tool_calls 的形态
func TestParseResponse_ToolCalls(t *testing.T) {
	body := []byte(`{"choices":[{"finish_reason":"tool_calls","index":0,"message":{"content":null,
"role":"assistant","tool_calls":[{"function":{"arguments":"{\"city\":\"北京\"}","name":"get_weather"},
"id":"call_2Dv","type":"function"}]}}],"usage":{"completion_tokens":18,"prompt_tokens":126,"total_tokens":144}}`)

	got, err := parseResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Name != "get_weather" {
		t.Errorf("tool name = %q", got.ToolCalls[0].Name)
	}
	if got.ToolCalls[0].Arguments["city"] != "北京" {
		t.Errorf("arguments = %v", got.ToolCalls[0].Arguments)
	}
}

// TestParseResponse_Errors 错误体与空 choices 都要报错而非静默返回空结果
func TestParseResponse_Errors(t *testing.T) {
	if _, err := parseResponse([]byte(`{"choices":[]}`)); err == nil {
		t.Error("empty choices should error")
	}
	if _, err := parseResponse([]byte(`not json`)); err == nil {
		t.Error("invalid json should error")
	}
	_, err := parseResponse([]byte(`{"error":{"message":"boom","code":"unsupported_value","param":"temperature"}}`))
	if err == nil {
		t.Fatal("error body should error")
	}
	// param/code 必须带进错误信息 —— 参数类 400 靠它定位字段
	for _, want := range []string{"boom", "temperature", "unsupported_value"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// ============================================================
// 流式解析
// ============================================================

// TestParseStream_TextDeltasAndUsage 逐 chunk 回调文本，
// 并从最后一个 choices 为空的 chunk 里取到 usage。
func TestParseStream_TextDeltasAndUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[],"prompt_filter_results":[{"prompt_index":0}]}`,
		`data: {"choices":[{"delta":{"content":"","role":"assistant"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"1"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"、"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"2"},"index":0}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}]}`,
		`data: {"choices":[],"usage":{"completion_tokens":11,"prompt_tokens":7,"total_tokens":18}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	var deltas []string
	got, err := parseStream(strings.NewReader(sse), func(d llm.StreamDelta) {
		deltas = append(deltas, d.Text)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "1、2" {
		t.Errorf("Text = %q, want 1、2", got.Text)
	}
	// 空 content 不该触发回调
	if strings.Join(deltas, "") != "1、2" || len(deltas) != 3 {
		t.Errorf("deltas = %v, want 3 non-empty deltas", deltas)
	}
	// usage 在 choices 为空的末尾 chunk 里，必须取到
	if got.Usage.TotalTokens != 18 {
		t.Errorf("Usage = %+v, want total=18", got.Usage)
	}
}

// TestParseStream_ToolCallAccumulation 工具调用按 index 分片推送，
// 首片带 name、后续只带 arguments 增量，需按 index 累积拼装。
func TestParseStream_ToolCallAccumulation(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"search","arguments":""}}]},"index":0}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]},"index":0}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]},"index":0}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"fetch","arguments":"{}"}}]},"index":0}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	got, err := parseStream(strings.NewReader(sse), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(got.ToolCalls))
	}
	// index 升序，保持模型原始顺序
	if got.ToolCalls[0].Name != "search" || got.ToolCalls[1].Name != "fetch" {
		t.Errorf("tool order wrong: %v", got.ToolCalls)
	}
	if got.ToolCalls[0].Arguments["q"] != "go" {
		t.Errorf("accumulated arguments = %v, want q=go", got.ToolCalls[0].Arguments)
	}
}

// TestParseStream_ErrorChunk 流中途出现 error 必须报错，不能静默截断
func TestParseStream_ErrorChunk(t *testing.T) {
	sse := "data: {\"error\":{\"message\":\"rate limited\",\"code\":\"429\"}}\n\n"
	if _, err := parseStream(strings.NewReader(sse), func(llm.StreamDelta) {}); err == nil {
		t.Error("error chunk should error")
	} else if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should mention cause, got %v", err)
	}
}

// ============================================================
// 端到端（httptest 假服务）
// ============================================================

// TestDoChat_RequestShape 验证真实发出的 HTTP 请求形态：
// api-key header（不是 Authorization）、路径含 deployment、query 含 api-version。
func TestDoChat_RequestShape(t *testing.T) {
	var gotPath, gotQuery, gotAPIKey, gotAuth, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("api-key")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],
"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer srv.Close()

	c := New(Config{
		BaseURL:    srv.URL + "/llm-service/azure/public/openai",
		APIKey:     "test-key",
		Model:      "gpt-5.6-sol",
		APIVersion: "2025-01-01-preview",
	})

	got, err := c.ChatWithTools(context.Background(),
		[]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		nil, llm.ToolChoiceAuto, llm.DefaultOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "ok" {
		t.Errorf("Text = %q", got.Text)
	}

	wantPath := "/llm-service/azure/public/openai/deployments/gpt-5.6-sol/chat/completions"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotQuery != "api-version=2025-01-01-preview" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("api-key header = %q, want test-key", gotAPIKey)
	}
	// Azure 用 api-key，绝不能发 Bearer
	if gotAuth != "" {
		t.Errorf("Authorization header must be empty for azure, got %q", gotAuth)
	}
	// DefaultOptions 带 temperature=0.7，必须被 provider 丢掉
	if strings.Contains(gotBody, "temperature") || strings.Contains(gotBody, "top_p") {
		t.Errorf("sampling params leaked into request: %s", gotBody)
	}
}

// TestDoChat_HTTPErrorSurfacesParam 非 200 时要把 Azure 的 param/code 带出来。
// 这类参数错误（temperature 不支持等）如果只报 HTTP 400 会很难排查。
func TestDoChat_HTTPErrorSurfacesParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'top_p' is not supported with this model.",
"type":"invalid_request_error","param":"top_p","code":"unsupported_parameter"}}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := c.ChatWithTools(context.Background(),
		[]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		nil, llm.ToolChoiceAuto, llm.Options{})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"400", "top_p", "unsupported_parameter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestDoChat_StreamErrorNotSwallowed 流式路径下的非 200 返回的是 JSON 错误体
// 而非 SSE，必须在进 SSE 解析器之前拦住 —— 否则只会得到一个空结果，
// 上层会误判成「模型什么都没返回」。
func TestDoChat_StreamErrorNotSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Access denied","code":"401"}}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "bad", Model: "m"})
	_, err := c.ChatWithToolsStream(context.Background(),
		[]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		nil, llm.ToolChoiceAuto, llm.Options{}, func(llm.StreamDelta) {})
	if err == nil {
		t.Fatal("want error on stream 401")
	}
	if !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("error should mention cause, got %v", err)
	}
}

// TestValidation 配置缺失与空消息要提前失败，不发无效请求
func TestValidation(t *testing.T) {
	ctx := context.Background()
	msg := []llm.Message{{Role: llm.RoleUser, Content: "hi"}}

	incomplete := []Config{
		{APIKey: "k", Model: "m"},
		{BaseURL: "https://h", Model: "m"},
		{BaseURL: "https://h", APIKey: "k"},
	}
	for _, cfg := range incomplete {
		if _, err := New(cfg).ChatWithTools(ctx, msg, nil, llm.ToolChoiceAuto, llm.Options{}); err == nil {
			t.Errorf("incomplete config %+v should error", cfg)
		}
	}

	full := New(Config{BaseURL: "https://h", APIKey: "k", Model: "m"})
	if _, err := full.ChatWithTools(ctx, nil, nil, llm.ToolChoiceAuto, llm.Options{}); err == nil {
		t.Error("empty messages should error")
	}
	// 流式必须传 onDelta
	if _, err := full.ChatWithToolsStream(ctx, msg, nil, llm.ToolChoiceAuto, llm.Options{}, nil); err == nil {
		t.Error("nil onDelta should error")
	}
}

// TestAzureContent_Multimodal 验证多模态 content：无图保持纯字符串，有图转 text+image_url 数组。
func TestAzureContent_Multimodal(t *testing.T) {
	plain := azureContent(llm.Message{Role: llm.RoleUser, Content: "hi"})
	if s, ok := plain.(string); !ok || s != "hi" {
		t.Fatalf("无图消息 content 应为纯字符串, got %#v", plain)
	}

	c := azureContent(llm.Message{
		Role:    llm.RoleUser,
		Content: "看这张图",
		Images:  []llm.ImagePart{{MimeType: "image/jpeg", Data: "aGVsbG8="}},
	})
	parts, ok := c.([]azureContentPart)
	if !ok {
		t.Fatalf("有图消息 content 应为 []azureContentPart, got %T", c)
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
	if want := "data:image/jpeg;base64,aGVsbG8="; parts[1].ImageURL.URL != want {
		t.Fatalf("image url = %q, want %q", parts[1].ImageURL.URL, want)
	}
}

// ============================================================
// reasoning_content 解析
// ============================================================

// TestParseResponse_ReasoningContent 非流式响应里 message 带 reasoning_content，
// 必须解析进 ChatResult.Reasoning —— 推理模型「正文为空」时思考链是唯一诊断线索。
func TestParseResponse_ReasoningContent(t *testing.T) {
	body := []byte(`{"choices":[{"finish_reason":"stop","index":0,
"message":{"content":null,"reasoning_content":"用户想打开 QQ 音乐，但截图失败，需要检查权限。","role":"assistant"}}],
"usage":{"completion_tokens":30,"prompt_tokens":100,"total_tokens":130}}`)

	got, err := parseResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "" {
		t.Errorf("content 为 null 时 Text 应为空，got %q", got.Text)
	}
	if !strings.Contains(got.Reasoning, "截图失败") {
		t.Errorf("Reasoning 未解析：%q", got.Reasoning)
	}
}

// TestParseStream_ReasoningContent 流式响应里思考链走 delta.reasoning_content，
// 正文走 delta.content，两者必须分开累积 —— 思考链不能污染正文。
func TestParseStream_ReasoningContent(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"思考第一步","role":"assistant"},"index":0}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"，思考第二步"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"最终答复"},"index":0}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	got, err := parseStream(strings.NewReader(sse), func(llm.StreamDelta) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "最终答复" {
		t.Errorf("Text = %q, 思考链污染了正文", got.Text)
	}
	if got.Reasoning != "思考第一步，思考第二步" {
		t.Errorf("Reasoning = %q, 思考链未累积", got.Reasoning)
	}
}
