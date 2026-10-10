// Package gemini 实现 llm.Client 接口的 Gemini provider。
//
// 走内部代理网关（gateway.example.com），Bearer 鉴权，Gemini 原生请求体协议。
// endpoint 使用 :streamGenerateContent —— 服务端会一次性返回 JSON 数组
// （数组里每个元素是一段 chunk），本 provider 聚合所有 chunk 的
// candidates[0].content.parts[].text 得到完整回复。
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// Config Gemini provider 配置
type Config struct {
	// BaseURL 网关根地址，例如 https://gateway.example.com
	BaseURL string
	// APIKey Bearer token
	APIKey string
	// Model 模型名，例如 gemini-3.1-pro-preview
	Model string
	// Timeout 单次调用超时；<=0 时使用默认 60s
	Timeout time.Duration
}

// Client Gemini provider 客户端
type Client struct {
	cfg  Config
	http *http.Client
}

// New 创建一个 Gemini Client
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
func (c *Client) Name() string { return "gemini:" + c.cfg.Model }

// ============================================================
// Gemini 请求 / 响应结构
// ============================================================

// geminiFunctionCall 模型返回的函数调用
type geminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

// geminiFunctionResponse 工具执行结果，回喂给模型
type geminiFunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

// geminiPart 一个 part 可以是 text / functionCall / functionResponse（三选一）
//
// ThoughtSignature 是模型内部推理状态的加密表示。Gemini 3 起，回放含 functionCall
// 的历史时必须把它原样带回原来那个 part，否则返回 400。
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
}

// geminiInlineData 内联图片数据（多模态输入）。
type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// dummyThoughtSignature 官方提供的占位签名，用于跳过 thought_signature 校验。
//
// 仅在确实拿不到真实签名时使用：
//   - 本次改动前落库的历史 turn（当时没有解析签名）
//   - 非模型生成的 functionCall（例如客户端确定性注入）
//
// 官方标注 strongly discouraged —— 会丢失推理连续性，可能降低模型表现，
// 所以只作兜底，真实签名永远优先。
const dummyThoughtSignature = "skip_thought_signature_validator"

// isFunctionResponseContent 判断一个 content 是否为工具结果消息，
// 用于把并行调用的多个 functionResponse 合并进同一个 user content。
func isFunctionResponseContent(c geminiContent) bool {
	if c.Role != "user" || len(c.Parts) == 0 {
		return false
	}
	// 只要全部 part 都是 functionResponse 就可以继续追加；
	// 混入 text 的 user 消息属于真实用户输入，不能往里塞工具结果。
	for _, p := range c.Parts {
		if p.FunctionResponse == nil {
			return false
		}
	}
	return true
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiSystemInstruction struct {
	Parts geminiPart `json:"parts"`
}

type geminiGenerationConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	TopP            float64 `json:"top_p,omitempty"`
	TopK            int     `json:"top_k,omitempty"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
}

// geminiFunctionDeclaration 一个 tool 的 schema
type geminiFunctionDeclaration struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"function_declarations"`
}

type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"` // AUTO / ANY / NONE
	AllowedFunctionNames []string `json:"allowed_function_names,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"function_calling_config"`
}

type geminiRequest struct {
	SystemInstruction *geminiSystemInstruction `json:"system_instruction,omitempty"`
	Contents          []geminiContent          `json:"contents"`
	GenerationConfig  geminiGenerationConfig   `json:"generation_config"`
	Tools             []geminiTool             `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig        `json:"tool_config,omitempty"`
}

// geminiChunk 每一段 chunk 的顶层结构
type geminiChunk struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason,omitempty"`
	} `json:"candidates"`
	// UsageMetadata token 用量。流式响应里通常只有最后一个 chunk 带，
	// 且是「截至当前的累计值」而非增量 —— 聚合时取最后一个非零值，不要累加。
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
	// 错误响应可能带 error 字段
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// geminiUsageMetadata Gemini 返回的 token 用量
type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	// ThoughtsTokenCount 思考 token（Gemini 3 推理模型）。
	// 已包含在 TotalTokenCount 里，单独列出便于观察推理开销。
	ThoughtsTokenCount int `json:"thoughtsTokenCount,omitempty"`
}

// Chat 调用 Gemini，一次性拿到完整回复文本（不使用 tools）
func (c *Client) Chat(ctx context.Context, messages []llm.Message, opts llm.Options) (string, error) {
	result, err := c.doChat(ctx, messages, nil, "", opts)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// ChatWithTools 调用 Gemini，带工具调用能力。
//   - tools 非空且 choice=any 时，通过 tool_config.mode=ANY 强制模型调用工具集里的函数
//   - 模型返回 functionCall part 时，聚合到 ChatResult.ToolCalls
//   - 模型返回 text part 时，聚合到 ChatResult.Text
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
		return nil, fmt.Errorf("gemini config incomplete: base_url/api_key/model required")
	}

	// 拆出 system instruction 和其余 contents
	systemText, contents := messagesToContents(messages)
	if len(contents) == 0 {
		return nil, fmt.Errorf("gemini chat: at least one user message required")
	}

	reqBody := geminiRequest{
		Contents: contents,
		GenerationConfig: geminiGenerationConfig{
			Temperature:     opts.Temperature,
			TopP:            opts.TopP,
			TopK:            opts.TopK,
			MaxOutputTokens: opts.MaxTokens,
		},
	}
	if systemText != "" {
		reqBody.SystemInstruction = &geminiSystemInstruction{
			Parts: geminiPart{Text: systemText},
		}
	}

	// 组装 tools / tool_config
	if len(tools) > 0 {
		decls := make([]geminiFunctionDeclaration, 0, len(tools))
		allowedNames := make([]string, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, geminiFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				// Gemini 只接受 OpenAPI 3.0 子集，必须清洗掉
				// $schema / additionalProperties / propertyNames 等 JSON Schema 关键字
				Parameters: sanitizeToolSchema(t.Parameters),
			})
			allowedNames = append(allowedNames, t.Name)
		}
		reqBody.Tools = []geminiTool{{FunctionDeclarations: decls}}

		mode := "AUTO"
		switch choice {
		case llm.ToolChoiceAny:
			mode = "ANY"
		case llm.ToolChoiceNone:
			mode = "NONE"
		}
		toolCfg := &geminiToolConfig{
			FunctionCallingConfig: geminiFunctionCallingConfig{
				Mode: mode,
			},
		}
		if mode == "ANY" {
			toolCfg.FunctionCallingConfig.AllowedFunctionNames = allowedNames
		}
		reqBody.ToolConfig = toolCfg
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini request: %w", err)
	}

	url := fmt.Sprintf("%s/%s:streamGenerateContent",
		strings.TrimRight(c.cfg.BaseURL, "/"), c.cfg.Model)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build gemini request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini http call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read gemini response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 400 多半是 schema 不被接受。把清洗后的实际 tools payload 打出来，
		// 否则只看到 "field_declarations[N]" 这样的索引，定位不到具体工具。
		if resp.StatusCode == http.StatusBadRequest && len(reqBody.Tools) > 0 {
			if tb, mErr := json.Marshal(reqBody.Tools); mErr == nil {
				log.Printf("[gemini] 400 with tools payload=%s", truncate(string(tb), 8000))
			}
		}
		return nil, fmt.Errorf("gemini http %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	result, err := parseChunksToResult(body)
	if err != nil {
		return nil, fmt.Errorf("parse gemini response: %w (body=%s)", err, truncate(string(body), 500))
	}
	return result, nil
}

// parseChunksToResult 解析 streamGenerateContent 一次性返回的 JSON 数组，
// 聚合每个 chunk 的 candidates[0].content.parts[]：text 拼字符串、functionCall 收集到 ToolCalls
func parseChunksToResult(body []byte) (*llm.ChatResult, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty response")
	}

	var chunks []geminiChunk
	if body[0] == '[' {
		if err := json.Unmarshal(body, &chunks); err != nil {
			return nil, fmt.Errorf("unmarshal chunk array: %w", err)
		}
	} else {
		var single geminiChunk
		if err := json.Unmarshal(body, &single); err != nil {
			return nil, fmt.Errorf("unmarshal single chunk: %w", err)
		}
		chunks = []geminiChunk{single}
	}

	result := &llm.ChatResult{}
	var textBuf strings.Builder
	for _, ch := range chunks {
		if ch.Error != nil {
			return nil, fmt.Errorf("gemini error %d %s: %s", ch.Error.Code, ch.Error.Status, ch.Error.Message)
		}
		// usageMetadata 是累计值，后面的 chunk 覆盖前面的（而非累加）。
		// 只在非零时覆盖，避免尾部空 usage 把有效值清掉。
		if ch.UsageMetadata != nil && ch.UsageMetadata.TotalTokenCount > 0 {
			result.Usage = llm.Usage{
				PromptTokens:     ch.UsageMetadata.PromptTokenCount,
				CompletionTokens: ch.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      ch.UsageMetadata.TotalTokenCount,
			}
			// 思考 token 计入输出侧：它是模型生成的，且已含在 total 内，
			// 不加上会导致 prompt + completion != total 看起来对不上账。
			result.Usage.CompletionTokens += ch.UsageMetadata.ThoughtsTokenCount
		}
		for _, cand := range ch.Candidates {
			for _, p := range cand.Content.Parts {
				if p.FunctionCall != nil {
					result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
						Name:      p.FunctionCall.Name,
						Arguments: p.FunctionCall.Args,
						// 签名必须跟着它所属的 functionCall 走。
						// 并行调用时只有第一个 part 带签名，其余为空是正常的。
						Signature: p.ThoughtSignature,
					})
					continue
				}
				if p.Text != "" {
					textBuf.WriteString(p.Text)
				}
			}
		}
	}
	result.Text = textBuf.String()
	return result, nil
}

// geminiUserParts 构造 user 消息的 parts：无图片 → 纯 text；有图片 → text + inlineData。
func geminiUserParts(m llm.Message) []geminiPart {
	if len(m.Images) == 0 {
		return []geminiPart{{Text: m.Content}}
	}
	parts := make([]geminiPart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, geminiPart{Text: m.Content})
	}
	for _, img := range m.Images {
		mime := img.MimeType
		if mime == "" {
			mime = "image/png"
		}
		parts = append(parts, geminiPart{
			InlineData: &geminiInlineData{MimeType: mime, Data: img.Data},
		})
	}
	return parts
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// messagesToContents 把统一协议的 Message 列表转成 Gemini contents，
// 并把 system 消息合并成单独的 system_instruction 文本返回。
//
// 抽成独立函数是为了可测 —— 签名回传、并行 functionResponse 合并这些
// 规则出错就是 400，必须能单独断言。
func messagesToContents(messages []llm.Message) (string, []geminiContent) {
	var systemText strings.Builder
	contents := make([]geminiContent, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			if systemText.Len() > 0 {
				systemText.WriteString("\n\n")
			}
			systemText.WriteString(m.Content)

		case llm.RoleUser:
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: geminiUserParts(m),
			})

		case llm.RoleAssistant:
			// assistant 消息可能是纯文本，也可能携带工具调用请求
			parts := make([]geminiPart, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for i, tc := range m.ToolCalls {
				part := geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tc.Name,
						Args: tc.Arguments,
					},
					ThoughtSignature: tc.Signature,
				}
				// Gemini 3 只校验每个 step 的第一个 functionCall part。
				// 历史数据（本次改动前落库的 turn）没有签名，直接发会 400；
				// 用官方给的 dummy 值跳过校验，保证旧会话仍能续跑。
				// 后续 part 缺签名是正常的（并行调用只有首个带签名），不要伪造。
				if i == 0 && part.ThoughtSignature == "" {
					part.ThoughtSignature = dummyThoughtSignature
				}
				parts = append(parts, part)
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, geminiContent{Role: "model", Parts: parts})

		case llm.RoleTool:
			// 工具执行结果。Gemini 要求以 role=user 的 functionResponse part 回喂。
			// 工具返回是任意文本，统一包成 {"result": "..."} 满足 response 必须是 object 的约束。
			respPart := geminiPart{
				FunctionResponse: &geminiFunctionResponse{
					Name:     m.ToolName,
					Response: map[string]interface{}{"result": m.Content},
				},
			}
			// 并行调用的多个结果必须合并进同一个 user content。
			// 拆成 FC1,FC2,FR1(user),FR2(user) 这样的交错结构会被 Gemini 判 400，
			// 正确形态是 [FC1,FC2](model) + [FR1,FR2](user)。
			if n := len(contents); n > 0 && isFunctionResponseContent(contents[n-1]) {
				contents[n-1].Parts = append(contents[n-1].Parts, respPart)
			} else {
				contents = append(contents, geminiContent{
					Role:  "user",
					Parts: []geminiPart{respPart},
				})
			}
		}
	}
	return systemText.String(), contents
}
