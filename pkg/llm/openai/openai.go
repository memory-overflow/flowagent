// Package openai 实现 llm.Client 接口的 OpenAI 兼容 provider。
//
// 面向任何实现 OpenAI /v1/chat/completions 协议的网关或模型服务
// （例如腾讯混元 tokenhub 网关）。与 gemini provider 的关键差异：
//
//   - 支持真流式（stream=true 返回 text/event-stream + chunked，
//     逐 token 推送 delta.content），因此额外实现 llm.StreamClient；
//   - 工具 schema 直接透传，无需 Gemini 那样的白名单清洗；
//   - 工具结果通过 tool_call_id 关联到 assistant 的 tool_calls，
//     而 llm.Message 只有 ToolName，因此在本 provider 内部按序生成 id 并配对。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// Config OpenAI 兼容 provider 配置
type Config struct {
	// BaseURL 服务根地址，形如 https://openai.example.com/v1
	// （不含尾斜杠，端点统一拼 /chat/completions）
	BaseURL string
	// APIKey Bearer token
	APIKey string
	// Model 模型名，例如 hy3
	Model string
	// Timeout 单次阻塞调用的整体超时；<=0 时默认 60s。
	// 注意：流式调用不受此限制，由 ctx 控制生命周期。
	Timeout time.Duration
}

// Client OpenAI 兼容 provider 客户端
type Client struct {
	cfg  Config
	http *http.Client
}

// New 创建一个 OpenAI 兼容 Client
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
func (c *Client) Name() string { return "openai:" + c.cfg.Model }

// ============================================================
// 网关差异：tool_choice 能力探测
// ============================================================
//
// OpenAI 兼容协议里「强制调用某个工具」写作 tool_choice="required"，
// 但并非所有模型/网关都接受它：lkeap 的 deepseek/deepseek-v4-pro 只认
// "auto"，传 "required" 直接 400（code 20024 invalid params），
// 而同一网关的 hy4-preview 却支持 —— 所以这是「模型 × 网关」级别的能力，
// 既不能一刀切禁用 required（会削弱支持它的模型上的强制性），
// 也不值得为每个模型加一个配置开关（新接一个模型就要配一次，忘了就静默失效）。
//
// 这里用「请求被拒 → 降级 auto 重试 → 记住结论」处理：
//   - 首次失败多花一次 400 往返（几百毫秒），之后命中缓存直接发 auto；
//   - 缓存按 base_url + model 维度记，换模型/换网关互不干扰；
//   - 结论只在进程内有效，不落库：网关行为可能变，重启即重新探测。

// toolChoiceUnsupported 记录「不接受 tool_choice=required」的模型。
// key = base_url + "|" + model（见 toolChoiceCacheKey）。
var toolChoiceUnsupported sync.Map

func toolChoiceCacheKey(cfg Config) string {
	return cfg.BaseURL + "|" + cfg.Model
}

// resolveToolChoice 把 llm.ToolChoice 映射为协议字段值。
// 该模型已知不支持 required 时（运行时探测命中 / 兼容档命中），ToolChoiceAny
// 降级为 auto —— 由提示词承担「必须调工具」的约束（规划期 system prompt 已
// 明确要求以 submit_plan 收尾）。
func (c *Client) resolveToolChoice(choice llm.ToolChoice) string {
	switch choice {
	case llm.ToolChoiceAny:
		// 兼容档优先：已知不兼容就不必再花一次 400 去探测
		if deepSeekCompat(c.cfg.Model) {
			return "auto"
		}
		if _, bad := toolChoiceUnsupported.Load(toolChoiceCacheKey(c.cfg)); bad {
			return "auto"
		}
		return "required"
	case llm.ToolChoiceNone:
		return "none"
	default:
		return "auto"
	}
}

// markToolChoiceUnsupported 记住该模型不接受 required。
func markToolChoiceUnsupported(cfg Config) {
	toolChoiceUnsupported.Store(toolChoiceCacheKey(cfg), true)
}

// httpStatusError 网关返回非 200。带原始响应体，便于定位是哪个参数被拒。
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("openai http %d: %s", e.Status, truncate(e.Body, 500))
}

// isParamRejected 判断错误是否是「请求参数不被接受」。
//
// 只对 400 生效：只有 400 才可能是「这个网关不支持该参数」，
// 5xx / 鉴权 / 网络错误重试不同参数没有意义，还会掩盖真实故障。
// 响应体里带 invalid params / tool_choice 等字样时判定更稳，
// 但网关文案不统一（实测还有 code=20024 这种纯数字），
// 因此只要 tool_choice 是 required 且拿到 400 就降级 —— 最坏情况是多一次请求。
func isParamRejected(err error) bool {
	var he *httpStatusError
	if !errors.As(err, &he) {
		return false
	}
	return he.Status == http.StatusBadRequest
}

// ============================================================
// 网关差异：deepseek 兼容档
// ============================================================
//
// lkeap 网关的 deepseek/deepseek-v4-pro 有两处硬限制（2026-09-21 逐字段实测，
// 同一网关的 hy4-preview 两处都正常，所以必须按模型名区分，不能按网关一刀切）：
//
//  1. tool_choice 只认 "auto"："required" / 对象形式 / 不传 → 400 code 20024；
//  2. **请求只要出现 assistant 携带 tool_calls 就 400 code 20024** —— 与是否
//     下发 tools、是否带工具结果、是否流式、tool_calls 里的 name/arguments/
//     id/type/content 怎么写全都无关（换名换参换 id 都 400；空数组反而 200）。
//     响应侧不受影响：它照常吐 tool_calls（任务 4e3bdf04 首轮就拿到了 2 个调用）。
//     也就是说它「能产出函数调用，但不接受函数调用历史的原生回放」。
//
// 限制 1 由上一节的运行时探测兜底（换 auto 重试一次），但兼容档直接发 auto 更省；
// 限制 2 没有参数能解开，只能换表达：把「assistant(tool_calls) + tool 结果」
// 摊平成一条 user 文本消息。实测摊平成 user → 200 且能继续吐 tool_calls /
// submit_plan；摊平成 assistant → 400；保留 tool_calls → 400。
//
// 代价：模型看到的是「文本化的调用记录」而不是原生结构，语义上略有损失，
// 但这是当前唯一能让 deepseek 走完多轮工具循环的方式 —— 否则第二轮必 400，
// 任务会在重试 3 次后判死（正是 4e3bdf04 的现象）。

// deepSeekCompat 模型名是否命中 deepseek 兼容档（不区分大小写，子串匹配：
// deepseek/deepseek-v4-pro、DeepSeek-V3 等都覆盖）。
func deepSeekCompat(model string) bool {
	return strings.Contains(strings.ToLower(model), "deepseek")
}

// flattenToolHistory 把函数调用历史摊平成纯文本，供不接受 tool_calls 回放的模型使用。
//
// 变换规则：
//   - assistant(tool_calls) + 紧跟其后的若干 tool 结果 → 一条 user 消息，
//     按调用顺序列出「工具名 + 参数 + 返回」；
//   - 孤儿 tool 结果（找不到配对的 assistant 调用）→ 一条 user 消息，避免丢内容；
//   - 相邻同角色的 user 消息合并（摊平后极易连号，合并后语义更连贯，
//     也顺手满足「严格交替」类网关的要求）。
func flattenToolHistory(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	appendUser := func(text string) {
		if n := len(out); n > 0 && out[n-1].Role == llm.RoleUser {
			out[n-1].Content += "\n" + text
			return
		}
		out = append(out, llm.Message{Role: llm.RoleUser, Content: text})
	}

	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch {
		case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
			// 收集紧跟其后的工具结果（顺序与调用顺序一致）
			var results []string
			j := i + 1
			for j < len(msgs) && msgs[j].Role == llm.RoleTool {
				results = append(results, msgs[j].Content)
				j++
			}
			var sb strings.Builder
			if strings.TrimSpace(m.Content) != "" {
				sb.WriteString(m.Content + "\n")
			}
			sb.WriteString("【已完成的工具调用】（已执行，不要重复调用，直接基于下面的返回继续）\n")
			for k, tc := range m.ToolCalls {
				sb.WriteString(fmt.Sprintf("%d. %s 参数：%s\n", k+1, tc.Name, marshalArgs(tc.Arguments)))
				if k < len(results) {
					sb.WriteString("   返回：" + truncate(results[k], compatToolResultLimit) + "\n")
				} else {
					sb.WriteString("   返回：（无结果）\n")
				}
			}
			appendUser(sb.String())
			i = j - 1 // 工具结果已并入上面这条，跳过
		case m.Role == llm.RoleTool:
			appendUser("【工具结果】" + truncate(m.Content, compatToolResultLimit))
		default:
			out = append(out, m)
		}
	}
	return out
}

// compatToolResultLimit 摊平文本里单个工具结果的字符上限。
//
// 只是防止极端结果把上下文顶爆；压缩/预算由上层 contextmgr 负责，
// 这里不重复裁剪（正常工具结果远小于该值）。
const compatToolResultLimit = 8000

// ============================================================
// OpenAI 请求 / 响应结构
// ============================================================

type openAIMessage struct {
	Role string `json:"role"`
	// Content 纯文本时为 string，多模态（含图片）时为 []openAIContentPart。
	// 请求侧两种形态都可能出现；响应侧（模型输出）恒为 string 或 null。
	Content interface{} `json:"content,omitempty"`
	// ReasoningContent 推理模型的思考链（响应侧才有）。请求侧不下发。
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

// openAIContentPart 多模态 content 数组里的一个元素（text / image_url 二选一）。
type openAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

// openAIImageURL 图片 URL。数据用 data URI（data:image/...;base64,...）内联。
type openAIImageURL struct {
	URL string `json:"url"`
}

type openAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIFunctionDecl `json:"function"`
}

type openAIFunctionDecl struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type openAIRequest struct {
	Model      string          `json:"model"`
	Messages   []openAIMessage `json:"messages"`
	Tools      []openAITool    `json:"tools,omitempty"`
	ToolChoice interface{}     `json:"tool_choice,omitempty"`
	Stream     bool            `json:"stream"`
	// StreamOpts 流式附加选项。不下发时服务端不会回 usage（见下方类型说明）。
	StreamOpts  *openAIStreamOptions `json:"stream_options,omitempty"`
	Temperature float64              `json:"temperature,omitempty"`
	TopP        float64              `json:"top_p,omitempty"`
	MaxTokens   int                  `json:"max_tokens,omitempty"`
}

// openAIStreamOptions 流式附加选项。
//
// include_usage=true 时服务端在 [DONE] 之前多发一个 chunk，该 chunk 的
// choices 为空数组、usage 有值。**不显式要求就拿不到流式 usage** ——
// OpenAI 兼容协议（含腾讯混元 tokenhub）在流式下默认不返回 token 消耗，
// 这会让 task_turn.total_tokens 恒为 0。
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAIResponse 同时承载非流式与流式 chunk 的解析。
//
// 非流式：choices[0].message 含完整 content 与 tool_calls，usage 有效；
// 流式：每行 data 是一个 chunk，choices[0].delta 含增量，usage 通常为空。
type openAIResponse struct {
	Choices []struct {
		Message      openAIMessage `json:"message"`
		Delta        openAIDelta   `json:"delta"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type openAIDelta struct {
	Role             string                `json:"role,omitempty"`
	Content          string                `json:"content,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls,omitempty"`
}

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// ============================================================
// llm.Client 实现
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
		return nil, fmt.Errorf("openai stream: onDelta callback required")
	}
	return c.doChat(ctx, messages, tools, choice, opts, true, onDelta)
}

// doChat Chat / ChatWithTools / ChatWithToolsStream 的实际实现。
// stream 为 true 时走 SSE 逐行解析，否则一次性读全解析。
func (c *Client) doChat(
	ctx context.Context, messages []llm.Message, tools []llm.Tool,
	choice llm.ToolChoice, opts llm.Options, stream bool, onDelta func(llm.StreamDelta),
) (*llm.ChatResult, error) {
	if c.cfg.BaseURL == "" || c.cfg.APIKey == "" || c.cfg.Model == "" {
		return nil, fmt.Errorf("openai config incomplete: base_url/api_key/model required")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("openai chat: at least one message required")
	}

	// deepseek 兼容：不接受 assistant 携带 tool_calls，先把函数调用历史摊平
	if deepSeekCompat(c.cfg.Model) {
		messages = flattenToolHistory(messages)
	}

	reqBody := openAIRequest{
		Model:       c.cfg.Model,
		Messages:    messagesToOpenAI(messages),
		Stream:      stream,
		Temperature: opts.Temperature,
		TopP:        opts.TopP,
		MaxTokens:   opts.MaxTokens,
	}
	if stream {
		// 不显式要求就拿不到流式 usage，token 统计会全为 0
		reqBody.StreamOpts = &openAIStreamOptions{IncludeUsage: true}
	}

	if len(tools) > 0 {
		reqBody.Tools = make([]openAITool, 0, len(tools))
		for _, t := range tools {
			reqBody.Tools = append(reqBody.Tools, openAITool{
				Type: "function",
				Function: openAIFunctionDecl{
					Name:        t.Name,
					Description: t.Description,
					// OpenAI 兼容协议接受完整 JSON Schema，无需清洗
					Parameters: t.Parameters,
				},
			})
		}
		reqBody.ToolChoice = c.resolveToolChoice(choice)
	}

	result, err := c.send(ctx, reqBody, opts, stream, onDelta)
	if err == nil {
		return result, nil
	}

	// 强制工具调用（required）被网关拒绝时，降级重试一次 auto。
	//
	// 实测：lkeap 网关的 deepseek/deepseek-v4-pro 只认 tool_choice="auto"，
	// 传 "required" 直接 400（code 20024 invalid params），而同一网关的
	// hy4-preview 支持 required —— 能力是「模型 × 网关」级别的，既不能
	// 一刀切禁用 required，也不该为每个模型加一个配置开关。
	// 这里按「请求失败 → 换 auto 重试 → 记住结论」处理，新模型自动覆盖。
	// 降级只针对 400：只有它才可能是「参数不被接受」，5xx / 鉴权 / 网络
	// 错误重试同一个参数是浪费一次请求。
	sent, _ := reqBody.ToolChoice.(string)
	if sent != "required" || !isParamRejected(err) {
		return nil, err
	}
	markToolChoiceUnsupported(c.cfg)
	reqBody.ToolChoice = "auto"
	retry, rErr := c.send(ctx, reqBody, opts, stream, onDelta)
	if rErr != nil {
		// 两次都失败：把第一次的错误带上，否则「为什么降级了」无从查起
		return nil, fmt.Errorf("openai chat failed (tool_choice=required rejected, retried with auto: %v): %w", err, rErr)
	}
	return retry, nil
}

// send 发一次请求并解析响应（流式走 SSE，否则一次性读全）。
//
// 与 doChat 分离是为了让「换 tool_choice 重试」不用重复一遍请求组装；
// 非 200 一律先在这里拦下 —— 流式分支此前没判状态码，把 400 的 JSON
// 错误体当 SSE 解析，结果是「空内容 + nil error」，网关报错被整个吞掉
// （表现为规划 turn 成功但 output={}、token 全 0）。
func (c *Client) send(
	ctx context.Context, reqBody openAIRequest, opts llm.Options,
	stream bool, onDelta func(llm.StreamDelta),
) (*llm.ChatResult, error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}
	payload, err = mergeExtraBody(payload, opts.ExtraBody)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/chat/completions", strings.TrimRight(c.cfg.BaseURL, "/"))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build openai request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	// 流式请求不受整体 Timeout 限制（生成过程随时有数据），由 ctx 控制。
	cli := c.http
	if stream {
		cli = &http.Client{Transport: c.http.Transport}
	}

	resp, err := cli.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai http call: %w", err)
	}
	defer resp.Body.Close()

	// 状态码判定必须在解析之前、且流式与非流式共用：
	// 错误响应体是 JSON（不是 SSE），交给 parseStream 就是静默空结果。
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, &httpStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	if stream {
		return c.parseStream(resp.Body, onDelta)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read openai response: %w", err)
	}
	return parseResponse(body)
}

// mergeExtraBody 把 opts.ExtraBody 合并进已序列化的请求体。
//
// 走「结构体 → map → 回填 → 重新序列化」而不是给 openAIRequest 加字段，
// 是因为这些私有参数只有个别网关认（混元的 chat_template_kwargs），
// 放进结构体就会变成所有 OpenAI 兼容网关的固定字段 —— 遇到严格校验的
// 网关直接 400。按需注入才能让「不传」成为默认值。
//
// key 冲突时以 ExtraBody 为准：调用方显式指定的意图强于结构体默认值。
func mergeExtraBody(payload []byte, extra map[string]interface{}) ([]byte, error) {
	if len(extra) == 0 {
		return payload, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("unmarshal openai request for extra body: %w", err)
	}
	for k, v := range extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal extra body field %q: %w", k, err)
		}
		body[k] = raw
	}
	merged, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("remarkshal openai request: %w", err)
	}
	return merged, nil
}

// parseResponse 解析非流式响应的完整结果。
func parseResponse(body []byte) (*llm.ChatResult, error) {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal openai response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("openai error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty choices")
	}

	msg := resp.Choices[0].Message
	result := &llm.ChatResult{
		Text:      openAIContentString(msg.Content),
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

// toolCallAcc 流式工具调用的累积器。OpenAI 流式下 tool_calls 按 index
// 分片推送（首个分片带 id/name，后续只带 arguments 增量），需要按 index 累积。
type toolCallAcc struct {
	id      string
	name    string
	argsBuf strings.Builder
}

// streamIdleTimeout 流式读取的空闲超时：连续这么长时间没有收到任何新数据，
// 就判定流已死，返回错误交由上层（functional_call 执行器，maxRetries=3）重试，
// 而不是永久阻塞等 15 分钟看门狗强杀。
//
// 背景：lkeap 网关的 deepseek 在生成长 reasoning 后偶发「不发送 [DONE] 也不
// 关闭连接」，bufio.Scanner 会因此永久阻塞在 Scan() 上（现象：云平台已统计
// token，但 flowagent 侧 turn 一直 running、total_tokens=0）。
// 空闲超时只约束「连续多久没有新数据」，正常生成（持续有 delta 到达）不受影响。
const streamIdleTimeout = 2 * time.Minute

// idleTimeoutReader 在底层 reader 连续 idle 超过 timeout 时返回错误，
// 打破 bufio.Scanner 对半开连接的永久阻塞。
//
// 用 goroutine + channel 而非 SetReadDeadline：http.Response.Body 中间隔着
// bufio/chunked 层，拿不到底层 net.Conn，类型断言 SetReadDeadline 不可靠。
// 超时后仍阻塞在底层 Read 的 goroutine 会随 resp.Body.Close() 解除，泄漏有限。
type idleTimeoutReader struct {
	r       io.Reader
	timeout time.Duration
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		n, err := r.r.Read(p)
		ch <- readResult{n, err}
	}()
	select {
	case v := <-ch:
		return v.n, v.err
	case <-time.After(r.timeout):
		return 0, fmt.Errorf("openai stream idle timeout: no data for %v", r.timeout)
	}
}

// parseStream 解析 text/event-stream 响应，逐 chunk 回调文本增量，
// 累积工具调用分片，最终返回与阻塞式等价的 ChatResult。
func (c *Client) parseStream(body io.Reader, onDelta func(llm.StreamDelta)) (*llm.ChatResult, error) {
	body = &idleTimeoutReader{r: body, timeout: streamIdleTimeout}
	scanner := bufio.NewScanner(body)
	// 单行 data 可能很长（含大段 arguments），放大默认 buffer
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var textBuf strings.Builder
	var reasonBuf strings.Builder
	accs := map[int]*toolCallAcc{}
	maxIndex := -1
	var usage llm.Usage

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk openAIResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("unmarshal stream chunk: %w (data=%s)", err, truncate(data, 300))
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("openai stream error: %s", chunk.Error.Message)
		}

		// usage 的位置在不同网关上不一致，必须在 choices 判空之前取：
		//
		//   - 标准 OpenAI：只在最后一个 chunk（choices 为空数组）上回传；
		//     若放在下面的 continue 之后取，这个 chunk 会被整个跳过。
		//   - 腾讯混元 hy4-preview：**每个** chunk 都带 usage，但中间的全是 0，
		//     真实值在最后一个带 finish_reason=stop 的 chunk 上（choices 非空）。
		//
		// 因此这里用「非零才覆盖」而不是「非 nil 就覆盖」—— 否则混元中间那些
		// 零值 usage 会把已取到的真实值刷掉，token 统计又变回 0。
		// 同理，多个 chunk 都带非零值时以最后一个为准（累计值语义）。
		if chunk.Usage != nil && (chunk.Usage.TotalTokens > 0 ||
			chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0) {
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
			if tcd.ID != "" {
				acc.id = tcd.ID
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
		return nil, fmt.Errorf("scan stream response: %w", err)
	}

	result := &llm.ChatResult{Text: textBuf.String(), Reasoning: reasonBuf.String()}
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
	// 服务端未回传用量时保持零值 —— 上层把 0 理解为「未知」而非「没消耗」
	if usage.TotalTokens == 0 && (usage.PromptTokens > 0 || usage.CompletionTokens > 0) {
		// 少数网关只给拆分值不给合计，这里补齐，避免 total 列写不进去
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	result.Usage = usage
	return result, nil
}

// ============================================================
// 消息转换
// ============================================================

// messagesToOpenAI 把统一协议的 Message 列表转成 OpenAI messages。
//
// llm.Message 的 tool 消息只有 ToolName 没有 tool_call_id，而 OpenAI 协议
// 要求 tool 消息用 tool_call_id 关联到 assistant 的 tool_calls。这里在
// provider 内部按序为 assistant 的 tool_calls 生成 id，并用 name 队列
// 跟踪待消费的 id，tool 消息按 ToolName 消费队首 id 完成配对。
//
// 上层（agent_history）已通过 dropOrphanToolResults 保证 tool 消息前必有
// 对应 name 的 assistant tool_calls，因此这里按名配对是可靠的。
func messagesToOpenAI(messages []llm.Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(messages))
	// pendingID[name] 该 name 尚未被 tool 结果消费的 id 队列
	pendingID := map[string][]string{}
	seq := 0

	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, openAIMessage{Role: "system", Content: m.Content})

		case llm.RoleUser:
			out = append(out, openAIMessage{Role: "user", Content: openAIContent(m)})

		case llm.RoleAssistant:
			oam := openAIMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				id := fmt.Sprintf("call_%d", seq)
				seq++
				oam.ToolCalls = append(oam.ToolCalls, openAIToolCall{
					ID:   id,
					Type: "function",
					Function: openAIFunction{
						Name:      tc.Name,
						Arguments: marshalArgs(tc.Arguments),
					},
				})
				pendingID[tc.Name] = append(pendingID[tc.Name], id)
			}
			out = append(out, oam)

		case llm.RoleTool:
			var id string
			if q := pendingID[m.ToolName]; len(q) > 0 {
				id = q[0]
				pendingID[m.ToolName] = q[1:]
			}
			out = append(out, openAIMessage{
				Role:       "tool",
				ToolCallID: id,
				Content:    m.Content,
			})
		}
	}
	return out
}

// openAIContent 把统一协议的消息内容转成 OpenAI content 形态：
// 无图片 → 纯字符串（保持既有纯文本路径不变）；有图片 → text + image_url 数组。
func openAIContent(m llm.Message) interface{} {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := make([]openAIContentPart, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, openAIContentPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, openAIContentPart{
			Type:     "image_url",
			ImageURL: &openAIImageURL{URL: imageDataURL(img)},
		})
	}
	return parts
}

// openAIContentString 从响应侧 content 里取回纯文本（content 可能是 string / null / 数组）。
func openAIContentString(c interface{}) string {
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

// marshalArgs 把参数 map 序列化成 JSON 字符串（OpenAI 的 arguments 是字符串）。
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
