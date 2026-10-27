// Package llm 提供统一的 LLM 调用中间层。
//
// 设计目标：
//   - 上层业务只依赖 llm.Client 接口，与具体 provider 解耦
//   - 各 provider（Gemini/OpenAI/Hunyuan/...）在子包中实现 Client 接口
//   - 请求 / 响应统一使用 Message + Options 结构，规避各家协议差异
package llm

import "context"

// Role 消息角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool 工具执行结果消息。用于 agent loop 把工具返回回喂给模型。
	RoleTool Role = "tool"
)

// Message 一条对话消息
//
// 三种形态：
//  1. 普通文本消息        —— Role + Content
//  2. assistant 发起工具调用 —— Role=assistant + ToolCalls（Content 通常为空）
//  3. 工具执行结果        —— Role=tool + ToolName + Content（工具返回内容）
//
// 文本消息可叠加图片（Images 非空）成为多模态消息；只有 user 消息会携带图片。
type Message struct {
	Role    Role
	Content string
	// Images 图片输入（仅 user 消息）。非空时表示这是一条多模态消息，
	// provider 会按各自协议把图片内联进请求。
	Images []ImagePart
	// ToolCalls assistant 消息携带的工具调用请求（形态 2）
	ToolCalls []ToolCall `json:",omitempty"`
	// ToolName 工具结果消息对应的工具名（形态 3）
	ToolName string `json:",omitempty"`
	// SourceTurnID 该消息的来源 turn（task_turn.id）。
	//
	// 仅用于上下文压缩的记账：压掉一批消息后要把「被压的是哪些 turn」
	// 写进压缩水位，后续才能按 turn 取回原始记录（下钻通道）。
	//
	// 纯内部字段，不参与 JSON 序列化（turn 快照里不出现）、也不下发给
	// provider —— 各协议的请求体是按字段显式组装的，不会带出去。
	// 只在「从 task_turn 重建历史」的路径上写入，重建之外的消息为零值。
	SourceTurnID int64 `json:"-"`
	// ToolCallIndex 该 tool 结果消息对应 assistant 声明的第几个 tool_call（0-based）。
	//
	// 工具调用并发执行后 turn_seq 是完成顺序、未必等于声明顺序；回喂 LLM 时
	// 需按此恢复声明顺序，否则「参数（如 read_file 的 path）与返回内容」错位。
	// 仅 tool 结果消息使用，纯内部排序字段，不参与序列化（同 SourceTurnID）。
	ToolCallIndex int `json:"-"`
}

// ImagePart 一张图片的多模态输入。
type ImagePart struct {
	// MimeType 图片 MIME 类型，如 image/png、image/jpeg。
	MimeType string
	// Data base64 编码的图片字节（不含 "data:image/...;base64," 前缀）。
	Data string
}

// Options 生成参数
type Options struct {
	Temperature float64 // 采样温度
	TopP        float64 // nucleus sampling
	TopK        int     // top-k 采样
	MaxTokens   int     // 最大生成 token 数（0 表示不限制）
	// ExtraBody 合并进请求体的额外字段，用于下发各 provider 的私有参数
	// （例如混元的 chat_template_kwargs）。key 冲突时以这里的值为准。
	//
	// 只由 openai 协议 provider 实现；其余 provider 忽略它 —— 私有字段
	// 一旦发错网关就是 400，宁可不生效也不能瞎传。
	ExtraBody map[string]interface{}
}

// DefaultOptions 默认生成参数
func DefaultOptions() Options {
	return Options{
		Temperature: 0.7,
		TopP:        1.0,
		TopK:        32,
	}
}

// Tool 描述一个可供模型调用的函数（function-calling / tools）
//
// Parameters 使用 JSON Schema 描述参数结构（object 类型 + properties + required）。
// Provider 内部会把它翻译成各家协议对应的字段。
type Tool struct {
	Name        string                 // 函数名，模型返回 tool_call 时会带上
	Description string                 // 函数用途说明
	Parameters  map[string]interface{} // JSON Schema（type: object, properties, required, ...）
}

// ToolChoice 控制模型如何选择工具
type ToolChoice string

const (
	// ToolChoiceAuto 由模型自主决定是否调用工具（默认）
	ToolChoiceAuto ToolChoice = "auto"
	// ToolChoiceAny 强制模型必须调用工具集里的某一个（Gemini: mode=ANY / OpenAI: required）
	ToolChoiceAny ToolChoice = "any"
	// ToolChoiceNone 禁止调用工具
	ToolChoiceNone ToolChoice = "none"
)

// ToolCall 模型发起的一次工具调用
type ToolCall struct {
	Name      string                 // 被调函数名
	Arguments map[string]interface{} // 参数（已解析）
	// Signature provider 侧的推理签名（Gemini thoughtSignature）。
	//
	// Gemini 3 强制要求：回放含 functionCall 的历史时，必须把签名
	// 原样放回它原本所在的那个 part，否则 400。并行调用时只有第一个
	// functionCall 带签名，后续的为空 —— 不要伪造。
	//
	// 放在 ToolCall 而非 Message 上，正是为了保证「签名跟着它所属的
	// part 走」这一语义。其他 provider 不需要时留空即可。
	Signature string `json:",omitempty"`
}

// Usage 一次调用的 token 消耗。
//
// Provider 未返回用量时各字段为 0 —— 调用方应把 0 理解为「未知」，
// 而不是「真的没消耗」。
type Usage struct {
	// PromptTokens 输入 token（含 system prompt、历史消息、工具 schema）
	PromptTokens int
	// CompletionTokens 输出 token（模型生成的文本与工具调用）
	CompletionTokens int
	// TotalTokens 总计。provider 未直接给出时由前两者相加得到。
	TotalTokens int
}

// ChatResult ChatWithTools 的返回结果
//
// Text 与 ToolCalls 通常二选一：
//   - 有 Tool 调用意图时 ToolCalls 非空、Text 通常为空
//   - 纯文本回复时 Text 非空、ToolCalls 为空
//
// 但并非互斥 —— 部分模型会同时给出解释文本和工具调用，两者都应保留。
type ChatResult struct {
	Text      string
	ToolCalls []ToolCall
	// Reasoning 推理模型的思考链内容（reasoning_content）。
	//
	// 推理模型（deepseek-reasoner / gpt-5.6 / o 系列等）把生成拆成
	// 「思考链 + 正文」两段。思考链对上层是过程而非答案，正常路径下上层
	// 只用 Text/ToolCalls；但思考链是诊断「正文为空」的关键 —— 模型只吐
	// 思考不吐正文时，Text 为空，思考链就是唯一能说明「它到底想了什么」的线索。
	// 非推理模型或 provider 不返回该字段时为空。
	Reasoning string
	// Usage token 消耗。provider 不支持时为零值。
	Usage Usage
	// FirstTokenMs 首 token 耗时（毫秒）：从发起请求到收到首个有内容的增量。
	// 仅流式 provider 记录；非流式（聚合返回）或无内容输出时为 0。
	FirstTokenMs int64
}

// Client LLM 客户端接口。所有 provider 必须实现。
//
// Chat 输入一组消息，返回模型的完整回复文本（不使用 tools）。
// ChatWithTools 输入消息 + tools schema，返回结构化结果（可能是文本或工具调用）。
// 具体协议（stream / non-stream、鉴权方式、模型名等）由 provider 内部处理。
type Client interface {
	Chat(ctx context.Context, messages []Message, opts Options) (string, error)
	ChatWithTools(ctx context.Context, messages []Message, tools []Tool, choice ToolChoice, opts Options) (*ChatResult, error)
	Name() string // provider 名称，用于日志/指标
}

// StreamDelta 流式输出的一段增量。
//
// Text 承载文本增量（服务于「最终回答文本」的逐 token 展示）。
// Reasoning 承载思考链增量（推理模型的 reasoning_content，供调试页观察原始思考链）。
// ToolCallDelta 承载工具调用分片增量（服务于调试页观察「完整的 raw 流式输出」）。
// 三者在一次回调中通常只有一个非空：模型要么在吐文本，要么在吐思考，要么在吐工具调用分片。
type StreamDelta struct {
	Text string // 增量文本（本轮为空说明没有新文本，例如纯工具调用轮次）
	// Reasoning 思考链增量（本轮为空说明没有新思考，非推理模型始终为空）。
	Reasoning string
	// ToolCallDelta 本轮的工具调用分片；本轮没有工具调用分片时为 nil。
	ToolCallDelta *ToolCallDelta
}

// ToolCallDelta 流式工具调用的一个分片。
//
// OpenAI/Azure 流式下 tool_calls 按 index 分片推送：首个分片带 id + function.name
// （arguments 通常为空字符串），后续分片只带 function.arguments 增量。上层需要按
// Index 归并，把 arguments 增量拼成完整 JSON 才能得到一次调用的最终参数。
type ToolCallDelta struct {
	Index int    // 工具调用序号（一次 assistant 回复里的第几个 tool_call）
	ID    string // 调用 id（仅首片非空）
	Name  string // 函数名（仅首片非空）
	Args  string // arguments 增量（原始 JSON 字符串片段，未解析）
}

// StreamClient 支持流式输出的客户端（可选能力）。
//
// 并非所有 provider 都支持真流式（例如某些内部网关会把流式聚合后一次性返回），
// 因此流式能力独立成接口而非塞进 Client：上层通过类型断言探测，
// 支持则走流式低延迟路径，不支持则退回阻塞式 ChatWithTools。
type StreamClient interface {
	Client
	// ChatWithToolsStream 语义与 ChatWithTools 一致，区别是生成过程通过
	// onDelta 逐段回调文本增量。返回的 ChatResult 与阻塞式等价
	// （Text 为完整文本、ToolCalls 为完整工具调用、Usage 为总消耗）。
	ChatWithToolsStream(
		ctx context.Context,
		messages []Message,
		tools []Tool,
		choice ToolChoice,
		opts Options,
		onDelta func(StreamDelta),
	) (*ChatResult, error)
}
