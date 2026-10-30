// Package actuator — turn 快照构建辅助
//
// 各 actuator 在 BeginTurn / FinishTurn 时复用这些函数，
// 把 llm 包的请求/响应结构转成 model 包的 turn 快照结构，统一落库格式。
package actuator

import (
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// buildLLMTurnInput 把一次 LLM 请求转为 turn 输入快照。
//
// systemPrompt 从 messages 里自动提取（role=system 的第一条），
// 其余消息按原顺序保留，便于完整回放当时的上下文。
//
// 注意：必须完整搬运 Message 的三种形态（文本 / assistant 工具调用 / 工具结果），
// 只拷 Content 会让 assistant 那条在调试页显示成 content:""，等于丢了这一步的语义。
func buildLLMTurnInput(
	modelName string,
	messages []llm.Message,
	tools []llm.Tool,
	choice llm.ToolChoice,
	opts llm.Options,
) *model.TurnLLMInput {
	in := &model.TurnLLMInput{
		Model:      modelName,
		ToolChoice: string(choice),
		Options: map[string]interface{}{
			"temperature": opts.Temperature,
			"top_p":       opts.TopP,
			"top_k":       opts.TopK,
			"max_tokens":  opts.MaxTokens,
		},
	}

	// 系统提示词单存到 SystemPrompt 字段，不在 Messages 里重复。
	// 原因：
	//   1. 调试页 dump JSON 时 system_prompt 和 messages[0].content 是同一段文本的两份复制，
	//      容易误以为"重复两遍"。
	//   2. provider 真实请求体里 system 与 messages 是同级但不同概念（OpenAI 走 messages.role=system，
	//      Gemini 走独立 system_instruction），SystemPrompt 字段对齐这个语义。
	// 实际生效顺序：遇 role=system 的消息时只填 SystemPrompt，跳过 Messages 拷贝。
	in.Messages = make([]model.TurnMessage, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			if in.SystemPrompt == "" {
				in.SystemPrompt = m.Content
			}
			continue
		}
		tm := model.TurnMessage{
			Role:     string(m.Role),
			Content:  m.Content,
			ToolName: m.ToolName,
		}
		// assistant 发起工具调用时 Content 天然为空，语义全在 ToolCalls 里。
		// 不搬过来的话调试页只能看到 {"role":"assistant","content":""}。
		if len(m.ToolCalls) > 0 {
			tm.ToolCalls = make([]model.TurnToolCallReq, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				tm.ToolCalls = append(tm.ToolCalls, model.TurnToolCallReq{
					Name:      c.Name,
					Arguments: c.Arguments,
					Signature: c.Signature,
				})
			}
		}
		in.Messages = append(in.Messages, tm)
	}

	in.Tools = make([]string, 0, len(tools))
	in.ToolSchemas = make([]model.TurnToolSchema, 0, len(tools))
	for _, t := range tools {
		in.Tools = append(in.Tools, t.Name)
		// 落全量 schema：参数定义是排查「模型传错参」的关键依据。
		// 真实请求一直带 schema，只是过去快照没存，看起来像没传。
		in.ToolSchemas = append(in.ToolSchemas, model.TurnToolSchema{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return in
}

// buildLLMTurnOutput 把一次 LLM 响应转为 turn 输出快照。
//
// Text / ToolCalls / Usage 三者都要落库：模型可能同时给出解释文本和工具调用，
// token 用量则是排查成本与上下文膨胀的依据。
func buildLLMTurnOutput(result *llm.ChatResult) *model.TurnLLMOutput {
	if result == nil {
		return &model.TurnLLMOutput{}
	}
	out := &model.TurnLLMOutput{
		Text:             result.Text,
		Reasoning:        result.Reasoning,
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
		TotalTokens:      result.Usage.TotalTokens,
	}
	if len(result.ToolCalls) > 0 {
		out.ToolCalls = make([]model.TurnToolCallReq, 0, len(result.ToolCalls))
		for _, c := range result.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, model.TurnToolCallReq{
				Name:      c.Name,
				Arguments: c.Arguments,
				// 签名必须落库：历史是从 task_turn 重建的，
				// 丢了签名下一轮回放会被 Gemini 判 400
				Signature: c.Signature,
			})
		}
	}
	return out
}

// buildToolTurnInput 把一次工具调用转为 turn 输入快照
func buildToolTurnInput(toolName, serverName, summary string, args map[string]interface{}, index int) *model.TurnToolInput {
	return &model.TurnToolInput{
		ToolName:   toolName,
		ServerName: serverName,
		Arguments:  args,
		Summary:    summary,
		Index:      index,
	}
}

// maxToolOutputStoreChars 单次工具返回落库的最大字符数（按 rune 计）。
//
// 回喂 LLM 时已按 maxToolResultChars(4000) 截断，落库本可以存更宽以保留调试信息；
// 但 run_command 这类工具一旦读到超大日志文件（几百 MB），把完整输出写进
// task_turn.output 会让详情页等读取路径在远程 DB 上传输数百 MB、耗时数分钟。
// 这里设一个远大于回喂上限、又足以避免撑爆 DB 的存储上限，超长时截断并标注。
const maxToolOutputStoreChars = 64 * 1024

// buildToolTurnOutput 把一次工具返回转为 turn 输出快照。
// 对超长返回做存储截断：不影响回喂 LLM（那一步已有更紧的截断），
// 只防止把异常大的工具输出（如整份日志）塞进 turn.output。
func buildToolTurnOutput(content string, isError bool, raw interface{}) *model.TurnToolOutput {
	if n := len([]rune(content)); n > maxToolOutputStoreChars {
		r := []rune(content)
		content = string(r[:maxToolOutputStoreChars]) + "\n…(工具输出过大，已截断)…"
	}
	return &model.TurnToolOutput{
		Content: content,
		IsError: isError,
		Raw:     raw,
	}
}
