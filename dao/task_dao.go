package dao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrWorkflowVersionConflict 工作流 DAG 乐观锁版本冲突：
// 写 DAG 时发现 dag_version 已被并发修改，本次「读-改-写」作废。
// 调用方应重读最新 DAG 重新合并（追加节点）或放弃本轮回写交由下轮重建（执行回写）。
var ErrWorkflowVersionConflict = errors.New("workflow dag version conflict")

// TaskDAO 任务数据访问对象
type TaskDAO struct {
	Tx *gorm.DB
}

// CreateTask 创建调度任务 —— 双写 task + task_temp
// 必须在事务中调用
func (d *TaskDAO) CreateTask(ctx context.Context, task *model.Task) error {
	db := d.getDB().WithContext(ctx)

	task.CreatedAt = time.Now()
	task.UpdatedAt = time.Now()

	// 1. 写终态表
	if err := db.Create(task).Error; err != nil {
		return fmt.Errorf("create task failed: %w", err)
	}

	// 2. 双写临时态表
	taskTemp := &model.TaskTemp{Task: *task}
	if err := db.Create(taskTemp).Error; err != nil {
		return fmt.Errorf("create task_temp failed: %w", err)
	}

	return nil
}

// UpdateTaskStatus 更新任务状态 —— 事务中同步更新终态表和临时态表
// 终态状态（COMPLETED/FAILED/STOPPED）会删除临时态记录
func (d *TaskDAO) UpdateTaskStatus(
	ctx context.Context, taskID string, newStatus model.TaskStatus,
	failedReason *model.FailedReason, progress float64,
) error {
	db := d.getDB().WithContext(ctx)
	now := time.Now()

	updates := map[string]interface{}{
		"status":       newStatus,
		"updated_time": now,
		"progress":     progress,
	}

	// 状态变更时间点：进入 Running 时记录「单次进入执行」时间（看门狗计时）。
	// start_time 用 COALESCE 保持首次非空值不变（任务真正开始时刻，产物扫描基准）。
	if newStatus == model.TaskStatusRunning {
		updates["stage_start_time"] = now
		updates["start_time"] = gorm.Expr("COALESCE(start_time, ?)", now)
	}
	if isTerminalStatus(newStatus) {
		updates["end_time"] = now
	}
	if failedReason != nil {
		reasonJSON, _ := json.Marshal(failedReason)
		updates["failed_reason"] = string(reasonJSON)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		// 终态 → 删除临时表、更新终态表
		if isTerminalStatus(newStatus) {
			if err := tx.Unscoped().Model(&model.TaskTemp{}).
				Where("task_id = ?", taskID).Delete(&model.TaskTemp{}).Error; err != nil {
				return fmt.Errorf("delete task_temp failed: %w", err)
			}
		} else {
			// 非终态 → 更新临时表
			if err := tx.Model(&model.TaskTemp{}).Where("task_id = ?", taskID).
				Updates(updates).Error; err != nil {
				return fmt.Errorf("update task_temp failed: %w", err)
			}
		}
		// 始终更新终态表
		if err := tx.Model(&model.Task{}).Where("task_id = ?", taskID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update task failed: %w", err)
		}
		return nil
	})
}

// UpdateTaskOutput 更新任务输出结果
func (d *TaskDAO) UpdateTaskOutput(ctx context.Context, taskID string, output string) error {
	db := d.getDB().WithContext(ctx)
	return db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"task_output":  output,
			"updated_time": time.Now(),
		}
		if err := tx.Model(&model.Task{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update task output failed: %w", err)
		}
		if err := tx.Model(&model.TaskTemp{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update task_temp output failed: %w", err)
		}
		return nil
	})
}

// UpdateTaskPlan 更新任务规划结果 —— 同步写 task + task_temp 两表
func (d *TaskDAO) UpdateTaskPlan(ctx context.Context, taskID string, plan string) error {
	db := d.getDB().WithContext(ctx)
	return db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"plan":         plan,
			"updated_time": time.Now(),
		}
		if err := tx.Model(&model.Task{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update task plan failed: %w", err)
		}
		if err := tx.Model(&model.TaskTemp{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update task_temp plan failed: %w", err)
		}
		return nil
	})
}

// UpdateTaskPhase 切换任务的语义阶段（规划期 → 执行期）—— 同步写 task + task_temp 两表。
//
// 必须双写：task_temp 是调度器的工作副本（执行器拿到的 TaskTemp 由它加载），
// 只更新终态表会让下一轮调度仍按规划期处理，任务卡在规划循环里出不来。
func (d *TaskDAO) UpdateTaskPhase(ctx context.Context, taskID string, phase int) error {
	db := d.getDB().WithContext(ctx)
	return db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{
			"phase":        phase,
			"updated_time": time.Now(),
		}
		if err := tx.Model(&model.Task{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update task phase failed: %w", err)
		}
		if err := tx.Model(&model.TaskTemp{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update task_temp phase failed: %w", err)
		}
		return nil
	})
}

// ============================================================
// 工作流任务专用
// ============================================================

// UpsertWorkflowDAGParams 追加节点后回写工作流任务的参数
type UpsertWorkflowDAGParams struct {
	// TaskID 工作流任务 ID
	TaskID string
	// Output 合并后的完整 DAG（WorkflowOutputData 序列化结果）
	Output string
	// Status 追加节点后工作流应处于的状态
	Status model.TaskStatus
	// Stage 追加节点后工作流应处于的调度阶段
	Stage model.TaskStage
	// ExpectVersion 乐观锁期望的 dag_version。读-改期间若有并发写入使版本
	// 前进，本次更新返回 ErrWorkflowVersionConflict，调用方重读重试。
	ExpectVersion int64
}

// UpdateWorkflowDAG 回写工作流任务的 DAG 快照与调度状态。
//
// 工作流任务被追加新节点时调用。除了更新两张表的字段，还要处理一个
// 容易漏掉的情况：工作流上一轮已跑完（终态），此时 task_temp 里的行
// 已被 UpdateTaskStatus 删除，只 Update 是 0 行受影响 —— 调度器再也
// 扫不到它，新追加的节点永远不会被执行。因此非终态时用 upsert 语义，
// task_temp 缺行则按最新的 task 记录补建。
func (d *TaskDAO) UpdateWorkflowDAG(ctx context.Context, p UpsertWorkflowDAGParams) error {
	db := d.getDB().WithContext(ctx)
	now := time.Now()

	updates := map[string]interface{}{
		"task_output":  p.Output,
		"status":       p.Status,
		"stage":        int(p.Stage),
		"updated_time": now,
		"dag_version":  gorm.Expr("dag_version + 1"),
	}
	// 重新排队意味着这一轮还没结束，清掉上一轮的结束时间，
	// 否则前端算出来的耗时会是负数或停在旧值。
	if !isTerminalStatus(p.Status) {
		updates["end_time"] = nil
	}

	return db.Transaction(func(tx *gorm.DB) error {
		// 乐观锁：只在 dag_version 仍等于读到的期望值时写。
		// 权威冲突判断只看 task 表 —— task_temp 可能因上一轮终态被清理而缺行，
		// 它的 0 行不构成冲突，走下面的补建逻辑。
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND dag_version = ?", p.TaskID, p.ExpectVersion).
			Updates(updates)
		if res.Error != nil {
			return fmt.Errorf("update workflow task failed: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrWorkflowVersionConflict
		}

		tempRes := tx.Model(&model.TaskTemp{}).
			Where("task_id = ? AND dag_version = ?", p.TaskID, p.ExpectVersion).
			Updates(updates)
		if tempRes.Error != nil {
			return fmt.Errorf("update workflow task_temp failed: %w", tempRes.Error)
		}
		if tempRes.RowsAffected > 0 || isTerminalStatus(p.Status) {
			return nil
		}

		// task_temp 无此行（上一轮已收尾被清理）→ 按终态表最新内容补建，
		// 让调度器重新看得到这个工作流。补建内容含刚 +1 后的 dag_version。
		var task model.Task
		if err := tx.Where("task_id = ?", p.TaskID).First(&task).Error; err != nil {
			return fmt.Errorf("reload workflow task for temp revive failed: %w", err)
		}
		if err := tx.Create(&model.TaskTemp{Task: task}).Error; err != nil {
			return fmt.Errorf("revive workflow task_temp failed: %w", err)
		}
		return nil
	})
}

// UpdateWorkflowRun 回写工作流任务的 DAG 快照与执行进度。
//
// 工作流编排执行期间（status=running，task_temp 行仍存在）由 workflow 执行器
// 周期调用：把节点状态/输出回写 task_output，供前端画布实时展示，并推进 progress。
// 与 UpdateWorkflowDAG（追加节点、可能补建 temp 行）不同，本方法假设两表行都在。
//
// expectVersion 是乐观锁期望的 dag_version：读 DAG 快照时记录的版本，
// 写回时若已被并发写入推进，则返回 ErrWorkflowVersionConflict，调用方放弃本轮回写。
func (d *TaskDAO) UpdateWorkflowRun(ctx context.Context, taskID string, output string, progress float64, expectVersion int64) error {
	db := d.getDB().WithContext(ctx)
	updates := map[string]interface{}{
		"task_output":  output,
		"progress":     progress,
		"updated_time": time.Now(),
		"dag_version":  gorm.Expr("dag_version + 1"),
	}
	return db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND dag_version = ?", taskID, expectVersion).
			Updates(updates)
		if res.Error != nil {
			return fmt.Errorf("update workflow task run: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrWorkflowVersionConflict
		}
		if err := tx.Model(&model.TaskTemp{}).
			Where("task_id = ? AND dag_version = ?", taskID, expectVersion).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update workflow task_temp run: %w", err)
		}
		return nil
	})
}

// RetryWorkflowParams 重试工作流任务的参数
type RetryWorkflowParams struct {
	// TaskID 工作流任务 ID
	TaskID string
	// Output 重置节点后的完整 DAG（WorkflowOutputData 序列化结果）
	Output string
	// Progress 重试后按已完成节点重算的进度（0.0~100.0）
	Progress float64
	// ExpectVersion 乐观锁期望的 dag_version。读 DAG 快照时记录的版本，
	// 写回时若已被并发写入推进，则返回 ErrWorkflowVersionConflict，调用方重读重试。
	ExpectVersion int64
}

// RetryWorkflow 把终态（失败/终止）的工作流任务重新排队，触发重新执行。
//
// 与 UpdateWorkflowDAG（追加节点）不同，本方法额外：
//   - 清空 failed_reason、start_time、end_time，让前端显示干净的「排队中」状态；
//   - 重置 progress（按保留的已完成节点重算，由调用方传入）；
//   - 恢复 task_temp 行 —— 工作流失败时 task_temp 已被 UpdateTaskStatus 清理，
//     必须补建，否则编排调度器扫不到它、重试永远不会被驱动。
//
// 乐观锁：WHERE dag_version = expectVersion，冲突返回 ErrWorkflowVersionConflict。
func (d *TaskDAO) RetryWorkflow(ctx context.Context, p RetryWorkflowParams) error {
	db := d.getDB().WithContext(ctx)
	now := time.Now()

	updates := map[string]interface{}{
		"task_output":   p.Output,
		"status":        model.TaskStatusQueued,
		"stage":         int(model.StageWorkflowWait),
		"progress":      p.Progress,
		"failed_reason": nil,
		"start_time":    nil,
		"end_time":      nil,
		"updated_time":  now,
		"dag_version":   gorm.Expr("dag_version + 1"),
	}

	return db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND dag_version = ?", p.TaskID, p.ExpectVersion).
			Updates(updates)
		if res.Error != nil {
			return fmt.Errorf("retry workflow task failed: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrWorkflowVersionConflict
		}

		// task_temp 行在终态时已被清理，先尝试 update（兜底：万一还在），
		// 0 行则按终态表最新内容补建，让调度器重新看得到这个工作流。
		tempRes := tx.Model(&model.TaskTemp{}).
			Where("task_id = ?", p.TaskID).Updates(updates)
		if tempRes.Error != nil {
			return fmt.Errorf("retry workflow task_temp failed: %w", tempRes.Error)
		}
		if tempRes.RowsAffected > 0 {
			return nil
		}

		var task model.Task
		if err := tx.Where("task_id = ?", p.TaskID).First(&task).Error; err != nil {
			return fmt.Errorf("reload workflow task for retry revive failed: %w", err)
		}
		if err := tx.Create(&model.TaskTemp{Task: task}).Error; err != nil {
			return fmt.Errorf("revive workflow task_temp for retry failed: %w", err)
		}
		return nil
	})
}

// RetryConversationTask 把终态（失败/终止）的对话任务重新排队，从失败发生的
// stage 继续执行（断点续跑），而非从头重跑。
//
// 与 RetryWorkflow（保留已完成节点、只重置失败节点）在语义上对齐：对话任务虽
// 没有节点概念，但失败时 ActionFailed 会把 stage 保留在失败发生的阶段（doing），
// 据此可还原「接着哪一步继续」（由调用方传入 resumeStage）：
//   - tool_call_doing（工具执行阶段整体失败）→ tool_call_wait：重新读取上一轮
//     LLM turn 里的 tool_calls 重新执行，此前的推理与工具结果都保留；
//   - functional_call_doing（LLM 调用失败）→ functional_call_wait：保留历史，
//     让 LLM 基于已有上下文重新做这一轮推理（失败的 LLM turn 不会进入历史）。
//
// 因此这里不再清空 task_output / plan / phase —— 它们都是续跑所需的上下文：
//   - plan 是规划期的产物，执行期重试时仍需注入（renderPlanContext）；
//   - phase 决定重试后是继续规划还是继续执行，回退到规划期会让执行期失败的任务
//     重头再规划一遍。
//
// 只重置：status=queued、stage=resumeStage、progress=0、failed_reason=nil、
// attempt_times=0、start_time/end_time=nil，前端显示干净的「排队中」。
// 旧 turn 也不再清空（见 handler 的 retryConversationTask），历史重建会过滤
// 掉非 success 的失败 turn，因此续跑不会把失败的那一轮污染进上下文。
func (d *TaskDAO) RetryConversationTask(ctx context.Context, taskID string, resumeStage model.TaskStage) error {
	db := d.getDB().WithContext(ctx)
	now := time.Now()

	updates := map[string]interface{}{
		"status":        model.TaskStatusQueued,
		"stage":         int(resumeStage),
		"progress":      0,
		"failed_reason": nil,
		"attempt_times": 0,
		"start_time":    nil,
		"end_time":      nil,
		"updated_time":  now,
	}

	return db.Transaction(func(tx *gorm.DB) error {
		// 权威更新在 task 表
		if err := tx.Model(&model.Task{}).
			Where("task_id = ?", taskID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("retry conversation task failed: %w", err)
		}

		// task_temp 行在终态时已被清理，先尝试 update（兜底：万一还在），
		// 0 行则按终态表最新内容补建，让调度器重新看得到这个任务。
		tempRes := tx.Model(&model.TaskTemp{}).
			Where("task_id = ?", taskID).Updates(updates)
		if tempRes.Error != nil {
			return fmt.Errorf("retry conversation task_temp failed: %w", tempRes.Error)
		}
		if tempRes.RowsAffected > 0 {
			return nil
		}

		var task model.Task
		if err := tx.Where("task_id = ?", taskID).First(&task).Error; err != nil {
			return fmt.Errorf("reload conversation task for retry revive failed: %w", err)
		}
		if err := tx.Create(&model.TaskTemp{Task: task}).Error; err != nil {
			return fmt.Errorf("revive conversation task_temp for retry failed: %w", err)
		}
		return nil
	})
}

// GetTaskByID 根据任务 ID 获取任务（优先查临时表，再由终态兜底）
func (d *TaskDAO) GetTaskByID(ctx context.Context, taskID string) (*model.Task, error) {
	db := d.getDB().WithContext(ctx)

	// 优先查临时态
	var temp model.TaskTemp
	if err := db.Where("task_id = ?", taskID).First(&temp).Error; err == nil {
		return &temp.Task, nil
	}
	// 查终态兜底
	var task model.Task
	if err := db.Where("task_id = ?", taskID).First(&task).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get task failed: %w", err)
	}
	return &task, nil
}

// GetTaskStatusByID 查询任务但排除 longtext 大字段（task_output / plan /
// task_input），供详情接口先判断「是否终态、能否命中缓存」。
//
// 详情接口对终态任务做缓存，但命中缓存前不能先读完整 task_output ——
// 那正是要省掉的 MB 级读取。用本方法先拿 status 判断，命中缓存则直接返回，
// 完全不碰 longtext。
func (d *TaskDAO) GetTaskStatusByID(ctx context.Context, taskID string) (*model.Task, error) {
	db := d.getDB().WithContext(ctx)

	var temp model.TaskTemp
	if err := db.Where("task_id = ?", taskID).First(&temp).Error; err == nil {
		return &temp.Task, nil
	}
	var task model.Task
	if err := db.Omit("TaskOutput", "Plan", "TaskInput").
		Where("task_id = ?", taskID).First(&task).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get task status failed: %w", err)
	}
	return &task, nil
}

// ListTasksByChatID 根据对话 ID 查询对话任务列表（双表查询：临时态 + 终态合并）。
//
// 只返回 TaskTypeConversation。工作流任务虽然也挂在同一个 chat_id 下，
// 但它不是一条「消息」—— 它是整段对话共享的 DAG，由 chat_info.workflow
// 单独返回。混进来会让前端多渲染一条空的用户提问气泡，
// 也会污染 agent 历史重建（它没有真实的 user 输入）。
func (d *TaskDAO) ListTasksByChatID(ctx context.Context, chatID string) ([]*model.Task, error) {
	db := d.getDB().WithContext(ctx)

	// 查临时态表（运行中/排队中的任务）
	var tempTasks []model.TaskTemp
	if err := db.Where("chat_id = ? AND task_type = ?", chatID, model.TaskTypeConversation).
		Order("created_time ASC").Find(&tempTasks).Error; err != nil {
		return nil, fmt.Errorf("list temp tasks by chat_id failed: %w", err)
	}

	// 查终态表
	var finalTasks []*model.Task
	if err := db.Where("chat_id = ? AND task_type = ?", chatID, model.TaskTypeConversation).
		Order("created_time ASC").Find(&finalTasks).Error; err != nil {
		return nil, fmt.Errorf("list tasks by chat_id failed: %w", err)
	}

	// 合并：临时态优先（避免终态未同步时的重复），再拼上终态中不在临时态的
	seen := make(map[string]bool)
	result := make([]*model.Task, 0, len(tempTasks)+len(finalTasks))
	for i := range tempTasks {
		t := tempTasks[i]
		seen[t.TaskID] = true
		result = append(result, &t.Task)
	}
	for _, t := range finalTasks {
		if !seen[t.TaskID] {
			result = append(result, t)
		}
	}

	// 统一按创建时间升序排序。若不排序，「临时态优先」会让运行中/排队中的
	// 任务（仍在 task_temp 表）被顶到列表最前，任务跑完进入终态表后才回到
	// 时间序 —— 表现为「执行中的消息跳最上、完成后归位」。消息应始终按
	// 发送先后排列，与任务是否处于运行态无关。SliceStable 保证创建时间相同时
	// 保持临时态优先的稳定序（去重语义不受影响）。
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

// ListUnfinishedPredecessors 列出对话里「早于 before 创建、且尚未收尾」的对话任务。
//
// 上下文压缩要用它判断能不能动手：压缩会把历史 task 的详情换成一份摘要，
// 并把这些 task 标记为「已覆盖」—— 覆盖之后它们后续产生的 turn 一律不再
// 回放。若某个更早的 task 还在跑，此时压缩等于把它后半段交互永久丢掉
// （用户连发多个问题时正是这种局面：后面的问题在跑，前面的还没跑完）。
//
// 只认对话任务（task_type=conversation）：
//   - 工作流任务（task_type=workflow）跑满一个 DAG 可能几十分钟，等它收尾
//     才压缩不现实；而且工作流的产出在历史里本就以「工作流摘要段」呈现，
//     不参与详情压缩，等它没有意义。
//   - 子任务（task_type=workflow_subtask）是执行细节，它们的 turn 也走
//     父任务的摘要段，同样不构成等待理由。
//
// 只查终态表不够：运行中的任务在 task_temp，终态表里那行是创建时双写的
// 快照，status 未必同步。因此两表合并、临时态优先（与 ListTasksByChatID 一致）。
func (d *TaskDAO) ListUnfinishedPredecessors(
	ctx context.Context, chatID string, before time.Time,
) ([]*model.Task, error) {
	db := d.getDB().WithContext(ctx)
	active := []model.TaskStatus{model.TaskStatusQueued, model.TaskStatusRunning}

	var temps []model.TaskTemp
	if err := db.Where("chat_id = ? AND task_type = ? AND status IN ? AND created_time < ?",
		chatID, model.TaskTypeConversation, active, before).
		Order("created_time ASC").Find(&temps).Error; err != nil {
		return nil, fmt.Errorf("list unfinished predecessors (temp): %w", err)
	}

	seen := make(map[string]bool, len(temps))
	result := make([]*model.Task, 0, len(temps))
	for i := range temps {
		seen[temps[i].TaskID] = true
		result = append(result, &temps[i].Task)
	}

	var finals []*model.Task
	if err := db.Where("chat_id = ? AND task_type = ? AND status IN ? AND created_time < ?",
		chatID, model.TaskTypeConversation, active, before).
		Order("created_time ASC").Find(&finals).Error; err != nil {
		return nil, fmt.Errorf("list unfinished predecessors: %w", err)
	}
	for _, t := range finals {
		if !seen[t.TaskID] {
			result = append(result, t)
		}
	}
	return result, nil
}

// SetTaskDelay 写入任务的延后调度信息（delay_until / defer_count），双表同步。
//
// 两表都要写：调度扫描读 task_temp，而 task 表是对外可见的权威快照，
// 不同步会让「前端/调试看到的状态」与调度行为不一致。
func (d *TaskDAO) SetTaskDelay(
	ctx context.Context, taskID string, until time.Time, deferCount int,
) error {
	db := d.getDB().WithContext(ctx)
	updates := map[string]interface{}{
		"delay_until":  until,
		"defer_count":  deferCount,
		"updated_time": time.Now(),
	}
	if err := db.Model(&model.TaskTemp{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
		return fmt.Errorf("set task delay (temp): %w", err)
	}
	if err := db.Model(&model.Task{}).Where("task_id = ?", taskID).Updates(updates).Error; err != nil {
		return fmt.Errorf("set task delay: %w", err)
	}
	return nil
}

// ListWorkflowTasksByChatID 查询一个对话下的全部工作流任务（task_type=2）。
//
// 一个对话可以有多个工作流（v1.4 起）：每条是一段相对独立的 DAG，
// 由各轮规划决定新开还是续接。全集直接以 task 表为准，不额外建关联表 ——
// chat_id + task_type 已经能唯一确定，再建一张表只会多一处需要同步的状态。
//
// 只查终态表：工作流任务即使在运行中也不会只存在于 task_temp
// （双写保证两表都有），而终态表里的时间/状态才是权威快照。
// 按创建时间升序返回，前端切换器据此给出稳定的「工作流 1 / 2 / 3」顺序。
func (d *TaskDAO) ListWorkflowTasksByChatID(ctx context.Context, chatID string) ([]*model.Task, error) {
	db := d.getDB().WithContext(ctx)
	var tasks []*model.Task
	if err := db.Where("chat_id = ? AND task_type = ?", chatID, model.TaskTypeWorkflow).
		Order("created_time ASC").Find(&tasks).Error; err != nil {
		return nil, fmt.Errorf("list workflow tasks by chat_id failed: %w", err)
	}
	return tasks, nil
}

// SyncWorkflowSourceTaskStatuses 把「来源对话任务」的状态绑定到工作流节点上。
//
// 每轮规划判定 workflow 后，对话任务当轮收口（stage=finished、status=completed），
// 但真正干活的是它规划出的节点（WorkflowNodeData.SourceTaskID 指回该任务）。
// 本方法按 model.DeriveBoundTaskStatus 的规则，把这些对话任务的状态刷新为
// 绑定节点的聚合结果，避免「工作流还在跑，回复却显示已完成」的割裂。
//
// 只改 task 表的 status/progress/end_time，刻意不碰 task_temp 与 stage：
// 对话任务收口时 task_temp 行已被清理，stage 停在 finished —— 若重建 temp 行
// 或改 stage，调度器会把一个已收口的对话任务重新捞起来执行，产生重复回复。
// 状态相同则跳过写入（工作流每秒轮询一次，避免无意义的每秒一写）。
//
// dag 为 nil、或全部节点都没有 SourceTaskID（旧数据）时是 no-op。
// wfStatus 是工作流任务当前（或即将被框架置为）的整体状态。
func (d *TaskDAO) SyncWorkflowSourceTaskStatuses(
	ctx context.Context, dag *model.WorkflowOutputData, wfStatus model.TaskStatus,
) error {
	if dag == nil || len(dag.Nodes) == 0 {
		return nil
	}

	// 按 SourceTaskID 分组收集绑定节点
	grouped := make(map[string][]model.WorkflowNodeData)
	for _, n := range dag.Nodes {
		if n.SourceTaskID == "" {
			continue
		}
		grouped[n.SourceTaskID] = append(grouped[n.SourceTaskID], n)
	}
	if len(grouped) == 0 {
		return nil
	}

	db := d.getDB().WithContext(ctx)
	for sourceTaskID, nodes := range grouped {
		wantStatus, progress := model.DeriveBoundTaskStatus(nodes, wfStatus)

		// 只处理对话任务：SourceTaskID 语义上只指向对话任务，防御性过滤
		// 避免脏数据把工作流/子任务误改出一套自相矛盾的状态。
		var task model.Task
		if err := db.Where("task_id = ? AND task_type = ?", sourceTaskID, model.TaskTypeConversation).
			First(&task).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue // 旧数据 / 已被清理，静默跳过
			}
			return fmt.Errorf("sync source task %s: load failed: %w", sourceTaskID, err)
		}
		if task.Status == wantStatus {
			continue // 状态未变，跳过写入
		}

		updates := map[string]interface{}{
			"status":       wantStatus,
			"progress":     progress,
			"updated_time": time.Now(),
		}
		if wantStatus == model.TaskStatusRunning || wantStatus == model.TaskStatusQueued {
			// 回到推进态：清掉规划收口时的结束时间，让前端耗时继续走
			updates["end_time"] = nil
		} else {
			updates["end_time"] = time.Now()
		}
		if err := db.Model(&model.Task{}).Where("task_id = ?", sourceTaskID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("sync source task %s: update failed: %w", sourceTaskID, err)
		}
		log.Printf("[task-dao] bound source task %s status %d → %d (progress %.0f%%, wf status %d)",
			sourceTaskID, task.Status, wantStatus, progress, wfStatus)
	}
	return nil
}

// ListSubTasksByParent 根据父任务 ID 查询所有子任务（双表合并：临时态优先）。
//
// 工作流编排用：执行器据此判断各节点的子任务终态，并驱动下游节点。
// 子任务运行中/排队中在 task_temp，终态（completed/failed）后 task_temp 行
// 被清理、只剩 task 表，因此必须两表合并，临时态优先。
func (d *TaskDAO) ListSubTasksByParent(ctx context.Context, parentTaskID string) ([]*model.Task, error) {
	db := d.getDB().WithContext(ctx)

	var temps []model.TaskTemp
	if err := db.Where("parent_task_id = ?", parentTaskID).
		Order("created_time ASC").Find(&temps).Error; err != nil {
		return nil, fmt.Errorf("list temp subtasks: %w", err)
	}

	var finals []*model.Task
	if err := db.Where("parent_task_id = ?", parentTaskID).
		Order("created_time ASC").Find(&finals).Error; err != nil {
		return nil, fmt.Errorf("list final subtasks: %w", err)
	}

	seen := make(map[string]bool, len(temps)+len(finals))
	result := make([]*model.Task, 0, len(temps)+len(finals))
	for i := range temps {
		seen[temps[i].TaskID] = true
		result = append(result, &temps[i].Task)
	}
	for _, t := range finals {
		if !seen[t.TaskID] {
			result = append(result, t)
		}
	}
	return result, nil
}

// TaskTimeRange 任务生命周期的时间快照。
//
// 只取时间与状态列：工作流详情要一次性给所有节点算执行时长，把
// task_output / plan 这类 longtext 一起捞上来会白白多读几十 MB。
type TaskTimeRange struct {
	TaskID       string           `gorm:"column:task_id"`
	ParentTaskID string           `gorm:"column:parent_task_id"`
	Status       model.TaskStatus `gorm:"column:status"`
	StartTime    *time.Time       `gorm:"column:start_time"`
	EndTime      *time.Time       `gorm:"column:end_time"`
	CreatedAt    time.Time        `gorm:"column:created_time"`
	UpdatedAt    time.Time        `gorm:"column:updated_time"`
}

// timeRangeCols ListTaskTimeRanges 查询的列，只含时间/状态，避免 longtext。
const timeRangeCols = "task_id, parent_task_id, status, start_time, end_time, created_time, updated_time"

// ListTaskTimeRanges 批量查询若干父任务的直接子任务时间快照（双表合并，临时态优先）。
//
// 一次查一批父任务而不是逐个查：工作流有 N 个节点时，逐个查会退化成 N+1 次
// 查询，而详情接口是 5s 轮询的高频路径。
func (d *TaskDAO) ListTaskTimeRanges(ctx context.Context, parentTaskIDs []string) ([]TaskTimeRange, error) {
	if len(parentTaskIDs) == 0 {
		return nil, nil
	}
	db := d.getDB().WithContext(ctx)

	var temps []TaskTimeRange
	if err := db.Model(&model.TaskTemp{}).Select(timeRangeCols).
		Where("parent_task_id IN ?", parentTaskIDs).Scan(&temps).Error; err != nil {
		return nil, fmt.Errorf("list temp task time ranges: %w", err)
	}

	var finals []TaskTimeRange
	if err := db.Model(&model.Task{}).Select(timeRangeCols).
		Where("parent_task_id IN ?", parentTaskIDs).Scan(&finals).Error; err != nil {
		return nil, fmt.Errorf("list task time ranges: %w", err)
	}

	seen := make(map[string]bool, len(temps))
	result := make([]TaskTimeRange, 0, len(temps)+len(finals))
	for _, t := range temps {
		seen[t.TaskID] = true
		result = append(result, t)
	}
	for _, t := range finals {
		if !seen[t.TaskID] {
			result = append(result, t)
		}
	}
	return result, nil
}

// IsTerminalStatus 任务状态是否已终态（完成/失败/已终止/部分失败）。
//
// 导出给 handler 层判断「未拿到 end_time 时该用 updated_time 还是当前时间」——
// 终态任务没有 end_time 只可能是历史脏数据，不该让前端一直看着耗时往上涨。
func IsTerminalStatus(s model.TaskStatus) bool { return isTerminalStatus(s) }

// ListActiveDescendants 递归收集一个任务下**整棵子树**里仍在排队/运行中的后代任务。
//
// 停止级联必须用这个而不是只扫一层直接子任务：deep_search 的搜索子任务挂在
// 「工作流节点子任务」之下（孙层），只扫一层会在工作流/节点被停止后把它们漏掉，
// 于是搜索子任务继续被调度器捞起跑 LLM（实测：工作流节点 stopped 之后，
// 搜索子任务仍在持续发起 LLM 与 fetch 调用）。
//
// 广度优先展开，maxDescendantDepth 与 maxDescendantNodes 双重兜底，
// 防止脏数据成环或异常膨胀把一次「停止」变成慢查询。
func (d *TaskDAO) ListActiveDescendants(ctx context.Context, rootTaskID string) ([]*model.Task, error) {
	return collectActiveDescendants(
		func(parentID string) ([]*model.Task, error) {
			return d.ListSubTasksByParent(ctx, parentID)
		},
		rootTaskID,
	)
}

// ReconcileOrphanTasks 启动自愈：把「祖先已终态、自己却还停在排队/运行中」的
// 孤儿任务收口成 stopped，返回被收口的任务 ID。
//
// 为什么需要它：停止动作是「自顶向下」的级联，而调度器是「自底向上」地捞任务，
// 两者之间存在竞态 —— 级联跑完之前创建出来的孙层任务（典型是 deep_search 搜索
// 子任务）会漏在网外。进程一重启，调度器只看得到「status=running」这一个事实，
// 于是把它们重新捞起来跑，用户看到的就是「明明停掉了，一开调度器又接着烧 token」。
//
// 与停止级联互补：级联负责「当下」的即时止损，这里负责「历史」的兜底收口。
// 只在进程启动时跑一次，成本是一次轻量扫描（只取三列）。
func (d *TaskDAO) ReconcileOrphanTasks(ctx context.Context) ([]string, error) {
	db := d.getDB().WithContext(ctx)

	// 只取判活需要的三列：全表扫描在任务量级下（千级）开销可忽略，
	// 换来的是不必再按祖先逐层回查。
	var rows []struct {
		TaskID       string `gorm:"column:task_id"`
		ParentTaskID string `gorm:"column:parent_task_id"`
		Status       model.TaskStatus
	}
	if err := db.Model(&model.Task{}).
		Select("task_id, parent_task_id, status").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("reconcile: load task graph: %w", err)
	}

	nodes := make([]taskNode, 0, len(rows))
	for _, r := range rows {
		nodes = append(nodes, taskNode{TaskID: r.TaskID, ParentTaskID: r.ParentTaskID, Status: r.Status})
	}

	orphans := orphanTaskIDs(nodes)
	if len(orphans) == 0 {
		return nil, nil
	}

	reason := &model.FailedReason{
		Code:    0,
		Message: "启动自愈：祖先任务已终止，本任务残留运行态，已自动收口为 stopped",
	}
	// 单个失败不阻断整体：记录后继续收口其余任务。
	for _, id := range orphans {
		if err := d.UpdateTaskStatus(ctx, id, model.TaskStatusStopped, reason, 0); err != nil {
			log.Printf("[reconcile] stop orphan task %s failed: %v", id, err)
		}
	}
	return orphans, nil
}

// taskNode 判活用的最小任务视图（避免把整行 longtext 拉进内存）。
type taskNode struct {
	TaskID       string
	ParentTaskID string
	Status       model.TaskStatus
}

// orphanTaskIDs 找出所有「自己还活着、但祖先链上已有终态」的任务 ID。
//
// 纯函数：给定全量任务视图即可判定，方便无 DB 覆盖各种树形。
// 向上追溯有环时按 seen 掐断（脏数据不至于把启动卡死）。
func orphanTaskIDs(nodes []taskNode) []string {
	byID := make(map[string]taskNode, len(nodes))
	for _, n := range nodes {
		byID[n.TaskID] = n
	}

	var out []string
	for _, n := range nodes {
		if n.Status != model.TaskStatusQueued && n.Status != model.TaskStatusRunning {
			continue // 已终态：不关心
		}
		if hasEndedAncestor(n, byID) {
			out = append(out, n.TaskID)
		}
	}
	sort.Strings(out)
	return out
}

// hasEndedAncestor 沿 parent 链向上找终态祖先。
func hasEndedAncestor(n taskNode, byID map[string]taskNode) bool {
	seen := map[string]bool{n.TaskID: true}
	cur := n
	for depth := 0; depth < maxDescendantDepth; depth++ {
		if cur.ParentTaskID == "" {
			return false // 爬到根都没遇到终态祖先：正常存活
		}
		parent, ok := byID[cur.ParentTaskID]
		if !ok {
			return false // 父记录缺失（已清理/跨库）：不猜，留给看门狗处理
		}
		if seen[parent.TaskID] {
			return false
		}
		seen[parent.TaskID] = true
		if isTerminalStatus(parent.Status) {
			return true
		}
		cur = parent
	}
	return false
}

// maxDescendantDepth 子树展开的最大层数。
//
// 真实链路最深是「工作流任务 → 节点子任务 → deep_search 子任务」三层；
// 给到 8 层是留给将来更深的嵌套，不是给环用的（环由 seen 去重直接掐断）。
const maxDescendantDepth = 8

// maxDescendantNodes 子树展开的累计任务数上限，超出即停止展开（已收集的照常返回）。
const maxDescendantNodes = 500

// collectActiveDescendants 子树展开的纯逻辑：给定「按父 ID 取子任务」的取数函数，
// 广度优先收集仍在排队/运行中的后代。
//
// 抽成纯函数是为了不依赖 DB 就能测（树形、环、深度上限都能覆盖）。
// 终止状态的子任务同样会继续向下展开 —— 一个已完成的节点子任务仍可能
// 遗留一个还在跑的 deep_search 孙任务，不展开就会漏掉它。
func collectActiveDescendants(
	fetch func(parentID string) ([]*model.Task, error), rootTaskID string,
) ([]*model.Task, error) {
	var out []*model.Task
	seen := map[string]bool{rootTaskID: true}
	level := []string{rootTaskID}

	for depth := 0; depth < maxDescendantDepth && len(level) > 0; depth++ {
		var next []string
		for _, parentID := range level {
			subs, err := fetch(parentID)
			if err != nil {
				return nil, err
			}
			for _, st := range subs {
				if seen[st.TaskID] {
					continue // 环 / 重复行：已处理过，跳过
				}
				seen[st.TaskID] = true
				if st.Status == model.TaskStatusQueued || st.Status == model.TaskStatusRunning {
					out = append(out, st)
				}
				next = append(next, st.TaskID)
			}
			if len(seen) > maxDescendantNodes {
				break
			}
		}
		if len(seen) > maxDescendantNodes {
			break
		}
		level = next
	}
	return out, nil
}

// AppendSubTaskParentTurnIDs 把复用本子任务的父 turn id 追加进 task_input.parent_turn_ids。
//
// 场景：LLM 在一批并发 tool_call 里对同一 query 调了多次 deep_search，或进程重启
// 重放重入同一个 pending 子任务 —— 这些 turn 都该在调试页挂上子任务入口，
// 而单值的 parent_turn_id 只能记一个，故复用时把新 turn 追加进数组。
//
// 并发安全：多个 goroutine 同时「读 task_input → append → 写回」会互相覆盖，
// 因此在事务里 SELECT ... FOR UPDATE 锁住 task 行后再改。子任务由 CreateTask 双写
// task / task_temp，终态后 task_temp 行才被删除，所以两张表都要更新，
// 否则运行中的子任务（临时态优先被读到）仍是旧值。
//
// 返回是否实际追加：parentTurnID 为 0、子任务不存在、或该 turn 已登记过时返回
// false，调用方可据此跳过日志等后续动作。
func (d *TaskDAO) AppendSubTaskParentTurnIDs(
	ctx context.Context, subTaskID string, parentTurnID int64,
) (bool, error) {
	if parentTurnID == 0 {
		return false, nil
	}
	db := d.getDB().WithContext(ctx)
	changed := false

	err := db.Transaction(func(tx *gorm.DB) error {
		var task model.Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("task_id = ?", subTaskID).First(&task).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// 子任务已被清理（理论上 task 表常驻）：丢失一次登记不影响主流程
				return nil
			}
			return fmt.Errorf("lock subtask %s: %w", subTaskID, err)
		}
		if task.TaskInput.Data == nil {
			task.TaskInput.Data = &model.TaskInputData{}
		}
		if !task.TaskInput.Data.AppendParentTurnID(parentTurnID) {
			return nil // 幂等：已登记过
		}
		inputJSON, err := json.Marshal(task.TaskInput.Data)
		if err != nil {
			return fmt.Errorf("marshal task_input: %w", err)
		}
		updates := map[string]interface{}{
			"task_input":   string(inputJSON),
			"updated_time": time.Now(),
		}
		if err := tx.Model(&model.Task{}).Where("task_id = ?", subTaskID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update subtask task_input: %w", err)
		}
		if err := tx.Model(&model.TaskTemp{}).Where("task_id = ?", subTaskID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update subtask temp task_input: %w", err)
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// GetLatestTaskByChatID 获取对话最新一条任务（用于列表页预览）
func (d *TaskDAO) GetLatestTaskByChatID(ctx context.Context, chatID string) (*model.Task, error) {
	db := d.getDB().WithContext(ctx)
	var task model.Task
	if err := db.Where("chat_id = ?", chatID).
		Order("created_time DESC").First(&task).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest task failed: %w", err)
	}
	return &task, nil
}

// CountRunningTasksByChatID 统计对话下运行中的任务数
func (d *TaskDAO) CountRunningTasksByChatID(ctx context.Context, chatID string) (int64, error) {
	db := d.getDB().WithContext(ctx)
	var count int64
	if err := db.Model(&model.Task{}).
		Where("chat_id = ? AND status IN ?", chatID,
			[]model.TaskStatus{model.TaskStatusQueued, model.TaskStatusRunning}).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count running tasks failed: %w", err)
	}
	return count, nil
}

// CountTopLevelTasksByStatusForChats 批量统计一组对话的「顶层任务」按状态分组的计数。
//
// 顶层任务 = 对话任务（TaskTypeConversation）+ 工作流任务（TaskTypeWorkflow），
// 即用户在对话里能直接感知的「一条消息 / 一段工作流」。子任务
// （TaskTypeWorkflowSubTask，如 deep_search 的搜索子任务、工作流节点展开的
// 执行单元）不参与统计 —— 它们是执行细节，其整体成败已由所属工作流任务的
// status 聚合体现，重复统计会误报（例如 deep_search 子任务失败但主对话最终
// 收口成功，此时不该在对话列表上打失败标记）。
//
// 只统计「运行中」（queued/running）与「失败」（failed/partial_failed）两类，
// 供对话列表渲染执行中 / 失败图标。单条 GROUP BY 查询避免 N+1。
//
// 返回 map[chatID]map[status]count，缺失的 chatID / status 一律视为 0。
func (d *TaskDAO) CountTopLevelTasksByStatusForChats(
	ctx context.Context, chatIDs []string,
) (map[string]map[model.TaskStatus]int64, error) {
	result := make(map[string]map[model.TaskStatus]int64, len(chatIDs))
	if len(chatIDs) == 0 {
		return result, nil
	}

	db := d.getDB().WithContext(ctx)
	var rows []struct {
		ChatID string           `gorm:"column:chat_id"`
		Status model.TaskStatus `gorm:"column:status"`
		Count  int64            `gorm:"column:cnt"`
	}
	err := db.Model(&model.Task{}).
		Select("chat_id, status, COUNT(*) AS cnt").
		Where("chat_id IN ? AND task_type IN ? AND status IN ?",
			chatIDs,
			[]model.TaskType{model.TaskTypeConversation, model.TaskTypeWorkflow},
			[]model.TaskStatus{
				model.TaskStatusQueued, model.TaskStatusRunning,
				model.TaskStatusFailed, model.TaskStatusPartialFailed,
			}).
		Group("chat_id, status").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("count top-level tasks by status: %w", err)
	}

	for _, r := range rows {
		if result[r.ChatID] == nil {
			result[r.ChatID] = make(map[model.TaskStatus]int64)
		}
		result[r.ChatID][r.Status] = r.Count
	}
	return result, nil
}

// FindTasksByStatus 根据类型和状态查询任务（调度器用）
func (d *TaskDAO) FindTasksByStatus(
	ctx context.Context, taskType model.TaskType, status model.TaskStatus, limit int,
) ([]*model.Task, error) {
	db := d.getDB().WithContext(ctx)
	var tasks []*model.Task
	query := db.Where("task_type = ? AND status = ?", taskType, status)
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Order("created_time ASC").Find(&tasks).Error; err != nil {
		return nil, fmt.Errorf("find tasks failed: %w", err)
	}
	return tasks, nil
}

func (d *TaskDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}

// isTerminalStatus 判断是否为终态
func isTerminalStatus(s model.TaskStatus) bool {
	return s == model.TaskStatusCompleted ||
		s == model.TaskStatusFailed ||
		s == model.TaskStatusStopped ||
		s == model.TaskStatusPartialFailed
}
