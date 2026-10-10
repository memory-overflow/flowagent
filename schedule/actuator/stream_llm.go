// Package actuator — LLM 流式调用辅助
//
// 把「LLM 文本增量 → Hub delta 事件 → SSE → 前端逐字渲染」这条链路里
// 的 actuator 侧职责收拢到一个函数，planning / functional_call 两个
// 执行器共用，避免各自重复类型断言与事件发布逻辑。
package actuator

import (
	"context"

	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/stream"
)

// chatWithToolsStreaming 调 LLM，优先走流式。
//
// provider 实现了 llm.StreamClient（例如 openai/混元，stream=true 逐 token
// 推送）时，通过 ChatWithToolsStream 把文本增量实时发布为 delta 事件，
// 前端据此逐字渲染；provider 不支持流式（例如 gemini 网关聚合返回）时，
// 退回阻塞式 ChatWithTools，行为与改造前完全一致。
//
// 返回的 ChatResult 与阻塞式等价（完整文本 / 工具调用 / token 用量），
// 因此调用方后续逻辑无需感知本次是否走了流式。
func chatWithToolsStreaming(
	ctx context.Context,
	client llm.Client,
	taskID string,
	turnID int64,
	turnSeq int32,
	messages []llm.Message,
	tools []llm.Tool,
	choice llm.ToolChoice,
	opts llm.Options,
) (*llm.ChatResult, error) {
	sc, ok := client.(llm.StreamClient)
	if !ok {
		// 不支持流式：退回阻塞式，行为不变
		return client.ChatWithTools(ctx, messages, tools, choice, opts)
	}

	return sc.ChatWithToolsStream(ctx, messages, tools, choice, opts, func(d llm.StreamDelta) {
		// 一个 delta 事件可同时携带文本增量与工具调用分片，但通常只有其一非空。
		data := map[string]interface{}{}
		if d.Text != "" {
			data["text"] = d.Text
		}
		if d.ToolCallDelta != nil {
			data["tool_call"] = map[string]interface{}{
				"index": d.ToolCallDelta.Index,
				"id":    d.ToolCallDelta.ID,
				"name":  d.ToolCallDelta.Name,
				"args":  d.ToolCallDelta.Args,
			}
		}
		if len(data) == 0 {
			return
		}
		stream.Publish(stream.Event{
			Type:    stream.EventTypeDelta,
			TaskID:  taskID,
			TurnID:  turnID,
			TurnSeq: turnSeq,
			Data:    data,
		})
	})
}
