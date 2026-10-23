// Package azureopenai 实现 llm.Client / llm.StreamClient 的 Azure OpenAI provider。
//
// 面向 Azure OpenAI 风格的网关（如某 Azure OpenAI 网关），承载 gpt-5.6 系列
// （sol / terra / luna）等新一代推理模型。
//
// 与 pkg/llm/openai（标准 OpenAI 兼容 provider）的四处协议差异 —— 也正是
// 不能直接复用那个 provider 的原因：
//
//  1. 鉴权：header 用 `api-key: <key>`，而非 `Authorization: Bearer <key>`。
//  2. 端点：模型名在 URL 路径里（.../deployments/<model>/chat/completions），
//     并强制带 ?api-version=<ver> query；请求体里的 model 字段被忽略。
//  3. 输出上限：字段名是 max_completion_tokens。gpt-5.6 系列虽然仍接受旧的
//     max_tokens，但新字段才是官方口径，统一用新字段避免后续版本被拒。
//  4. 采样参数：**gpt-5.6 系列不接受 temperature / top_p**。
//     实测非默认 temperature 返回 400 unsupported_value，
//     top_p 返回 400 unsupported_parameter。因此本 provider 一律不下发这两个字段
//     （llm.Options 里的对应值被有意丢弃，详见 buildRequest 注释）。
//
// 相同之处：/chat/completions 的请求/响应体、tools schema、tool_calls 结构、
// SSE 流式分片格式都与 OpenAI 一致，所以消息转换与流式累积逻辑同构。
package azureopenai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// DefaultAPIVersion 未显式配置时使用的 api-version。
// gpt-5.6 系列需要 2025-01-01-preview 及以上。
const DefaultAPIVersion = "2025-01-01-preview"

// Config Azure OpenAI provider 配置
type Config struct {
	// BaseURL 部署根地址，到 openai 段为止，例如
	// https://gateway.example.com/llm-service/azure/public/openai
	// （本 provider 会补 /deployments/<model>/chat/completions）
	BaseURL string
	// APIKey 网关密钥，走 api-key header
	APIKey string
	// Model deployment 名，例如 gpt-5.6-sol
	Model string
	// APIVersion api-version query 参数；为空时用 DefaultAPIVersion
	APIVersion string
	// Timeout 单次阻塞调用的整体超时；<=0 时默认 120s。
	//
	// 比 openai provider 的 60s 更宽松：gpt-5.6 系列是推理模型，
	// 带 max/high reasoning effort 时首 token 延迟可达数十秒。
	// 流式调用不受此限制，由 ctx 控制生命周期。
	Timeout time.Duration
}

// Client Azure OpenAI provider 客户端
type Client struct {
	cfg  Config
	http *http.Client
}

// New 创建一个 Azure OpenAI Client
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = DefaultAPIVersion
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: timeout},
	}
}

// Name 实现 llm.Client
func (c *Client) Name() string { return "azure:" + c.cfg.Model }

// endpoint 拼出本次调用的完整 URL。
//
// deployment 名需转义：模型名含点号（gpt-5.6-sol）在路径里是合法的，
// 但走 PathEscape 可防未来出现含斜杠等特殊字符的 deployment 名。
func (c *Client) endpoint() string {
	base := strings.TrimRight(c.cfg.BaseURL, "/")
	return fmt.Sprintf("%s/deployments/%s/chat/completions?api-version=%s",
		base, url.PathEscape(c.cfg.Model), url.QueryEscape(c.cfg.APIVersion))
}

// ============================================================
// 请求 / 响应结构
// ============================================================

type azureMessage struct {
	Role string `json:"role"`
	// Content 纯文本时为 string，多模态（含图片）时为 []azureContentPart。
	// 请求侧两种形态都可能出现；响应侧（模型输出）恒为 string 或 null。
	Content interface{} `json:"content,omitempty"`
	// ReasoningContent 推理模型的思考链（响应侧才有）。请求侧不下发。
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []azureToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
}

// azureContentPart 多模态 content 数组里的一个元素（text / image_url 二选一）。
type azureContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *azureImageURL `json:"image_url,omitempty"`
}

// azureImageURL 图片 URL。数据用 data URI（data:image/...;base64,...）内联。
type azureImageURL struct {
	URL string `json:"url"`
}

type azureToolCall struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function azureFunction `json:"function"`
}

type azureFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type azureTool struct {
	Type     string            `json:"type"`
	Function azureFunctionDecl `json:"function"`
}

type azureFunctionDecl struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// azureRequest 请求体。
//
// 刻意不含 temperature / top_p：gpt-5.6 系列会对这两个字段报 400。
// 也不含 model —— deployment 已在 URL 路径里，请求体里的 model 会被网关忽略。
type azureRequest struct {
	Messages   []azureMessage      `json:"messages"`
	Tools      []azureTool         `json:"tools,omitempty"`
	ToolChoice interface{}         `json:"tool_choice,omitempty"`
	Stream     bool                `json:"stream"`
	StreamOpts *azureStreamOptions `json:"stream_options,omitempty"`
	// MaxCompletionTokens 输出上限。Azure 新版字段名，替代 max_tokens。
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	// ReasoningEffort 推理强度 none/low/medium/high/xhigh/max。
	// 为空则不下发，由服务端取默认值。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// azureStreamOptions 流式附加选项。
// include_usage=true 时最后一个 chunk 会带 usage，否则流式下拿不到 token 消耗。
type azureStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// azureResponse 同时承载非流式与流式 chunk 的解析。
type azureResponse struct {
	Choices []struct {
		Message      azureMessage `json:"message"`
		Delta        azureDelta   `json:"delta"`
		FinishReason string       `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *azureError `json:"error"`
}

// azureError 错误体。Azure 的 message/type/param/code 比标准 OpenAI 多一个 param，
// 参数类错误（如 temperature 不被支持）靠它才能定位到具体字段。
type azureError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    string `json:"code"`
}

// describe 拼出便于排查的错误字符串
func (e *azureError) describe() string {
	var b strings.Builder
	b.WriteString(e.Message)
	if e.Param != "" {
		fmt.Fprintf(&b, " (param=%s", e.Param)
		if e.Code != "" {
			fmt.Fprintf(&b, " code=%s", e.Code)
		}
		b.WriteString(")")
	} else if e.Code != "" {
		fmt.Fprintf(&b, " (code=%s)", e.Code)
	}
	return b.String()
}

type azureDelta struct {
	Role             string               `json:"role,omitempty"`
	Content          string               `json:"content,omitempty"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	ToolCalls        []azureToolCallDelta `json:"tool_calls,omitempty"`
}

type azureToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// ============================================================
// llm.Client / llm.StreamClient 实现
// ============================================================

// Chat 调用模型，一次性拿到完整回复文本（不使用 tools）。
func (c *Client) Chat(ctx context.Context, messages []llm.Message, opts llm.Options) (string, error) {
	result, err := c.doChat(ctx, messages, nil, "", opts, false, nil)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// ChatWithTools 调用模型，带工具调用能力。
func (c *Client) ChatWithTools(
	ctx context.Context, messages []llm.Message, tools []llm.Tool,
	choice llm.ToolChoice, opts llm.Options,
) (*llm.ChatResult, error) {
	return c.doChat(ctx, messages, tools, choice, opts, false, nil)
}

// ChatWithToolsStream 实现 llm.StreamClient：流式调用，文本增量通过 onDelta 回调。
func (c *Client) ChatWithToolsStream(
	ctx context.Context, messages []llm.Message, tools []llm.Tool,
	choice llm.ToolChoice, opts llm.Options, onDelta func(llm.StreamDelta),
) (*llm.ChatResult, error) {
	if onDelta == nil {
		return nil, fmt.Errorf("azure stream: onDelta callback required")
	}
	return c.doChat(ctx, messages, tools, choice, opts, true, onDelta)
}

// buildRequest 组装请求体。
//
// 关于被丢弃的 opts.Temperature / opts.TopP / opts.TopK：
// gpt-5.6 系列对这三个采样参数一律不接受（temperature 非 1 报
// unsupported_value，top_p 报 unsupported_parameter）。这里选择静默忽略
// 而非返回错误 —— 上层 actuator 统一用 llm.DefaultOptions()（temperature=0.7），
// 若报错则所有调用都会失败；而采样参数对本系列模型本就无意义，
// 忽略是唯一能让同一份上层代码同时适配两类 provider 的做法。
func buildRequest(
	messages []llm.Message, tools []llm.Tool,
	choice llm.ToolChoice, opts llm.Options, stream bool,
) azureRequest {
	req := azureRequest{
		Messages:            messagesToAzure(messages),
		Stream:              stream,
		MaxCompletionTokens: opts.MaxTokens,
	}
	if stream {
		// 不显式要求就拿不到流式 usage，token 统计会全为 0
		req.StreamOpts = &azureStreamOptions{IncludeUsage: true}
	}

	if len(tools) > 0 {
		req.Tools = make([]azureTool, 0, len(tools))
		for _, t := range tools {
			req.Tools = append(req.Tools, azureTool{
				Type: "function",
				Function: azureFunctionDecl{
					Name:        t.Name,
					Description: t.Description,
					// Azure 接受完整 JSON Schema，无需 Gemini 那样的白名单清洗
					Parameters: t.Parameters,
				},
			})
		}
		switch choice {
		case llm.ToolChoiceAny:
			req.ToolChoice = "required"
		case llm.ToolChoiceNone:
			req.ToolChoice = "none"
		default:
			req.ToolChoice = "auto"
		}
	}
	return req
}

// doChat Chat / ChatWithTools / ChatWithToolsStream 的实际实现。
func (c *Client) doChat(
	ctx context.Context, messages []llm.Message, tools []llm.Tool,
	choice llm.ToolChoice, opts llm.Options, stream bool, onDelta func(llm.StreamDelta),
) (*llm.ChatResult, error) {
	if c.cfg.BaseURL == "" || c.cfg.APIKey == "" || c.cfg.Model == "" {
		return nil, fmt.Errorf("azure config incomplete: base_url/api_key/model required")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("azure chat: at least one message required")
	}

	reqBody := buildRequest(messages, tools, choice, opts, stream)
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal azure request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build azure request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	// Azure 风格鉴权：api-key header，而非 Authorization: Bearer
	httpReq.Header.Set("api-key", c.cfg.APIKey)

	// 流式请求不受整体 Timeout 限制（生成过程随时有数据），由 ctx 控制。
	cli := c.http
	if stream {
		cli = &http.Client{Transport: c.http.Transport}
	}

	resp, err := cli.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("azure http call: %w", err)
	}
	defer resp.Body.Close()

	// 流式路径也要先拦非 200：出错时返回的是 JSON 错误体而非 SSE，
	// 直接交给 SSE 解析器会得到「没有 data: 行」这种无信息量的空结果。
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("azure http %d: %s", resp.StatusCode, describeErrorBody(body))
	}

	if stream {
		return parseStream(resp.Body, onDelta)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read azure response: %w", err)
	}
	return parseResponse(body)
}

// describeErrorBody 从非 200 响应体里提取可读错误信息。
// 解析失败则回退到截断的原文，保证排查线索不丢。
func describeErrorBody(body []byte) string {
	var resp azureResponse
	if err := json.Unmarshal(body, &resp); err == nil && resp.Error != nil {
		return resp.Error.describe()
	}
	return truncate(string(body), 500)
}

// parseResponse 解析非流式响应的完整结果。
func parseResponse(body []byte) (*llm.ChatResult, error) {
	var resp azureResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal azure response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("azure error: %s", resp.Error.describe())
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("azure: empty choices")
	}

	msg := resp.Choices[0].Message
	result := &llm.ChatResult{
		Text:      azureContentString(msg.Content),
		Reasoning: msg.ReasoningContent,
	}
	for _, tc := range msg.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
			Name:      tc.Function.Name,
			Arguments: parseArgs(tc.Function.Arguments),
		})
	}
	if resp.Usage != nil {
		result.Usage = llm.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}
	return result, nil
}

// toolCallAcc 流式工具调用的累积器。
// 流式下 tool_calls 按 index 分片推送（首片带 id/name，后续只带 arguments 增量）。
type toolCallAcc struct {
	name    string
	argsBuf strings.Builder
}

// bodyPrefixCapture 在读取响应体的同时，记录开头 maxBytes 字节的原始内容。
//
// 用途：流式解析出「空结果」（无正文、无工具调用）时，光看空 ChatResult 无法判断
// 是网关真的吐了空 SSE，还是吐了一段非 SSE 的裸 JSON（例如 200 + {"error":...}）——
// 后者会被 parseStream 的 `HasPrefix("data:")` 过滤掉、静默变成空。打印原始前缀
// 能一眼区分这两种情况，把「抖动」和「真错误被吞」分开。
type bodyPrefixCapture struct {
	r      io.Reader
	prefix []byte
	max    int
}

func (c *bodyPrefixCapture) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && len(c.prefix) < c.max {
		need := c.max - len(c.prefix)
		if n < need {
			need = n
		}
		c.prefix = append(c.prefix, p[:need]...)
	}
	return n, err
}

// emptyResultLogPrefixBytes 空结果时打印的原始响应前缀字节数。
// 只需要足够识别「是 SSE / 是 JSON 错误体 / 是空 body」即可，过长无意义。
const emptyResultLogPrefixBytes = 512

// parseStream 解析 text/event-stream 响应，逐 chunk 回调文本增量，
// 累积工具调用分片，最终返回与阻塞式等价的 ChatResult。
func parseStream(body io.Reader, onDelta func(llm.StreamDelta)) (*llm.ChatResult, error) {
	capture := &bodyPrefixCapture{r: body, max: emptyResultLogPrefixBytes}
	scanner := bufio.NewScanner(capture)
	// 单行 data 可能很长（含大段 arguments），放大默认 buffer
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var textBuf strings.Builder
	var reasonBuf strings.Builder
	accs := map[int]*toolCallAcc{}
	maxIndex := -1
	var usage llm.Usage

	// 识别「非 SSE 的裸错误体」：网关在流式下可能返回 200 + {"error":{...}}（不是 data: 行），
	// 这些行会被下面的 HasPrefix("data:") 跳过。记录是否见过 data: 行、并收集非 data 内容，
	// 循环结束后据此兜底，把真实错误暴露出来而不是静默吞成空结果。
	sawDataLine := false
	var nonData strings.Builder

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			nonData.WriteString(line)
			nonData.WriteByte('\n')
			continue
		}
		sawDataLine = true
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk azureResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("unmarshal azure stream chunk: %w (data=%s)",
				err, truncate(data, 300))
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("azure stream error: %s", chunk.Error.describe())
		}

		// usage 在最后一个 chunk（choices 为空）里，必须在 choices 判空之前取，
		// 否则会被下面的 continue 跳过，token 统计永远是 0。
		if chunk.Usage != nil {
			usage = llm.Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			textBuf.WriteString(delta.Content)
			onDelta(llm.StreamDelta{Text: delta.Content})
		}
		if delta.ReasoningContent != "" {
			reasonBuf.WriteString(delta.ReasoningContent)
			// 思考链增量一并透传：调试页据此实时展示原始 reasoning_content。
			onDelta(llm.StreamDelta{Reasoning: delta.ReasoningContent})
		}
		for _, tcd := range delta.ToolCalls {
			acc := accs[tcd.Index]
			if acc == nil {
				acc = &toolCallAcc{}
				accs[tcd.Index] = acc
				if tcd.Index > maxIndex {
					maxIndex = tcd.Index
				}
			}
			if tcd.Function.Name != "" {
				acc.name = tcd.Function.Name
			}
			if tcd.Function.Arguments != "" {
				acc.argsBuf.WriteString(tcd.Function.Arguments)
			}
			// 把工具调用分片一并透传给上层，供调试页观察完整 raw 流式输出。
			// 这里只做增量转发，最终结构化结果仍由下方按 index 累积得到。
			onDelta(llm.StreamDelta{ToolCallDelta: &llm.ToolCallDelta{
				Index: tcd.Index,
				ID:    tcd.ID,
				Name:  tcd.Function.Name,
				Args:  tcd.Function.Arguments,
			}})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan azure stream response: %w", err)
	}

	// 非 SSE 响应体兜底：全程没见到 data: 行，说明网关返回的根本不是流。
	// 典型：gpt-5.6-sol 网关在流式下对「tool_call_id 配对不完整」返回 200 + 裸 JSON 错误体
	// （含 "error" 字段），这段会被上面的 HasPrefix("data:") 全部跳过；不当成错误
	// 就会被误判成「模型没吐内容 / 没吐工具调用」。
	if !sawDataLine {
		if trimmed := strings.TrimSpace(nonData.String()); trimmed != "" {
			var errResp azureResponse
			if json.Unmarshal([]byte(trimmed), &errResp) == nil && errResp.Error != nil {
				return nil, fmt.Errorf("azure stream error (non-SSE body): %s", errResp.Error.describe())
			}
			log.Printf("[azure] stream body is not SSE (no data lines): %q", truncate(trimmed, 300))
		}
	}

	result := &llm.ChatResult{Text: textBuf.String(), Reasoning: reasonBuf.String(), Usage: usage}
	// tool_calls 按 index 升序输出，保持模型原始顺序
	for i := 0; i <= maxIndex; i++ {
		acc := accs[i]
		if acc == nil {
			continue
		}
		result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
			Name:      acc.name,
			Arguments: parseArgs(acc.argsBuf.String()),
		})
	}
	// 空结果打原始前缀：正文与工具调用都为空时，空 ChatResult 本身无法区分
	// 「网关真空返回」与「非 SSE 裸 JSON 被过滤」两类情况，前缀日志是关键排查线索。
	if result.Text == "" && len(result.ToolCalls) == 0 {
		log.Printf("[azure] stream returned empty content, raw body prefix: %q", string(capture.prefix))
	}
	return result, nil
}

// ============================================================
// 消息转换
// ============================================================

// messagesToAzure 把统一协议的 Message 列表转成 Azure messages。
//
// llm.Message 的 tool 消息只有 ToolName 没有 tool_call_id，而协议要求 tool
// 消息用 tool_call_id 关联到 assistant 的 tool_calls。这里在 provider 内部
// 按序为 assistant 的 tool_calls 生成 id，并用 name 队列跟踪待消费的 id，
// tool 消息按 ToolName 消费队首 id 完成配对。
//
// 上层（agent_history）已通过 dropOrphanToolResults 保证 tool 消息前必有
// 对应 name 的 assistant tool_calls，因此按名配对是可靠的。
//
// llm.ToolCall.Signature 被有意忽略：那是 Gemini 的 thoughtSignature 机制，
// Azure/OpenAI 协议没有对应字段。
func messagesToAzure(messages []llm.Message) []azureMessage {
	out := make([]azureMessage, 0, len(messages))
	// pendingID[name] 该 name 尚未被 tool 结果消费的 id 队列
	pendingID := map[string][]string{}
	seq := 0

	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, azureMessage{Role: "system", Content: m.Content})

		case llm.RoleUser:
			out = append(out, azureMessage{Role: "user", Content: azureContent(m)})

		case llm.RoleAssistant:
			am := azureMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				id := fmt.Sprintf("call_%d", seq)
				seq++
				am.ToolCalls = append(am.ToolCalls, azureToolCall{
					ID:   id,
					Type: "function",
					Function: azureFunction{
						Name:      tc.Name,
						Arguments: marshalArgs(tc.Arguments),
					},
				})
				pendingID[tc.Name] = append(pendingID[tc.Name], id)
			}
			out = append(out, am)

		case llm.RoleTool:
			var id string
			if q := pendingID[m.ToolName]; len(q) > 0 {
				id = q[0]
				pendingID[m.ToolName] = q[1:]
			}
			out = append(out, azureMessage{
				Role:       "tool",
				ToolCallID: id,
				Content:    m.Content,
			})
		}
	}
	return out
}

// azureContent 把统一协议的消息内容转成 Azure content 形态：
// 无图片 → 纯字符串（保持既有纯文本路径不变）；有图片 → text + image_url 数组。
func azureContent(m llm.Message) interface{} {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := make([]azureContentPart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, azureContentPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, azureContentPart{
			Type:     "image_url",
			ImageURL: &azureImageURL{URL: imageDataURL(img)},
		})
	}
	return parts
}

// azureContentString 从响应侧 content 里取回纯文本（content 可能是 string / null / 数组）。
func azureContentString(c interface{}) string {
	if s, ok := c.(string); ok {
		return s
	}
	return ""
}

// imageDataURL 把 ImagePart 拼成 data URI（data:image/png;base64,...）。
func imageDataURL(img llm.ImagePart) string {
	mime := img.MimeType
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + img.Data
}

// marshalArgs 把参数 map 序列化成 JSON 字符串（协议的 arguments 是字符串）。
func marshalArgs(args map[string]interface{}) string {
	if args == nil {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseArgs 把 JSON 字符串参数解析成 map。空串或非法 JSON 返回空 map。
func parseArgs(s string) map[string]interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return map[string]interface{}{}
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// 编译期确认接口实现
var (
	_ llm.Client       = (*Client)(nil)
	_ llm.StreamClient = (*Client)(nil)
)
