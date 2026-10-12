// Package actuator — LLM 流式调用辅助
//
// 把「LLM 文本增量 → Hub delta 事件 → SSE → 前端逐字渲染」这条链路里
// 的 actuator 侧职责收拢到一个函数，planning / functional_call 两个
// 执行器共用，避免各自重复类型断言与事件发布逻辑。
package actuator

import (
	"context"
	"time"

	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/stream"
)

// stopCheckInterval 停止感知的检查间隔。
//
// 停止任务发生在 server 进程（只删 task_temp、改 status），无法跨进程 cancel
// scheduler 里正在阻塞等 LLM 的 goroutine。这里靠一个伴生 goroutine 周期检查
// 任务是否仍活跃，一旦发现被停止就 cancel，让 LLM 调用尽快中断而非等它跑完
// （否则会出现「任务已停止、function call 仍继续执行完成」的现象）。
const stopCheckInterval = 2 * time.Second

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
	// 停止感知：派生一个可 cancel 的 ctx，伴生 goroutine 周期检查任务是否仍活跃，
	// 被停止则 cancel 正在进行的 LLM 调用。正常路径下 cancel 只在本函数返回前触发，
	// 不会误伤仍在执行的任务。
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCheckDone := make(chan struct{})
	go func() {
		defer close(stopCheckDone)
		ticker := time.NewTicker(stopCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-guardCtx.Done():
				return
			case <-ticker.C:
				if !isTaskActive(guardCtx, taskID) {
					cancel()
					return
				}
			}
		}
	}()

	sc, ok := client.(llm.StreamClient)
	var result *llm.ChatResult
	var err error
	if !ok {
		// 不支持流式：退回阻塞式，行为不变
		result, err = client.ChatWithTools(guardCtx, messages, tools, choice, opts)
	} else {
		result, err = sc.ChatWithToolsStream(guardCtx, messages, tools, choice, opts, func(d llm.StreamDelta) {
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

	// 收尾 stop-check goroutine：cancel 触发其退出，等它结束避免 goroutine 泄漏。
	cancel()
	<-stopCheckDone
	return result, err
}
