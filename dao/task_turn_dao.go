// Package dao — TaskTurn 原子调用明细数据访问
//
// 核心职责：
//  1. BeginTurn —— 在事务中原子递增 task.turn_count，用返回值作为 turn_seq 插入 task_turn。
//     这样保证并发下 turn_seq 不重复、不跳号，且 task.turn_count 恒等于该任务的 turn 总数。
//  2. FinishTurn / FailTurn —— 回填输出、状态与耗时。
//  3. 各类查询 —— 按任务回溯完整执行时间线。
package dao

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// TaskTurnDAO 原子调用明细数据访问对象
type TaskTurnDAO struct {
	Tx *gorm.DB
}

// ============================================================
// BeginTurn — 开启一个 turn
// ============================================================

// BeginTurnParams 开启一个 turn 所需的参数
type BeginTurnParams struct {
	// TaskID 所属任务（必填）
	TaskID string
	// ChatID 所属对话（必填）
	ChatID string
	// ParentTaskID 父任务 ID（工作流子任务时填）
	ParentTaskID string
	// TurnType 原子调用类型（必填）
	TurnType model.TurnType
	// Phase 该 turn 发生时任务所处的语义阶段（必填）。
	// 与 TurnType 正交：规划期同样会调 LLM 与工具。
	Phase model.TaskPhase
	// Stage 当前所处 stage
	Stage model.TaskStage
	// Round 循环轮次（functional_call ↔ tool_call 的第几轮）
	Round int32

	// Model LLM 调用时的模型名
	Model string
	// ToolName 工具调用时的工具名
	ToolName string
	// ServerName 工具调用时的 MCP server 名
	ServerName string

	// Input 输入快照。传 *model.TurnLLMInput 或 *model.TurnToolInput，
	// 由本方法负责 JSON 序列化。
	Input interface{}
}

// BeginTurn 开启一个新的 turn。
//
// 在同一个事务里完成两件事：
//  1. task / task_temp 的 turn_count += 1，并读回新值作为 turn_seq
//  2. 按该 turn_seq 插入一条 status=running 的 task_turn 记录
//
// 返回创建好的 TaskTurn（含 ID 与 TurnSeq），供后续 FinishTurn / FailTurn 回填。
func (d *TaskTurnDAO) BeginTurn(ctx context.Context, p BeginTurnParams) (*model.TaskTurn, error) {
	if p.TaskID == "" {
		return nil, fmt.Errorf("begin turn: task_id is required")
	}
	if p.TurnType == 0 {
		return nil, fmt.Errorf("begin turn: turn_type is required")
	}
	if p.Phase == 0 {
		return nil, fmt.Errorf("begin turn: phase is required")
	}

	inputJSON, err := marshalTurnPayload(p.Input)
	if err != nil {
		return nil, fmt.Errorf("begin turn: marshal input: %w", err)
	}

	now := time.Now()
	turn := &model.TaskTurn{
		TaskID:       p.TaskID,
		ChatID:       p.ChatID,
		ParentTaskID: p.ParentTaskID,
		TurnType:     p.TurnType,
		Phase:        int(p.Phase),
		Stage:        int(p.Stage),
		Status:       model.TurnStatusRunning,
		Round:        p.Round,
		Model:        p.Model,
		ToolName:     p.ToolName,
		ServerName:   p.ServerName,
		Input:        inputJSON,
		StartTime:    now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	runInTx := func(tx *gorm.DB) error {
		// 1. 原子递增 turn_count（终态表为准），拿到本次 turn 的序号
		if err := tx.Model(&model.Task{}).
			Where("task_id = ?", p.TaskID).
			UpdateColumn("turn_count", gorm.Expr("turn_count + 1")).Error; err != nil {
			return fmt.Errorf("incr task turn_count: %w", err)
		}

		var seq int32
		if err := tx.Model(&model.Task{}).
			Where("task_id = ?", p.TaskID).
			Select("turn_count").Scan(&seq).Error; err != nil {
			return fmt.Errorf("read back turn_count: %w", err)
		}
		if seq <= 0 {
			return fmt.Errorf("task %s not found or invalid turn_count %d", p.TaskID, seq)
		}

		// 2. 同步临时态表（存在则更新，不存在忽略——任务可能已到终态被清理）
		if err := tx.Model(&model.TaskTemp{}).
			Where("task_id = ?", p.TaskID).
			UpdateColumn("turn_count", seq).Error; err != nil {
			return fmt.Errorf("sync task_temp turn_count: %w", err)
		}

		// 3. 按该序号插入 turn 明细
		turn.TurnSeq = seq
		if err := tx.Create(turn).Error; err != nil {
			return fmt.Errorf("create task_turn: %w", err)
		}
		return nil
	}

	// 若外部已传入事务则复用，否则自开一个
	if d.Tx != nil {
		if err := runInTx(d.Tx.WithContext(ctx)); err != nil {
			return nil, err
		}
		return turn, nil
	}
	if err := DB().WithContext(ctx).Transaction(runInTx); err != nil {
		return nil, err
	}
	return turn, nil
}

// ============================================================
// FinishTurn / FailTurn — 结束一个 turn
// ============================================================

// FinishTurnParams 成功结束一个 turn 的参数
type FinishTurnParams struct {
	// Output 输出快照。传 *model.TurnLLMOutput 或 *model.TurnToolOutput
	Output interface{}
	// TotalTokens LLM 调用消耗的总 token（非 LLM 调用传 0）
	TotalTokens int
	// PromptTokens 输入 token。上下文管理的度量对象，见 turnTokens 的说明
	PromptTokens int
	// CompletionTokens 输出 token
	CompletionTokens int
}

// tokenUsage 从参数里取出三元组（LLM 调用），非 LLM 调用返回零值
func (p FinishTurnParams) tokenUsage() turnTokens {
	return turnTokens{
		prompt:     p.PromptTokens,
		completion: p.CompletionTokens,
		total:      p.TotalTokens,
	}.normalize()
}

// FinishTurn 标记 turn 成功结束，回填输出、耗时与 token 消耗。
func (d *TaskTurnDAO) FinishTurn(ctx context.Context, turnID int64, p FinishTurnParams) error {
	outputJSON, err := marshalTurnPayload(p.Output)
	if err != nil {
		return fmt.Errorf("finish turn: marshal output: %w", err)
	}
	return d.closeTurn(ctx, turnID, model.TurnStatusSuccess, outputJSON, "", p.tokenUsage())
}

// FailTurn 标记 turn 失败，回填错误原因与耗时。
// output 可为 nil；若失败前已拿到部分输出，可一并传入便于排查。
//
// token 从 output 快照里自动提取：LLM 已经返回过响应就意味着费用已经产生，
// 后续校验失败不该让这笔消耗在统计里凭空消失。
func (d *TaskTurnDAO) FailTurn(ctx context.Context, turnID int64, reason error, output interface{}) error {
	outputJSON, err := marshalTurnPayload(output)
	if err != nil {
		// 序列化失败不应掩盖真实错误，降级为空输出
		outputJSON = ""
	}
	msg := ""
	if reason != nil {
		msg = reason.Error()
	}
	return d.closeTurn(ctx, turnID, model.TurnStatusFailed, outputJSON, msg, extractTurnTokens(output))
}

// turnTokens 一次 turn 的 token 消耗三元组。
//
// 三个值必须一起落库，不能只留 total：
//   - prompt     —— 系统提示 + 历史消息 + 工具 schema 全算在这里，是「上下文
//     水位」唯一正确的度量；上下文管理（遮蔽 / 摘要）要压的就是这一项。
//   - completion —— 模型输出。压它只会让回答变短，与上下文无关。
//   - total      —— 成本口径。input / output 单价通常差 3–5 倍，只有一个
//     total 既算不清成本，也看不出「输入暴涨」还是「输出暴涨」。
type turnTokens struct {
	prompt     int
	completion int
	total      int
}

// normalize 补全缺失的合计值。
//
// 只补 total（prompt + completion 已知时），不反向拆分 ——
// provider 未回传用量时拆分无从推断，保持 0 表示「未知」，而不是硬凑两个数出来。
func (t turnTokens) normalize() turnTokens {
	if t.total == 0 && (t.prompt > 0 || t.completion > 0) {
		t.total = t.prompt + t.completion
	}
	return t
}

// extractTurnTokens 从输出快照里取 token 消耗；非 LLM 输出或 nil 返回零值
func extractTurnTokens(output interface{}) turnTokens {
	if out, ok := output.(*model.TurnLLMOutput); ok && out != nil {
		return turnTokens{
			prompt:     out.PromptTokens,
			completion: out.CompletionTokens,
			total:      out.TotalTokens,
		}.normalize()
	}
	return turnTokens{}
}

// closeTurn 收尾一个 turn：写状态、输出、错误、耗时与 token 消耗
func (d *TaskTurnDAO) closeTurn(
	ctx context.Context, turnID int64,
	status model.TurnStatus, outputJSON, errMsg string, tokens turnTokens,
) error {
	db := d.getDB().WithContext(ctx)

	// 先取 start_time 计算耗时，避免依赖 DB 端函数导致方言差异
	var turn model.TaskTurn
	if err := db.Select("id", "start_time").Where("id = ?", turnID).First(&turn).Error; err != nil {
		return fmt.Errorf("close turn: load turn %d: %w", turnID, err)
	}

	now := time.Now()
	updates := map[string]interface{}{
		"status":       status,
		"end_time":     now,
		"time_cost_ms": now.Sub(turn.StartTime).Milliseconds(),
		"updated_time": now,
	}
	if outputJSON != "" {
		updates["output"] = outputJSON
	}
	if errMsg != "" {
		updates["error_msg"] = errMsg
	}
	tokens = tokens.normalize()
	// 逐列判断而非整体判断：provider 只回传了 total（prompt/completion 缺失）时，
	// 已有的那一列照样要写进去，不能因为另一个是 0 就整批丢弃。
	if tokens.total > 0 {
		updates["total_tokens"] = tokens.total
	}
	if tokens.prompt > 0 {
		updates["prompt_tokens"] = tokens.prompt
	}
	if tokens.completion > 0 {
		updates["completion_tokens"] = tokens.completion
	}

	if err := db.Model(&model.TaskTurn{}).Where("id = ?", turnID).Updates(updates).Error; err != nil {
		return fmt.Errorf("close turn %d: %w", turnID, err)
	}
	return nil
}

// ============================================================
// 查询
// ============================================================

// ListTurnsByTaskID 按 turn_seq 升序返回任务的完整执行时间线
func (d *TaskTurnDAO) ListTurnsByTaskID(ctx context.Context, taskID string) ([]*model.TaskTurn, error) {
	db := d.getDB().WithContext(ctx)
	var turns []*model.TaskTurn
	if err := db.Where("task_id = ?", taskID).
		Order("turn_seq ASC").Find(&turns).Error; err != nil {
		return nil, fmt.Errorf("list turns by task_id: %w", err)
	}
	return model.NormalizeTurns(turns), nil
}

// ListTurnsByTaskIDForThink 返回任务的完整执行时间线，但不加载 tool_call 的 output 大字段。
//
// 详情页（GetConversationDetail）的 think_process 只需要工具的 summary/入参
// （在 input 里）与元信息，不需要工具执行结果 output。而 run_command 等工具
// 可能把几百 MB 的日志写进 output，全量加载会让 detail 接口在远程 DB 上
// 传输数百 MB、耗时数分钟。这里 Omit output，让 describeToolTurn 退化为
// 「成功，无返回内容」，摘要（模型说明 / 工具名）不受影响。
//
// 但 LLM 调用（functional_call，含规划期）的 output 不能省：describePlanningTurn
// 靠 output.tool_calls 判断「提交规划 / 规划前调研」，describeLLMTurn 靠它判断
// 「请求工具 / 给出回复」。省略后二者都会退化，规划期调研会被错写成
// 「任务规划完成」。LLM 的 output 只是 text+tool_calls 的小字段，回填成本可忽略。
func (d *TaskTurnDAO) ListTurnsByTaskIDForThink(ctx context.Context, taskID string) ([]*model.TaskTurn, error) {
	db := d.getDB().WithContext(ctx)
	var turns []*model.TaskTurn
	if err := db.Omit("output").
		Where("task_id = ?", taskID).
		Order("turn_seq ASC").Find(&turns).Error; err != nil {
		return nil, fmt.Errorf("list turns by task_id (light): %w", err)
	}

	// 收集 LLM 调用的主键，单独回填其 output（小字段）。
	var llmIDs []int64
	for _, t := range turns {
		if t.TurnType.IsLLMCall() {
			llmIDs = append(llmIDs, t.ID)
		}
	}
	if len(llmIDs) > 0 {
		var outs []struct {
			ID     int64
			Output string
		}
		if err := db.Model(&model.TaskTurn{}).
			Where("id IN ?", llmIDs).
			Select("id", "output").
			Find(&outs).Error; err != nil {
			return nil, fmt.Errorf("backfill llm output: %w", err)
		}
		outputByID := make(map[int64]string, len(outs))
		for _, o := range outs {
			outputByID[o.ID] = o.Output
		}
		for _, t := range turns {
			if t.TurnType.IsLLMCall() {
				t.Output = outputByID[t.ID]
			}
		}
	}
	return model.NormalizeTurns(turns), nil
}

// ListTurnsByChatID 按对话维度返回所有 turn（用于对话级回溯）
func (d *TaskTurnDAO) ListTurnsByChatID(ctx context.Context, chatID string) ([]*model.TaskTurn, error) {
	db := d.getDB().WithContext(ctx)
	var turns []*model.TaskTurn
	if err := db.Where("chat_id = ?", chatID).
		Order("created_time ASC, turn_seq ASC").Find(&turns).Error; err != nil {
		return nil, fmt.Errorf("list turns by chat_id: %w", err)
	}
	return model.NormalizeTurns(turns), nil
}

// GetLatestChatContextTurn 返回对话（仅顶层任务，排除工作流/deep_search 子任务）
// 最近一次带分块 token 明细（input.context_blocks）的 LLM 调用 turn。
//
// 「上下文用量」弹窗的数据源：它反映的是最近一次真实送出的输入构成，
// 比重新估算便宜且更真实。分块明细由 actuator 组装消息时顺手写进
// input 快照（见 actuator 的 assembleTurnMessages / contextBlocks.toSnapshot），
// 早期 turn 没有该字段，用 JSON 包含性过滤掉。
//
// 找不到时返回 (nil, nil) —— 新对话还没有任何 LLM 调用属正常情况，不算错误。
func (d *TaskTurnDAO) GetLatestChatContextTurn(ctx context.Context, chatID string) (*model.TaskTurn, error) {
	db := d.getDB().WithContext(ctx)
	var turn model.TaskTurn
	err := db.Where("chat_id = ? AND parent_task_id = ''", chatID).
		Where("turn_type IN ?", []model.TurnType{
			model.TurnTypeFunctionalCall, model.TurnTypePlanningLegacy,
		}).
		Where("input LIKE '%\"context_blocks\"%'").
		Order("id DESC").First(&turn).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest chat context turn: %w", err)
	}
	return model.NormalizeTurn(&turn), nil
}

// GetTurn 按主键获取单条 turn
func (d *TaskTurnDAO) GetTurn(ctx context.Context, turnID int64) (*model.TaskTurn, error) {
	db := d.getDB().WithContext(ctx)
	var turn model.TaskTurn
	if err := db.Where("id = ?", turnID).First(&turn).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get turn: %w", err)
	}
	return model.NormalizeTurn(&turn), nil
}

// GetLatestTurn 获取任务最新一个 turn
func (d *TaskTurnDAO) GetLatestTurn(ctx context.Context, taskID string) (*model.TaskTurn, error) {
	db := d.getDB().WithContext(ctx)
	var turn model.TaskTurn
	if err := db.Where("task_id = ?", taskID).
		Order("turn_seq DESC").First(&turn).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest turn: %w", err)
	}
	return model.NormalizeTurn(&turn), nil
}

// GetLatestTurnByType 获取任务指定类型的最新一个 turn。
//
// 查询 functional_call 时会一并匹配存量的 turn_type=1（legacy planning）——
// 那些记录本质就是 functional_call，漏掉会让 tool_call 阶段找不到
// 上一次 LLM 输出，把正常任务判成失败。
func (d *TaskTurnDAO) GetLatestTurnByType(
	ctx context.Context, taskID string, turnType model.TurnType,
) (*model.TaskTurn, error) {
	db := d.getDB().WithContext(ctx)
	var turn model.TaskTurn
	if err := db.Where("task_id = ?", taskID).Where(turnTypeCond(turnType)).
		Order("turn_seq DESC").First(&turn).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest turn by type: %w", err)
	}
	return model.NormalizeTurn(&turn), nil
}

// GetLatestLLMTurn 获取任务最近一次 LLM 调用的 turn，不区分语义阶段。
//
// 取代此前「分别查 functional_call 与 planning 再比 turn_seq」的两次查询：
// 拆出 phase 列后，规划期与执行期的 LLM 调用 turn_type 都是 functional_call，
// 一条 SQL 即可，也不会再漏掉任何一边。
func (d *TaskTurnDAO) GetLatestLLMTurn(
	ctx context.Context, taskID string,
) (*model.TaskTurn, error) {
	return d.GetLatestTurnByType(ctx, taskID, model.TurnTypeFunctionalCall)
}

// CountTurnsByType 统计任务某类型的 turn 数量。
// 典型用途：判断 functional_call ↔ tool_call 的循环轮次是否超限。
func (d *TaskTurnDAO) CountTurnsByType(
	ctx context.Context, taskID string, turnType model.TurnType,
) (int64, error) {
	db := d.getDB().WithContext(ctx)
	var count int64
	if err := db.Model(&model.TaskTurn{}).
		Where("task_id = ?", taskID).Where(turnTypeCond(turnType)).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count turns by type: %w", err)
	}
	return count, nil
}

// CountTurnsByTypeAndPhase 统计任务在指定语义阶段内某类型的 turn 数量。
//
// 用于按 phase 独立计算工具调用轮次：规划期的调研预算与执行期的
// 干活预算互不侵占（见 actuator 侧 planningMaxToolCallRound）。
func (d *TaskTurnDAO) CountTurnsByTypeAndPhase(
	ctx context.Context, taskID string, turnType model.TurnType, phase model.TaskPhase,
) (int64, error) {
	db := d.getDB().WithContext(ctx)
	var count int64
	if err := db.Model(&model.TaskTurn{}).
		Where("task_id = ?", taskID).
		Where(turnTypeCond(turnType)).
		Where(phaseCond(phase)).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count turns by type and phase: %w", err)
	}
	return count, nil
}

// CountToolCallRoundsByPhase 统计任务在指定语义阶段内已完成的工具调用「轮数」。
//
// 与 CountTurnsByTypeAndPhase（数 tool_call turn 的条数）不同：一轮工具调用
// 可能并行执行多个工具（产生多条 tool_call turn），用条数会高估轮次。
// 轮次是内存 hint.ToolCallRound 的持久化镜像 —— 每个 tool_call 阶段收尾时 +1
// （见 tool_call_actuator 的 IncrToolCallRound），与 turn 表 round 列（从 0 起
// 编号）一一对应，故用 COUNT(DISTINCT round) 还原。
//
// 只统计已结束（非 running）的 turn：进程重启恢复时，卡在 running 的孤儿 turn
// 属于「尚未完成的那一轮」，不该计入已完成轮数。
func (d *TaskTurnDAO) CountToolCallRoundsByPhase(
	ctx context.Context, taskID string, phase model.TaskPhase,
) (int64, error) {
	db := d.getDB().WithContext(ctx)
	var count int64
	if err := db.Model(&model.TaskTurn{}).
		Distinct("round").
		Where("task_id = ?", taskID).
		Where(turnTypeCond(model.TurnTypeToolCall)).
		Where(phaseCond(phase)).
		Where("status <> ?", model.TurnStatusRunning).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count tool call rounds by phase: %w", err)
	}
	return count, nil
}

// CountToolCallTurns 统计任务已执行的工具调用**总次数**（跨轮累计）。
//
// 与 CountToolCallRoundsByPhase（数轮次）的区别正是这里要它的原因：一轮里
// 模型常并行发多个工具调用，每个调用一条 turn（一条 turn 一个 tool_name），
// 于是「轮次数」远小于「调用次数」。只限轮次等于没限总量。
//
// 只数 tool_name 非空的 turn：LLM turn 与工具结果 turn 都不算一次调用。
func (d *TaskTurnDAO) CountToolCallTurns(ctx context.Context, taskID string) (int64, error) {
	db := d.getDB().WithContext(ctx)
	var count int64
	if err := db.Model(&model.TaskTurn{}).
		Where("task_id = ?", taskID).
		Where("tool_name IS NOT NULL AND tool_name <> ''").
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count tool call turns: %w", err)
	}
	return count, nil
}

// HasToolCallTurnAfter 判断指定 turn_seq 之后是否已存在「执行完」的 tool_call turn。
//
// 幂等场景：tool_call 执行器拿到上一轮 LLM turn 的待执行工具后，若发现
// 这些工具已经执行过（该 LLM turn 之后已有 tool_call turn），说明上次进程在
// 「工具已执行完、但轮次 hint 落库 / 状态流转」之前崩溃。此时跳过执行，
// 避免同一批工具被重复调用 —— 工具调用往往有副作用（写文件、发请求、起进程），
// 重复执行会产生脏数据甚至对外造成二次影响。
//
// 为什么只统计 success / failed、排除 interrupted / running：
// 进程重启恢复会把卡在 running 的 turn 标成 interrupted（MarkRunningTurnsInterrupted），
// 但「被中断」不等于「执行完」—— 尤其 deep_search 这类同步阻塞工具（BeginTurn 后
// 等子任务最长 15 分钟），崩溃窗口极大。若把 interrupted 也算「已执行」，主任务
// 重放时会误判跳过 deep_search，导致子任务没人等、结论没人收，整个搜索废弃。
// 只有真正执行完（成功/失败已落库）才值得跳过。
func (d *TaskTurnDAO) HasToolCallTurnAfter(
	ctx context.Context, taskID string, afterSeq int32,
) (bool, error) {
	db := d.getDB().WithContext(ctx)
	var count int64
	if err := db.Model(&model.TaskTurn{}).
		Where("task_id = ?", taskID).
		Where("turn_type = ?", model.TurnTypeToolCall).
		Where("turn_seq > ?", afterSeq).
		Where("status IN ?", []model.TurnStatus{
			model.TurnStatusSuccess, model.TurnStatusFailed,
		}).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("has tool call turn after: %w", err)
	}
	return count > 0, nil
}

// MarkRunningTurnsInterrupted 把任务所有卡在 running 的 turn 收口为 interrupted。
//
// 进程重启后，上一进程发出的 LLM / 工具请求已无人接管，对应 turn 会永远停在
// running（end_time 为空）。若不清理，前端会一直显示「执行中」的僵尸记录。
// 恢复 stale 任务时调用本方法，把这些孤儿 turn 标记为 interrupted 收口。
//
// 返回受影响的行数。
func (d *TaskTurnDAO) MarkRunningTurnsInterrupted(ctx context.Context, taskID string) (int64, error) {
	db := d.getDB().WithContext(ctx)
	now := time.Now()
	res := db.Model(&model.TaskTurn{}).
		Where("task_id = ?", taskID).
		Where("status = ?", model.TurnStatusRunning).
		Updates(map[string]interface{}{
			"status":       model.TurnStatusInterrupted,
			"end_time":     now,
			"updated_time": now,
			"error_msg":    "进程重启，调用被中断",
		})
	if res.Error != nil {
		return 0, fmt.Errorf("mark running turns interrupted: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// turnTypeCond 构造 turn_type 匹配条件，functional_call 额外兼容 legacy 取值。
func turnTypeCond(turnType model.TurnType) *gorm.DB {
	if turnType == model.TurnTypeFunctionalCall {
		return DB().Where("turn_type IN ?",
			[]model.TurnType{model.TurnTypeFunctionalCall, model.TurnTypePlanningLegacy})
	}
	return DB().Where("turn_type = ?", turnType)
}

// phaseCond 构造 phase 匹配条件，并兼容 phase 列上线前的存量记录。
//
// 存量记录 phase=0：turn_type=1 的属规划期，其余属执行期
// （与 model.NormalizeTurn 的反推规则保持一致，否则同一条记录
// 在「列表展示」与「计数统计」两处会被归到不同阶段）。
func phaseCond(phase model.TaskPhase) *gorm.DB {
	if phase == model.PhasePlanning {
		return DB().Where("phase = ? OR (phase = 0 AND turn_type = ?)",
			phase, model.TurnTypePlanningLegacy)
	}
	return DB().Where("phase = ? OR (phase = 0 AND turn_type <> ?)",
		phase, model.TurnTypePlanningLegacy)
}

// TokenSums 一组 token 求和结果（三列分别聚合）
type TokenSums struct {
	Prompt     int64
	Completion int64
	Total      int64
}

// SumTokenColumnsByTaskID 按任务统计 prompt / completion / total 三列之和。
//
// 只统计本任务自身的 turn —— 子任务（deep_search 等）是独立的 task_id，
// 不在这里。要含子任务请用 SumTaskTokensTree。
func (d *TaskTurnDAO) SumTokenColumnsByTaskID(ctx context.Context, taskID string) (TokenSums, error) {
	db := d.getDB().WithContext(ctx)
	var row struct {
		Prompt     int64
		Completion int64
		Total      int64
	}
	// COALESCE 兜底：该任务还没有任何 turn 时 SUM 返回 NULL，
	// 直接 Scan 进 int64 会报错，包一层让空集返回 0。
	if err := db.Model(&model.TaskTurn{}).
		Where("task_id = ?", taskID).
		Select(`COALESCE(SUM(prompt_tokens), 0) AS prompt,
			COALESCE(SUM(completion_tokens), 0) AS completion,
			COALESCE(SUM(total_tokens), 0) AS total`).
		Scan(&row).Error; err != nil {
		return TokenSums{}, fmt.Errorf("sum token columns: %w", err)
	}
	return TokenSums{Prompt: row.Prompt, Completion: row.Completion, Total: row.Total}, nil
}

// SumTokensByTaskID 统计任务自身 turn 累计消耗的 token 总量（不含子任务）
func (d *TaskTurnDAO) SumTokensByTaskID(ctx context.Context, taskID string) (int64, error) {
	sums, err := d.SumTokenColumnsByTaskID(ctx, taskID)
	if err != nil {
		return 0, err
	}
	return sums.Total, nil
}

// DeleteTurnsByTaskID 物理删除某任务的全部 turn 明细。
//
// 对话任务重试时调用：重试语义是「从头重新执行」，若保留旧 turn，
// 历史重建（buildFullHistory）会把上一轮已成功的工具调用/回复带入新的
// LLM 上下文，导致「续跑」而非「重来」。清空后任务回到干净的
// system + user 起点。task_turn 无软删除（无 DeletedAt），Delete 即物理删除。
func (d *TaskTurnDAO) DeleteTurnsByTaskID(ctx context.Context, taskID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("task_id = ?", taskID).Delete(&model.TaskTurn{}).Error; err != nil {
		return fmt.Errorf("delete turns by task_id: %w", err)
	}
	return nil
}

// ============================================================
// 内部辅助
// ============================================================

func (d *TaskTurnDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}

// marshalTurnPayload 序列化输入/输出快照；nil 返回空串
func marshalTurnPayload(v interface{}) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
