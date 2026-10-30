// Package container — Stage 任务容器
//
// StageTaskContainer 实现 light-task-scheduler 的 TaskContainer 接口。
//
// 与固定配置 stage 的方式不同，本容器采用**动态状态流转**：
//   - 容器只绑定自己负责扫描的 stage（scanStage），用于 GetWaitingTask / GetRunningTask
//   - 下一个阶段由 model.TaskStage.NextStage(hint) 状态机动态计算
//   - hint 由执行器在执行过程中写入 StageTransitionStore，容器在流转时读取
//   - 任何中间阶段失败 → 任务统一变为 TaskStatusFailed终态（不重试到其他阶段）
package container

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/stream"

	framework "github.com/memory-overflow/light-task-scheduler"
)

// ============================================================
// StageTransitionStore — 执行器与容器之间的决策传递
// ============================================================

// StageTransitionStore 存储各任务的流转决策提示。
// 执行器执行完成后写入 hint，容器在 ToSuccessStatus 时读取，
// 通过状态机计算出下一个阶段。
type StageTransitionStore struct {
	mu    sync.RWMutex
	hints map[string]*model.StageTransitionHint
}

// globalTransitionStore 全局共享的流转决策存储。
// 所有 stage 的容器和执行器共用同一份，实现跨调度器的决策传递。
var globalTransitionStore = &StageTransitionStore{
	hints: make(map[string]*model.StageTransitionHint),
}

// GetTransitionStore 获取全局流转决策存储（供执行器使用）
func GetTransitionStore() *StageTransitionStore {
	return globalTransitionStore
}

// SetHint 设置任务的流转决策
func (s *StageTransitionStore) SetHint(taskID string, hint *model.StageTransitionHint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hints[taskID] = hint
}

// GetHint 获取任务的流转决策
func (s *StageTransitionStore) GetHint(taskID string) *model.StageTransitionHint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hints[taskID]
}

// IncrToolCallRound 递增任务的工具调用轮次
func (s *StageTransitionStore) IncrToolCallRound(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hint, ok := s.hints[taskID]
	if !ok {
		hint = &model.StageTransitionHint{}
		s.hints[taskID] = hint
	}
	hint.ToolCallRound++
}

// SetDeferred 标记任务「本轮延后重排」。
//
// 与 Incr* 系列一样走「取已有 hint 再改」，避免覆盖执行器已写入的
// ToolCallRound / PlanSubmitted 等决策 —— 那些字段在 hint 恢复时会被重建，
// 但同一轮内的决策不能因为一次延后标记而丢失。
func (s *StageTransitionStore) SetDeferred(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hint, ok := s.hints[taskID]
	if !ok {
		hint = &model.StageTransitionHint{}
		s.hints[taskID] = hint
	}
	hint.Deferred = true
}

// ClearDeferred 清掉任务的延后标记。
//
// 必须由执行器在「本轮真正开始执行前」调用：hint 是常驻内存的，上一轮的
// Deferred 若不清理，下一轮正常执行完也会带着它流转 —— 任务会被反复
// 拨回 wait 重排，永远跑不完。
func (s *StageTransitionStore) ClearDeferred(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if hint, ok := s.hints[taskID]; ok {
		hint.Deferred = false
	}
}

// IncrConsecutiveFailures 记一次调用指纹的失败（熔断计数）。
//
// 熔断口径是「同一工具 + 同样的参数」连续失败：fingerprint 相同则计数 +1，
// 不同指纹各自独立计数（换工具/换参数是新的尝试，不背旧账）。
// ConsecutiveFailures 始终维护为所有指纹计数的最大值，供消费端直接读。
func (s *StageTransitionStore) IncrConsecutiveFailures(taskID, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hint, ok := s.hints[taskID]
	if !ok {
		hint = &model.StageTransitionHint{}
		s.hints[taskID] = hint
	}
	if hint.FailStreaks == nil {
		hint.FailStreaks = make(map[string]int)
	}
	hint.FailStreaks[fingerprint]++
	if hint.FailStreaks[fingerprint] > hint.ConsecutiveFailures {
		hint.ConsecutiveFailures = hint.FailStreaks[fingerprint]
	}
}

// ResetConsecutiveFailures 清零某一调用指纹的连续失败计数（该调用成功了）。
// 只清本指纹：其他仍在重试的失败调用不受影响，继续累计。
func (s *StageTransitionStore) ResetConsecutiveFailures(taskID, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hint, ok := s.hints[taskID]
	if !ok {
		return
	}
	if hint.FailStreaks == nil {
		return
	}
	delete(hint.FailStreaks, fingerprint)
	max := 0
	for _, v := range hint.FailStreaks {
		if v > max {
			max = v
		}
	}
	hint.ConsecutiveFailures = max
}

// UpdateToolCallFingerprint 更新任务的工具调用指纹，维护连续重复计数。
//
// 死循环检测：fp 与上一轮一致则连续重复计数 +1，不一致则重置为 1。
// 空指纹（fp == ""）表示本轮没有工具调用，此时清零计数并清空指纹，
// 避免「有调用 → 无调用 → 有相同调用」被误判成连续重复。
func (s *StageTransitionStore) UpdateToolCallFingerprint(taskID, fp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hint, ok := s.hints[taskID]
	if !ok {
		hint = &model.StageTransitionHint{}
		s.hints[taskID] = hint
	}
	if fp == "" {
		hint.LastToolCallFingerprint = ""
		hint.RepeatedToolCallCount = 0
		return
	}
	if hint.LastToolCallFingerprint == fp {
		hint.RepeatedToolCallCount++
	} else {
		hint.RepeatedToolCallCount = 1
	}
	hint.LastToolCallFingerprint = fp
}

// ClearHint 清理任务的流转决策（任务结束时调用）
func (s *StageTransitionStore) ClearHint(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hints, taskID)
}

// ============================================================
// hint 恢复 —— 内存态丢失后的兜底
// ============================================================

// resolveHint 取任务的流转决策，内存里没有时从 task_turn 表恢复。
//
// 为什么需要恢复：
// hint 是纯内存态，但任务状态在 DB。以下场景内存 hint 会缺失：
//   - 调度器进程重启，内存态清空但任务仍在 DB 里等待推进
//   - 多副本部署，任务的相邻两个 stage 被不同副本接管
//
// 若此时 hint 为 nil，状态机会走默认分支：
//   - functional_call_doing → finished（本该继续 tool_call 却提前完成）
//   - ToolCallRound 归零（循环上限失效，可能无限循环）
//
// task_turn 表是持久化的完整执行记录，据此可重建关键决策。
//
// 接收整个 temp 而非仅 taskID：轮次统计要按 task.phase 隔离，
// 只传 taskID 就得为此再查一次 task 表。
func (s *StageTransitionStore) resolveHint(
	ctx context.Context, temp *model.TaskTemp, currentStage model.TaskStage,
) *model.StageTransitionHint {
	taskID := temp.TaskID
	if hint := s.GetHint(taskID); hint != nil {
		return hint
	}

	hint := &model.StageTransitionHint{
		MaxToolCallRound: config.CurrentAgentSettings().SingleAgentMaxToolCallRounds,
	}

	turnDAO := &dao.TaskTurnDAO{}

	// 已完成的 tool_call 轮数即当前轮次，按 phase 隔离统计。
	//
	// 必须按 phase 数：规划期调研与执行期干活各有独立预算，
	// 混在一起会让执行期背着规划期的轮次提前收口。
	// 拆出 phase 列之前这里做不到，只能接受该偏差。
	phase := model.TaskPhase(temp.Phase)
	if phase == 0 {
		phase = model.PhaseExecuting
	}
	if round, err := turnDAO.CountToolCallRoundsByPhase(
		ctx, taskID, phase); err == nil {
		hint.ToolCallRound = int(round)
	} else {
		log.Printf("[transition-store] task %s count tool_call rounds failed: %v", taskID, err)
	}

	// 若当前正处于 functional_call_doing，需要知道最后一次 LLM 调用的意图。
	//
	// 规划期与执行期共享该 stage，两者的 LLM 调用 turn_type 都是 functional_call
	// （差别在 phase 列），因此一次查询就能拿到真正的最后一步：
	//   - 它调了 submit_plan(mode=workflow)     → PlanDelegated（活已交给工作流，收口）
	//   - 它调了 submit_plan(mode=direct_reply) → PlanDelegated（已直接作答，收口）
	//   - 它调了 submit_plan(mode=single_agent) → PlanSubmitted（切执行期，回 functional_call）
	//   - 它调了其他真实工具                     → NeedToolCall
	//   - 都没有                                → 收口 finished
	if currentStage == model.StageFunctionalCallDoing {
		t, err := turnDAO.GetLatestLLMTurn(ctx, taskID)
		if err != nil {
			log.Printf("[transition-store] task %s get latest llm turn failed: %v", taskID, err)
		}
		if t != nil {
			hint.PlanDelegated, hint.PlanSubmitted, hint.NeedToolCall = llmTurnIntent(t)
		}
	}

	log.Printf("[transition-store] task %s hint recovered from DB "+
		"(stage=%s plan_delegated=%v plan_submitted=%v need_tool=%v round=%d)",
		taskID, currentStage, hint.PlanDelegated, hint.PlanSubmitted,
		hint.NeedToolCall, hint.ToolCallRound)

	s.SetHint(taskID, hint)
	return hint
}

// llmTurnIntent 解析一次 LLM turn 的意图：提交了规划（哪种模式），还是请求了真实工具。
//
// submit_plan 是规划期的协议工具，不能算成「需要 tool_call」——
// 那会让 tool_call 阶段去找一个不存在的工具，记一条无意义的失败 turn。
//
// 必须区分 submit_plan 的 mode：workflow 模式下活已移交给对话绑定的工作流任务，
// 当前对话任务应当收口。若都当成 PlanSubmitted，进程重启后这个任务会被拉回
// functional_call 再跑一遍单 Agent —— 同一件事做两遍，用户还会看到两份回复。
func llmTurnIntent(t *model.TaskTurn) (planDelegated, planSubmitted, needToolCall bool) {
	if t.Output == "" {
		return false, false, false
	}
	var out model.TurnLLMOutput
	if err := json.Unmarshal([]byte(t.Output), &out); err != nil {
		return false, false, false
	}
	for _, tc := range out.ToolCalls {
		if tc.Name == model.SubmitPlanToolName {
			planSubmitted = true
			// workflow（移交工作流）与 direct_reply（已直接作答）都是当轮收口
			if mode, ok := tc.Arguments["mode"].(string); ok && model.PlanModeFinishesTask(mode) {
				planDelegated = true
			}
			continue
		}
		needToolCall = true
	}
	// 同时出现时以「已提交规划」为准，与执行器的分流顺序保持一致
	if planDelegated {
		return true, false, false
	}
	if planSubmitted {
		return false, true, false
	}
	return false, false, needToolCall
}

// ============================================================
// StageTaskContainer — 动态状态流转的任务容器
// ============================================================

// StageTaskContainer 实现 framework.TaskContainer 接口。
//
// 每个调度器实例绑定一个 wait 阶段（scanWaitStage）用于扫描待调度任务，
// 其对应的 doing 阶段由 scanWaitStage.ToDoingStage() 推导。
// 流转到下一阶段时由状态机动态决策，无需在构造时硬编码。
type StageTaskContainer struct {
	// scanWaitStage 本容器负责扫描的 wait 阶段
	scanWaitStage model.TaskStage
	// TaskName 调度器名称（日志用）
	TaskName string
	// store 流转决策存储
	store *StageTransitionStore
	// taskTypeFilter 若非 nil，扫描时只处理该类型的任务。
	//
	// 用于「同一 stage 按任务类型拆成多个调度器」的场景：functional_call
	// 阶段同时承载对话任务与工作流子任务，但两者需要的单次 LLM 调用超时
	// 差异巨大（子任务可能一次性生成大文件），必须拆成两个调度器实例
	// 分别赋予不同 timeout。此时用本字段把任务流按 task_type 分流。
	taskTypeFilter *model.TaskType
}

// NewStageTaskContainer 创建 Stage 任务容器。
//
// 只需指定本容器负责的 wait 阶段，doing 阶段自动推导，
// 下一个阶段由状态机 model.TaskStage.NextStage 动态计算。
//
// taskType 传 nil 表示不限类型；传非 nil 则容器只扫描该 task_type 的任务，
// 供同一 stage 拆多个调度器实例时做分流。
func NewStageTaskContainer(
	scanWaitStage model.TaskStage, taskName string, taskType *model.TaskType,
) *StageTaskContainer {
	return &StageTaskContainer{
		scanWaitStage:  scanWaitStage,
		TaskName:       taskName,
		store:          globalTransitionStore,
		taskTypeFilter: taskType,
	}
}

// WaitStage 返回本容器扫描的 wait 阶段
func (c *StageTaskContainer) WaitStage() model.TaskStage {
	return c.scanWaitStage
}

// DoingStage 返回本容器扫描的 doing 阶段
func (c *StageTaskContainer) DoingStage() model.TaskStage {
	return c.scanWaitStage.ToDoingStage()
}

// ============================================================
// framework.TaskContainer 接口实现
// ============================================================

// AddTask 添加任务到容器（任务创建由外部 handler 完成）
func (c *StageTaskContainer) AddTask(ctx context.Context, task framework.Task) error {
	return nil
}

// GetRunningTask 获取本阶段所有运行中的任务
func (c *StageTaskContainer) GetRunningTask(ctx context.Context) ([]framework.Task, error) {
	db := dao.DB().WithContext(ctx)
	var temps []model.TaskTemp
	query := db.Where("stage = ? AND status = ?",
		int(c.DoingStage()), model.TaskStatusRunning)
	if c.taskTypeFilter != nil {
		query = query.Where("task_type = ?", *c.taskTypeFilter)
	}
	if err := query.Find(&temps).Error; err != nil {
		return nil, fmt.Errorf("[%s] get running tasks: %w", c.TaskName, err)
	}

	tasks := make([]framework.Task, 0, len(temps))
	for i := range temps {
		tasks = append(tasks, c.toFrameworkTask(&temps[i], framework.TASK_STATUS_RUNNING))
	}
	return tasks, nil
}

// GetRunningTaskCount 获取本阶段运行中的任务数（并发控制用）
func (c *StageTaskContainer) GetRunningTaskCount(ctx context.Context) (int32, error) {
	db := dao.DB().WithContext(ctx)
	var count int64
	query := db.Model(&model.TaskTemp{}).
		Where("stage = ? AND status = ?",
			int(c.DoingStage()), model.TaskStatusRunning)
	if c.taskTypeFilter != nil {
		query = query.Where("task_type = ?", *c.taskTypeFilter)
	}
	if err := query.Count(&count).Error; err != nil {
		return 0, fmt.Errorf("[%s] count running tasks: %w", c.TaskName, err)
	}
	return int32(count), nil
}

// GetWaitingTask 获取本阶段等待调度的任务
//
// 延后重排的任务（delay_until 未到期）在这里被直接跳过：
// 调度器每 1s 扫一次，若不跳过，等待前置任务的任务会以 1s 的节奏被反复
// 唤醒 —— 每轮都要重建整段对话历史并计量 token，纯属空转。
// 到期（或从未被延后）的行照常进入队列，无需额外的「唤醒」动作。
func (c *StageTaskContainer) GetWaitingTask(ctx context.Context, limit int32) ([]framework.Task, error) {
	db := dao.DB().WithContext(ctx)
	var temps []model.TaskTemp
	query := db.Where("stage = ? AND status = ?",
		int(c.scanWaitStage), model.TaskStatusQueued)
	if c.taskTypeFilter != nil {
		query = query.Where("task_type = ?", *c.taskTypeFilter)
	}
	query = query.Where("delay_until IS NULL OR delay_until <= ?", time.Now())
	if err := query.Order("created_time ASC").Limit(int(limit)).Find(&temps).Error; err != nil {
		return nil, fmt.Errorf("[%s] get waiting tasks: %w", c.TaskName, err)
	}

	tasks := make([]framework.Task, 0, len(temps))
	for i := range temps {
		tasks = append(tasks, c.toFrameworkTask(&temps[i], framework.TASK_STATUS_WAITING))
	}
	return tasks, nil
}

// ToRunningStatus 将任务拨到 running。
//
// 正常路径（框架 scheduleOnce 对 GetWaitingTask 取到的 wait 任务）：wait → doing。
// 重试路径（框架 updateOnce 对失败任务重新 Start 后再次调用本方法）：传入的
// 已是 doing 任务。此时绝不能用 NextStage 重算 —— NextStage 对 doing 走的是
// ActionSuccess 分支，会把 stage 拨回 wait（甚至 finished），再配上这里硬编码的
// status=Running，就会写出「stage=wait + status=Running」的脏数据：GetWaitingTask
// 只查 status=queued、GetRunningTask 只查 stage=doing，两边都扫不到，任务从此卡死。
//
// 因此：wait → 拨到 doing；doing → 保持 stage 不动，只刷新 running 状态与
// start_time（重试看门狗重新计时）。
func (c *StageTaskContainer) ToRunningStatus(ctx context.Context, task *framework.Task) (*framework.Task, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return task, fmt.Errorf("[%s] task item is not *model.TaskTemp", c.TaskName)
	}

	currentStage := model.TaskStage(temp.Stage)

	nextStage := currentStage
	if currentStage.IsWaitStage() {
		nextStage = currentStage.ToDoingStage()
	}

	return c.updateStage(ctx, task, currentStage, nextStage,
		model.TaskStatusRunning, framework.TASK_STATUS_RUNNING)
}

// ToSuccessStatus 将任务流转到下一阶段（动态：通过状态机计算）
func (c *StageTaskContainer) ToSuccessStatus(ctx context.Context, task *framework.Task) (*framework.Task, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return task, fmt.Errorf("[%s] task item is not *model.TaskTemp", c.TaskName)
	}

	// 动态计算下一阶段：doing → 下一个 wait 或 finished
	currentStage := model.TaskStage(temp.Stage)
	hint := c.store.resolveHint(ctx, temp, currentStage)
	nextStage := currentStage.NextStage(hint)

	// 到达终态 → 标记任务完成
	if nextStage.IsTerminalStage() {
		c.store.ClearHint(temp.TaskID)
		return c.toTerminal(ctx, task, currentStage, nextStage, model.TaskStatusCompleted)
	}

	// 流转到下一阶段的 wait 状态，等待对应调度器接管
	return c.updateStage(ctx, task, currentStage, nextStage,
		model.TaskStatusQueued, framework.TASK_STATUS_WAITING)
}

// ToFailedStatus 将任务标记为失败终态。
//
// 任何中间阶段失败 → 任务统一变为 TaskStatusFailed，
// stage 保留在失败时的阶段（便于排查），不再流转。
func (c *StageTaskContainer) ToFailedStatus(ctx context.Context, task *framework.Task, reason error) (*framework.Task, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return task, fmt.Errorf("[%s] task item is not *model.TaskTemp", c.TaskName)
	}

	db := dao.DB().WithContext(ctx)
	now := time.Now()

	reasonJSON, _ := json.Marshal(&model.FailedReason{
		Code:    500,
		Message: reason.Error(),
	})

	// 失败终态：status = failed，stage 保留在当前失败的阶段
	updates := map[string]interface{}{
		"status":        model.TaskStatusFailed,
		"failed_reason": string(reasonJSON),
		"attempt_times": task.TaskAttemptsTime,
		"end_time":      now,
		"updated_time":  now,
	}

	if err := db.Model(&model.TaskTemp{}).Where("task_id = ?", temp.TaskID).Updates(updates).Error; err != nil {
		return task, fmt.Errorf("[%s] update to failed: %w", c.TaskName, err)
	}

	// 同步终态表
	if err := db.Model(&model.Task{}).Where("task_id = ?", temp.TaskID).Updates(updates).Error; err != nil {
		log.Printf("[%s] sync failed status to task table error: %v", c.TaskName, err)
	}

	// 终态落库后清理 task_temp，与 dao.UpdateTaskStatus 保持一致。
	c.purgeTaskTemp(ctx, temp.TaskID)

	// 补写执行过程。
	// 失败路径不会走 actuator 的 persistAgentOutput，若不在此补写，
	// 用户只能看到一句失败原因，无法得知中间执行到哪一步、调了哪些工具。
	c.persistFailedTaskOutput(ctx, temp, reason)

	c.store.ClearHint(temp.TaskID)
	log.Printf("[%s] task %s FAILED at stage %s: %v",
		c.TaskName, temp.TaskID, model.TaskStage(temp.Stage), reason)

	stream.Publish(stream.Event{
		Type:   stream.EventTypeTaskDone,
		TaskID: temp.TaskID,
		Data:   map[string]interface{}{"status": int(model.TaskStatusFailed)},
	})

	task.TaskStatus = framework.TASK_STATUS_FAILED
	return task, nil
}

// persistFailedTaskOutput 任务失败时补写 task_output，保留已发生的执行轨迹。
//
// 只在原本没有输出时写入 —— 若 actuator 已写过（例如工具都跑完了、
// 只是最后落库失败），不覆盖它的内容。
// 任何异常只记日志：失败状态本身已经落库，过程信息属于增强项。
func (c *StageTaskContainer) persistFailedTaskOutput(
	ctx context.Context, temp *model.TaskTemp, reason error,
) {
	// 只有对话任务的 task_output 契约是 AgentOutputData。
	// 工作流任务存的是 DAG，往里写 AgentOutputData 会让前端解析不出节点，
	// 整个工作流面板直接空掉。
	if temp.TaskType != model.TaskTypeConversation {
		return
	}

	turnDAO := &dao.TaskTurnDAO{}
	turns, err := turnDAO.ListTurnsByTaskID(ctx, temp.TaskID)
	if err != nil {
		log.Printf("[%s] task %s list turns for failed output error: %v",
			c.TaskName, temp.TaskID, err)
		return
	}
	if len(turns) == 0 {
		return
	}

	out := model.AgentOutputData{}
	if !out.FillThinkProcess(turns) {
		return
	}
	for _, t := range turns {
		if t.TurnType == model.TurnTypeToolCall {
			out.ToolCalls = append(out.ToolCalls, buildToolCallRecordForFailed(t))
		}
	}

	outJSON, err := json.Marshal(&out)
	if err != nil {
		log.Printf("[%s] task %s marshal failed output error: %v", c.TaskName, temp.TaskID, err)
		return
	}

	// 仅当 task_output 为空时写入，避免覆盖 actuator 已有的输出
	db := dao.DB().WithContext(ctx)
	upd := map[string]interface{}{"task_output": string(outJSON)}
	if err := db.Model(&model.Task{}).
		Where("task_id = ? AND (task_output IS NULL OR task_output = '')", temp.TaskID).
		Updates(upd).Error; err != nil {
		log.Printf("[%s] task %s write failed output error: %v", c.TaskName, temp.TaskID, err)
	}
}

// buildToolCallRecordForFailed 把 tool_call turn 转成对外记录。
// 与 actuator 侧同名逻辑保持一致；因分属不同包各留一份，避免为此引入循环依赖。
func buildToolCallRecordForFailed(t *model.TaskTurn) model.ToolCallRecord {
	rec := model.ToolCallRecord{
		ToolName:   t.ToolName,
		ServerName: t.ServerName,
		Status:     int(t.Status),
		TimeCost:   fmt.Sprintf("%dms", t.TimeCostMs),
	}
	if t.Input != "" {
		var in model.TurnToolInput
		if json.Unmarshal([]byte(t.Input), &in) == nil {
			if argBytes, mErr := json.Marshal(in.Arguments); mErr == nil {
				rec.Input = string(argBytes)
			}
			if rec.ToolName == "" {
				rec.ToolName = in.ToolName
			}
			rec.Summary = strings.TrimSpace(in.Summary)
		}
	}
	if t.Output != "" {
		var o model.TurnToolOutput
		if json.Unmarshal([]byte(t.Output), &o) == nil {
			rec.Output = model.TruncateToolCallOutput(o.Content)
		}
	}
	if t.ErrorMsg != "" && rec.Output == "" {
		rec.Output = t.ErrorMsg
	}
	return rec
}

// ToStopStatus 停止任务：回退到当前阶段的 wait 状态
func (c *StageTaskContainer) ToStopStatus(ctx context.Context, task *framework.Task) (*framework.Task, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return task, fmt.Errorf("[%s] task item is not *model.TaskTemp", c.TaskName)
	}

	currentStage := model.TaskStage(temp.Stage)
	// doing → 回退到对应的 wait
	waitStage := currentStage.ToWaitStage()

	return c.updateStage(ctx, task, currentStage, waitStage,
		model.TaskStatusQueued, framework.TASK_STATUS_WAITING)
}

// ToDeleteStatus 接口要求，暂不支持
func (c *StageTaskContainer) ToDeleteStatus(ctx context.Context, task *framework.Task) (*framework.Task, error) {
	return task, nil
}

// ToExportStatus 接口要求，本系统无导出阶段
func (c *StageTaskContainer) ToExportStatus(ctx context.Context, task *framework.Task) (*framework.Task, error) {
	return task, nil
}

// UpdateRunningTaskStatus 更新运行中任务的进度
func (c *StageTaskContainer) UpdateRunningTaskStatus(ctx context.Context, task *framework.Task, status framework.AsyncTaskStatus) error {
	return nil
}

// ============================================================
// 内部方法
// ============================================================

// toFrameworkTask 将 model.TaskTemp 转换为 framework.Task
func (c *StageTaskContainer) toFrameworkTask(temp *model.TaskTemp, status framework.TaskStatus) framework.Task {
	task := framework.Task{
		TaskId:           temp.TaskID,
		TaskItem:         temp,
		TaskStatus:       status,
		TaskAttemptsTime: temp.AttemptTimes,
	}
	// 看门狗计时基准（framework 用它判断「单次进入 doing 阶段」是否超时）：
	// 优先取 stage_start_time（每次进入 RUNNING 重设），老数据无该列时回落到
	// start_time，再回落 now。
	switch {
	case temp.StageStartTime != nil:
		task.TaskStartTime = *temp.StageStartTime
	case temp.StartTime != nil:
		task.TaskStartTime = *temp.StartTime
	default:
		task.TaskStartTime = time.Now()
	}
	return task
}

// updateStage 更新任务的 stage 和 status（非终态）
func (c *StageTaskContainer) updateStage(
	ctx context.Context, task *framework.Task,
	fromStage, toStage model.TaskStage,
	status model.TaskStatus, fwStatus framework.TaskStatus,
) (*framework.Task, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return task, fmt.Errorf("[%s] task item is not *model.TaskTemp", c.TaskName)
	}

	db := dao.DB().WithContext(ctx)
	now := time.Now()

	updates := map[string]interface{}{
		"stage":         int(toStage),
		"status":        status,
		"attempt_times": task.TaskAttemptsTime,
		"updated_time":  now,
	}

	if fwStatus == framework.TASK_STATUS_RUNNING {
		// 看门狗计时：每次进入 doing 阶段都重设，用于「单次进入 doing 阶段」超时。
		updates["stage_start_time"] = now
		// 任务开始时间：只在首次进入执行时记录，之后保持不变。
		// 供收口扫描产物（mtime > start_time）等需要「任务真正开始时刻」的场景使用。
		if temp.StartTime == nil {
			updates["start_time"] = now
		}
	}

	// 带 stage 前置条件的乐观锁更新。
	//
	// 不加条件的话，两个阶段调度器可能对同一任务并发写 stage，
	// 后写的覆盖先写的，任务会被"拉回"上一阶段或跳过阶段。
	// 加上 stage = fromStage 后，只有观察到预期起点的那个调度器能写成功。
	res := db.Model(&model.TaskTemp{}).
		Where("task_id = ? AND stage = ?", temp.TaskID, int(fromStage)).
		Updates(updates)
	if res.Error != nil {
		return task, fmt.Errorf("[%s] update stage %s → %s: %w", c.TaskName, fromStage, toStage, res.Error)
	}
	if res.RowsAffected == 0 {
		// 任务已被其他调度器推进（或已进入终态），本次流转作废。
		//
		// 不返回 error —— 框架在 ToRunningStatus 失败时会直接把任务判失败
		// （task_scheduler.go:315），而这个任务实际正常运行中，只是被别人先推进了。
		// 静默跳过，让真正持有它的调度器继续推进。
		log.Printf("[%s] task %s stale transition %s → %s (already advanced by another scheduler), skip",
			c.TaskName, temp.TaskID, fromStage, toStage)
		return task, nil
	}

	// 同步 stage/status 到终态表，保持前端可查
	if err := db.Model(&model.Task{}).Where("task_id = ?", temp.TaskID).Updates(updates).Error; err != nil {
		log.Printf("[%s] sync stage to task table error: %v", c.TaskName, err)
	}

	log.Printf("[%s] task %s: %s → %s", c.TaskName, temp.TaskID, fromStage, toStage)

	// 更新内存中的 task 对象，保持一致
	temp.Stage = int(toStage)
	temp.Status = status
	task.TaskStatus = fwStatus
	if fwStatus == framework.TASK_STATUS_RUNNING {
		task.TaskStartTime = now
		temp.StageStartTime = &now
		if temp.StartTime == nil {
			temp.StartTime = &now
		}
	}
	return task, nil
}

// toTerminal 将任务标记为成功终态
func (c *StageTaskContainer) toTerminal(
	ctx context.Context, task *framework.Task,
	fromStage, toStage model.TaskStage, status model.TaskStatus,
) (*framework.Task, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return task, fmt.Errorf("[%s] task item is not *model.TaskTemp", c.TaskName)
	}

	db := dao.DB().WithContext(ctx)
	now := time.Now()

	updates := map[string]interface{}{
		"stage":         int(toStage),
		"status":        status,
		"attempt_times": task.TaskAttemptsTime,
		"progress":      100.0,
		"end_time":      now,
		"updated_time":  now,
	}

	if err := db.Model(&model.TaskTemp{}).Where("task_id = ?", temp.TaskID).Updates(updates).Error; err != nil {
		return task, fmt.Errorf("[%s] update to terminal: %w", c.TaskName, err)
	}

	if err := db.Model(&model.Task{}).Where("task_id = ?", temp.TaskID).Updates(updates).Error; err != nil {
		log.Printf("[%s] sync terminal status to task table error: %v", c.TaskName, err)
	}

	// 终态落库后清理 task_temp，与 dao.UpdateTaskStatus 保持一致。
	c.purgeTaskTemp(ctx, temp.TaskID)

	log.Printf("[%s] task %s FINISHED (%s → %s)", c.TaskName, temp.TaskID, fromStage, toStage)

	stream.Publish(stream.Event{
		Type:   stream.EventTypeTaskDone,
		TaskID: temp.TaskID,
		Data:   map[string]interface{}{"status": int(status)},
	})

	temp.Stage = int(toStage)
	temp.Status = status
	task.TaskStatus = framework.TASK_STATUS_SUCCESS
	return task, nil
}

// purgeTaskTemp 终态落库后物理删除 task_temp 行。
//
// 终态任务不再参与调度，task_temp 里的行没有保留价值；若不清理，
// task_temp 会随每次任务收尾无限增长。与 dao.UpdateTaskStatus 的终态处理
// （Unscoped 删除 task_temp）保持一致，也让 ListSubTasksByParent 等依赖
// 「终态子任务只留 task 表」的双表合并逻辑真正成立。
//
// TaskTemp 嵌入了带软删除（DeletedAt）的 Task，必须用 Unscoped 物理删除，
// 否则软删除行仍占据磁盘，只是查询被 gorm 自动过滤。
func (c *StageTaskContainer) purgeTaskTemp(ctx context.Context, taskID string) {
	db := dao.DB().WithContext(ctx)
	if err := db.Unscoped().Model(&model.TaskTemp{}).
		Where("task_id = ?", taskID).Delete(&model.TaskTemp{}).Error; err != nil {
		log.Printf("[%s] purge task_temp for terminal task %s error: %v", c.TaskName, taskID, err)
	}
}

// Ensure interface compliance
var _ framework.TaskContainer = (*StageTaskContainer)(nil)
