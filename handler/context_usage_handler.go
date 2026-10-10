// Package handler — 对话上下文用量查询
//
// 「上下文用量」弹窗的后端数据源：返回该对话最近一次 LLM 调用时
// 真实送出的输入分块明细（system / tools / 工作流摘要 / 压缩摘要 /
// 历史详情 / 本轮输入与当前任务）+ 当前模型的上下文上限与各档水位。
//
// 为什么读最近一次调用而不是现场重新估算：
//   - 估算要加载 MCP 工具列表（真实网络调用）+ 重建历史 + tiktoken 全量编码，
//     成本高且与真实请求有偏差；
//   - 分块明细由 actuator 组装消息时顺手写进 turn input 快照
//     （见 actuator 的 assembleTurnMessages / contextBlocks.toSnapshot），
//     读出来就是「当时真的发了什么」，零额外计算。
//
// 代价是对话从未发过消息（或老数据没有该字段）时无数据可读 —— 前端
// 据 has_data 显示引导文案即可。
package handler

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
)

// ============================================================
// 请求 / 响应结构体
// ============================================================

// --- 查询对话上下文用量 ---
type GetContextUsageRequest struct {
	ChatID string `json:"chat_id" binding:"required"`
}

// ContextBlocksInfo 输入上下文的分块 token 占用（与 model.TurnContextBlocks
// 对应，但不暴露 ContextLimit —— 上限在响应顶层统一给）。
type ContextBlocksInfo struct {
	SystemPrompt    int `json:"system_prompt"`
	Tools           int `json:"tools"`
	WorkflowSummary int `json:"workflow_summary"`
	Compacted       int `json:"compacted"`
	Detail          int `json:"detail"`
	UserAndCurrent  int `json:"user_and_current"`
	Total           int `json:"total"`
}

// ContextUsageTurnInfo 数据来源 turn 的元信息。
type ContextUsageTurnInfo struct {
	TaskID     string `json:"task_id"`
	TurnSeq    int32  `json:"turn_seq"`
	Phase      int    `json:"phase"`
	PhaseName  string `json:"phase_name"`
	Model      string `json:"model,omitempty"`
	StatusName string `json:"status_name"`
	CreateTs   int64  `json:"create_ts"`
	// PromptTokens provider 回报的真实输入 token。
	// 与 Blocks.Total 的关系：后者是 tiktoken 估算（组装时算的），
	// 前者是 provider 计费口径。两者相差一般在个位数百分比内，
	// 同时展示便于观察估算器的偏差。
	PromptTokens int `json:"prompt_tokens"`
}

// GetContextUsageResponse 上下文用量响应。
type GetContextUsageResponse struct {
	ChatID string `json:"chat_id"`
	// ModelName 对话当前所选模型的名称（展示用）
	ModelName string `json:"model_name,omitempty"`
	// ContextLimit 当前模型的最大输入 token 上限（口径=最大输入，非总窗口）
	ContextLimit int `json:"context_limit"`
	// Trigger / Target 压缩的触发线与目标线（token），供前端画水位刻度
	Trigger int `json:"trigger"`
	Target  int `json:"target"`
	// HasData 是否有可展示的分块数据。
	// false = 该对话还没有带分块明细的 LLM 调用（新对话或老数据）。
	HasData bool                  `json:"has_data"`
	Blocks  *ContextBlocksInfo    `json:"blocks,omitempty"`
	Turn    *ContextUsageTurnInfo `json:"turn,omitempty"`
}

// ============================================================
// Handler
// ============================================================

// GetContextUsage 查询对话的上下文用量（最近一次 LLM 调用的分块明细）。
func GetContextUsage(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*GetContextUsageRequest)
	c := ctx.Request.Context()

	limit, modelName, err := resolveChatModelLimit(c, r.ChatID)
	if err != nil {
		return nil, err
	}
	th := contextmgr.NewThresholds(limit)

	resp := &GetContextUsageResponse{
		ChatID:       r.ChatID,
		ModelName:    modelName,
		ContextLimit: th.Limit,
		Trigger:      th.Trigger,
		Target:       th.Target,
	}

	turn, err := (&dao.TaskTurnDAO{}).GetLatestChatContextTurn(c, r.ChatID)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return resp, nil
	}

	blocks, ok := extractContextBlocks(turn.Input)
	if !ok {
		// 理论上到不了：DAO 已按 JSON 包含性过滤。兜底按无数据处理。
		return resp, nil
	}

	resp.HasData = true
	resp.Blocks = blocks
	resp.Turn = &ContextUsageTurnInfo{
		TaskID:       turn.TaskID,
		TurnSeq:      turn.TurnSeq,
		Phase:        turn.Phase,
		PhaseName:    turn.TurnPhase().String(),
		Model:        turn.Model,
		StatusName:   turn.Status.String(),
		CreateTs:     turn.StartTime.UnixMilli(),
		PromptTokens: turn.PromptTokens,
	}
	return resp, nil
}

// GetContextUsageBinder 绑定请求体
func GetContextUsageBinder() interface{} {
	return &GetContextUsageRequest{}
}

// ============================================================
// 内部辅助
// ============================================================

// resolveChatModelLimit 解析对话当前所选模型的上下文上限与名称。
//
// 口径与 actuator 侧 resolveChatContextLimit 一致：优先取模型配置里的
// context_limit（最大输入 token），漏配/未配置时按模型名查内置表兜底，
// 再不行回落 DefaultContextLimit —— 宁可保守也不能把水位线画高。
func resolveChatModelLimit(ctx context.Context, chatID string) (int, string, error) {
	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, chatID)
	if err != nil {
		return 0, "", err
	}

	confDAO := &dao.ModelConfigDAO{}
	var conf *model.ModelConfig
	if conv != nil && conv.ModelID != "" {
		if conf, err = confDAO.GetByID(ctx, conv.ModelID); err != nil {
			return 0, "", err
		}
	}
	if conf == nil {
		// 对话未显式选模型：回落默认模型；连默认都没有就只有名称为空的上限兜底
		if conf, err = confDAO.GetDefault(ctx); err != nil {
			return 0, "", err
		}
	}
	if conf == nil {
		return contextmgr.DefaultContextLimit, "", nil
	}
	return contextmgr.ResolveContextLimit(conf.ContextLimit, conf.Model), conf.Name, nil
}

// extractContextBlocks 从 turn input 快照 JSON 里抽出分块明细。
//
// 快照按「前端直接可用」的结构存（context_blocks 字段），这里解开
// 类型断言的脏活，取不到任何一个数值字段时视为无数据。
func extractContextBlocks(inputJSON string) (*ContextBlocksInfo, bool) {
	if inputJSON == "" {
		return nil, false
	}
	var snap struct {
		ContextBlocks *ContextBlocksInfo `json:"context_blocks"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &snap); err != nil || snap.ContextBlocks == nil {
		return nil, false
	}
	b := snap.ContextBlocks
	if b.Total == 0 && b.SystemPrompt == 0 && b.Tools == 0 && b.UserAndCurrent == 0 &&
		b.Detail == 0 && b.Compacted == 0 && b.WorkflowSummary == 0 {
		return nil, false
	}
	return b, true
}
