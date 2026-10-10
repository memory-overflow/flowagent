// Package claude 实现 llm.Client 接口的 Claude（原生 Anthropic Messages API）provider。
//
// 面向 Anthropic 兼容协议（如腾讯云 Token Plan 的 /plan/anthropic 入口），
// 请求走 POST {base}/v1/messages，鉴权用 x-api-key + anthropic-version。
// 与 openai/gemini provider 的关键差异：
//
//   - system 是请求体的顶层字段，而非 messages 里的 system 角色；
//   - 工具调用使用 tool_use / tool_result 内容块，靠 tool_use_id 关联，
//     llm.Message 只有 ToolName，因此本 provider 内部按序生成 id 并配对；
//   - Anthropic 要求消息 user / assistant 交替，并行工具结果必须合并进
//     同一个 user 消息的 content 数组（tool_result 块逐个列出）。
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// anthropicVersion 官方要求的 API 版本头。取值固定，网关按此派发协议行为。
const anthropicVersion = "2023-06-01"

// defaultMaxTokens Anthropic 要求 max_tokens 必填且 > 0。llm.Options.MaxTokens
// 为 0 表示「不限制」，但协议不认，此时回落一个足够大的默认值。
//
// 不能设成任意大：各网关对 max_tokens 有上限，超限会 400 invalid params。
// 实测腾讯云 lkeap /plan/anthropic 的 glm-5.3 上限在 131072(OK) 与
// 262144(400) 之间，故取 131072（128K）作为「不限制」的兜底，既覆盖长输出
// 场景，又不触发网关参数校验。
const defaultMaxTokens = 131072

// Config Claude provider 配置
type Config struct {
	// BaseURL Anthropic 兼容根地址，如 https://api.lkeap.cloud.tencent.com/plan/anthropic
	// （不含尾斜杠，端点统一拼 /v1/messages）
	BaseURL string
	// APIKey 网关密钥，经 x-api-key header 传递
	APIKey string
	// Model 模型名，如 claude-sonnet-4-5
	Model string
	// Timeout 单次阻塞调用超时；<=0 时默认 60s
	Timeout time.Duration
}

// Client Claude provider 客户端
type Client struct {
	cfg  Config
	http *http.Client
}

// New 创建一个 Claude Client
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: timeout,
		},
	}
}

// Name 实现 llm.Client
func (c *Client) Name() string { return "claude:" + c.cfg.Model }

// ============================================================
// Claude 请求 / 响应结构
// ============================================================

// claudeContentBlock 一个内容块：text / tool_use / tool_result 三选一。
//
//   - text：Text 非空
//   - tool_use：ID + Name + Input 非空（模型发起的工具调用）
//   - tool_result：ToolUseID + Content 非空（工具执行结果回喂）
type claudeContentBlock struct {
	Type      string                 `json:"type"`
	Text      string                 `json:"text,omitempty"`
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	ToolUseID string                 `json:"tool_use_id,omitempty"`
	Content   interface{}            `json:"content,omitempty"`
	Source    *claudeImageSource     `json:"source,omitempty"`
}

// claudeImageSource 图片源。base64 内联数据。
type claudeImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png"
	Data      string `json:"data"`       // base64
}

// claudeMessage 一条消息。Content 是 string（纯文本）或 []claudeContentBlock。
type claudeMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type claudeTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type claudeToolChoice struct {
	Type string `json:"type"` // auto / any / none
}

type claudeRequest struct {
	Model       string            `json:"model"`
	MaxTokens   int               `json:"max_tokens"`
	System      string            `json:"system,omitempty"`
	Messages    []claudeMessage   `json:"messages"`
	Tools       []claudeTool      `json:"tools,omitempty"`
	ToolChoice  *claudeToolChoice `json:"tool_choice,omitempty"`
	Temperature float64           `json:"temperature,omitempty"`
	TopP        float64           `json:"top_p,omitempty"`
	TopK        int               `json:"top_k,omitempty"`
}

type claudeResponse struct {
	Content []struct {
		Type  string                 `json:"type"`
		Text  string                 `json:"text,omitempty"`
		ID    string                 `json:"id,omitempty"`
		Name  string                 `json:"name,omitempty"`
		Input map[string]interface{} `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason,omitempty"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ============================================================
// llm.Client 实现
// ============================================================

// Chat 调用 Claude，一次性拿到完整回复文本（不使用 tools）。
func (c *Client) Chat(ctx context.Context, messages []llm.Message, opts llm.Options) (string, error) {
	result, err := c.doChat(ctx, messages, nil, "", opts)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// ChatWithTools 调用 Claude，带工具调用能力。
func (c *Client) ChatWithTools(
	ctx context.Context, messages []llm.Message, tools []llm.Tool,
	choice llm.ToolChoice, opts llm.Options,
) (*llm.ChatResult, error) {
	return c.doChat(ctx, messages, tools, choice, opts)
}

// doChat Chat / ChatWithTools 的实际实现
func (c *Client) doChat(
	ctx context.Context, messages []llm.Message, tools []llm.Tool,
	choice llm.ToolChoice, opts llm.Options,
) (*llm.ChatResult, error) {
	if c.cfg.BaseURL == "" || c.cfg.APIKey == "" || c.cfg.Model == "" {
		return nil, fmt.Errorf("claude config incomplete: base_url/api_key/model required")
	}

	system, msgs := messagesToAnthropic(messages)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("claude chat: at least one message required")
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	reqBody := claudeRequest{
		Model:       c.cfg.Model,
		MaxTokens:   maxTokens,
		System:      system,
		Messages:    msgs,
		Temperature: opts.Temperature,
		TopP:        opts.TopP,
		TopK:        opts.TopK,
	}

	if len(tools) > 0 {
		reqBody.Tools = make([]claudeTool, 0, len(tools))
		for _, t := range tools {
			schema := t.Parameters
			if schema == nil {
				schema = map[string]interface{}{"type": "object"}
			}
			reqBody.Tools = append(reqBody.Tools, claudeTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schema,
			})
		}
		tc := &claudeToolChoice{}
		switch choice {
		case llm.ToolChoiceAny:
			tc.Type = "any"
		case llm.ToolChoiceNone:
			tc.Type = "none"
		default:
			tc.Type = "auto"
		}
		reqBody.ToolChoice = tc
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal claude request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/messages", strings.TrimRight(c.cfg.BaseURL, "/"))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build claude request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude http call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read claude response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude http %d: %s", resp.StatusCode, truncate(string(body), 500))
	}
	return parseResponse(body)
}

// parseResponse 解析非流式响应：content 里的 text 块拼 Text、tool_use 块收集 ToolCalls。
func parseResponse(body []byte) (*llm.ChatResult, error) {
	var resp claudeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal claude response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("claude error: %s", resp.Error.Message)
	}

	result := &llm.ChatResult{}
	var textBuf strings.Builder
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			textBuf.WriteString(block.Text)
		case "tool_use":
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				Name:      block.Name,
				Arguments: block.Input,
			})
		}
	}
	result.Text = textBuf.String()
	result.Usage = llm.Usage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}
	return result, nil
}

// ============================================================
// 消息转换
// ============================================================

// messagesToAnthropic 把统一协议的 Message 列表转成 Anthropic messages，
// 并把 system 消息合并成顶层 system 字符串返回。
//
// 关键规则：
//   - RoleTool 的 tool_result 块必须紧跟发起 tool_use 的 assistant 消息，
//     并行调用的多个结果合并进同一个 user 消息（用 pendingResults 累积，
//     遇到下一条非 tool 消息时 flush）。
//   - tool_use_id 在 provider 内部按序生成（toolu_<seq>），按 ToolName 队列
//     配对到后续的 tool_result，保证回喂时引用正确的 id。
func messagesToAnthropic(messages []llm.Message) (string, []claudeMessage) {
	var system strings.Builder
	msgs := make([]claudeMessage, 0, len(messages))
	// pendingID[name] 该 name 尚未被 tool_result 消费的 tool_use_id 队列
	pendingID := map[string][]string{}
	seq := 0

	// 累积的 tool_result 块，等待 flush 成一个 user 消息
	pendingResults := make([]claudeContentBlock, 0, 4)
	flushResults := func() {
		if len(pendingResults) == 0 {
			return
		}
		msgs = append(msgs, claudeMessage{Role: "user", Content: pendingResults})
		pendingResults = nil
	}

	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			if system.Len() > 0 {
				system.WriteString("\n\n")
			}
			system.WriteString(m.Content)

		case llm.RoleUser:
			flushResults()
			msgs = append(msgs, claudeMessage{Role: "user", Content: claudeUserContent(m)})

		case llm.RoleAssistant:
			flushResults()
			if len(m.ToolCalls) == 0 {
				msgs = append(msgs, claudeMessage{Role: "assistant", Content: m.Content})
				continue
			}
			blocks := make([]claudeContentBlock, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				blocks = append(blocks, claudeContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				id := fmt.Sprintf("toolu_%d", seq)
				seq++
				input := tc.Arguments
				if input == nil {
					input = map[string]interface{}{}
				}
				blocks = append(blocks, claudeContentBlock{
					Type:  "tool_use",
					ID:    id,
					Name:  tc.Name,
					Input: input,
				})
				pendingID[tc.Name] = append(pendingID[tc.Name], id)
			}
			msgs = append(msgs, claudeMessage{Role: "assistant", Content: blocks})

		case llm.RoleTool:
			var id string
			if q := pendingID[m.ToolName]; len(q) > 0 {
				id = q[0]
				pendingID[m.ToolName] = q[1:]
			}
			pendingResults = append(pendingResults, claudeContentBlock{
				Type:      "tool_result",
				ToolUseID: id,
				Content:   m.Content,
			})
		}
	}
	flushResults()
	return system.String(), msgs
}

// claudeUserContent 构造 user 消息的 content：无图片 → 纯字符串；有图片 → text + image 块数组。
func claudeUserContent(m llm.Message) interface{} {
	if len(m.Images) == 0 {
		return m.Content
	}
	blocks := make([]claudeContentBlock, 0, len(m.Images)+1)
	if m.Content != "" {
		blocks = append(blocks, claudeContentBlock{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		mime := img.MimeType
		if mime == "" {
			mime = "image/png"
		}
		blocks = append(blocks, claudeContentBlock{
			Type:   "image",
			Source: &claudeImageSource{Type: "base64", MediaType: mime, Data: img.Data},
		})
	}
	return blocks
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
