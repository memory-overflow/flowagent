// Package actuator — Workflow 阶段执行器
//
// WorkflowActuator 负责 DAG 工作流的调度与执行。
//
// 整体思路参考 fusion-media-platform 的 workflow_actuator.go：
//
//	把工作流任务（task_type=workflow，DAG 存 task_output）展开为一批
//	单 Agent 子任务（task_type=workflow_subtask，parent_task_id 指向工作流任务），
//	根据 depends_on 依赖关系驱动推进 —— 无依赖（或依赖已全部完成）的节点
//	先创建子任务，子任务完成后继续启动下游节点，直到全部完成或任一失败。
//
// 与参考实现的差异：本项目不做「依赖计数 + 内存 flowContext」，
// 而是**无状态地**在每次 GetAsyncTaskStatus 轮询时从 DB 重建 DAG 与子任务终态。
// 原因：
//   - DAG 本身已持久化在工作流任务 task_output，子任务也各有独立行
//   - 无状态天然支持多副本部署与进程重启恢复，无需处理 flowCache 的
//     一致性/失效问题
//   - 工作流规模小（对话级 DAG），每 1s 重建一次开销可忽略
//
// 单 Agent 子任务复用 functional_call ↔ tool_call 调度链（子任务创建时
// Stage=functional_call_wait，Phase=executing），由 functional_call 执行器
// 的 workflowSubTaskExecImpl 分支组装 prompt（携带工作流目标 + 节点描述 +
// 父节点输出），产出 AgentOutputData 写回子任务 task_output。
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
	"gorm.io/gorm"

	framework "github.com/memory-overflow/light-task-scheduler"
)

// WorkflowActuator DAG 工作流调度执行器。
//
// 无状态：不持有 flowContext，每次轮询从 DB 重建。唯一依赖是 taskDAO，
// 用于读写工作流任务、创建子任务、回写 DAG 进度。llmResolver / convDAO
// 仅用于工作流收口时对绑定节点做一次 LLM 结果汇总（见 summarizeWorkflowResults）。
type WorkflowActuator struct {
	taskDAO     *dao.TaskDAO
	llmResolver *llmreg.Resolver
	convDAO     *dao.ConversationDAO
}

// NewWorkflowActuator 创建 Workflow 阶段执行器
func NewWorkflowActuator(
	taskDAO *dao.TaskDAO, llmResolver *llmreg.Resolver, convDAO *dao.ConversationDAO,
) *WorkflowActuator {
	return &WorkflowActuator{taskDAO: taskDAO, llmResolver: llmResolver, convDAO: convDAO}
}

// Init 初始化（无动作）
func (a *WorkflowActuator) Init(ctx context.Context, task *framework.Task) (*framework.Task, error) {
	return task, nil
}

// Start 启动工作流编排（非阻塞）。
//
// 首次拿到工作流任务时，为「依赖已满足」的节点创建单 Agent 子任务。
// 随后框架调用 ToRunningStatus 把工作流任务从 workflow_wait 拨到
// workflow_doing，真正的推进由 GetAsyncTaskStatus 周期完成。
func (a *WorkflowActuator) Start(ctx context.Context, task *framework.Task) (*framework.Task, bool, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return task, false, fmt.Errorf("workflow-actuator: task item is not *model.TaskTemp")
	}

	dag := parseWorkflowOutput(temp.TaskOutput, temp.ChatID)
	if dag.ActiveNodeCount() == 0 {
		// 空 DAG（脏数据 / 全部节点已归档）不应出现在执行链路，
		// 但也不判失败 —— 直接让它走完（下一次状态轮询会判 SUCCESS）。
		log.Printf("[workflow-actuator][%s] no active node, nothing to run (total=%d)",
			temp.TaskID, len(dag.Nodes))
		return task, false, nil
	}

	if err := a.spawnReadyNodes(ctx, &temp.Task, dag); err != nil {
		return task, false, fmt.Errorf("workflow-actuator: spawn ready nodes failed: %w", err)
	}

	log.Printf("[workflow-actuator][%s] workflow started (%d/%d active nodes)",
		temp.TaskID, dag.ActiveNodeCount(), len(dag.Nodes))
	return task, false, nil
}

// GetOutput 工作流任务的输出即其 DAG（已随执行周期回写 task_output）。
// 框架的 Persistencer 为 nil，本方法不会被真正调用，返回 nil 即可。
func (a *WorkflowActuator) GetOutput(ctx context.Context, task *framework.Task) (interface{}, error) {
	return nil, nil
}

// Stop 停止工作流：终止所有仍在排队/运行中的子任务。
func (a *WorkflowActuator) Stop(ctx context.Context, task *framework.Task) error {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return fmt.Errorf("workflow-actuator: task item is not *model.TaskTemp")
	}
	return a.stopSubTasks(ctx, temp.TaskID)
}

// stopSubTasks 终止指定工作流下整棵子树里仍在排队/运行中的后代任务。
//
// 必须递归而不是只扫一层：节点子任务在执行中可能调 deep_search，派发出的
// 搜索子任务挂在节点之下（孙层）。只停直接子任务会让这些搜索子任务在
// 工作流失败/停止收口后继续被调度执行 —— 表现为「工作流已经停了，
// 搜索还在跑」。
func (a *WorkflowActuator) stopSubTasks(ctx context.Context, parentTaskID string) error {
	pendings, err := a.taskDAO.ListActiveDescendants(ctx, parentTaskID)
	if err != nil {
		return fmt.Errorf("workflow-actuator: list descendants to stop: %w", err)
	}
	for _, st := range pendings {
		if err := a.taskDAO.UpdateTaskStatus(ctx, st.TaskID,
			model.TaskStatusStopped, nil, 0); err != nil {
			log.Printf("[workflow-actuator][%s] stop task %s failed: %v",
				parentTaskID, st.TaskID, err)
		}
	}
	if len(pendings) > 0 {
		log.Printf("[workflow-actuator][%s] stopped %d descendant task(s)",
			parentTaskID, len(pendings))
	}
	return nil
}

// reapOrphanSubTasks 回收已创建但未能写回 DAG 的孤儿子任务。
//
// 版本冲突放弃 DAG 回写后，本轮新建的子任务 TaskID 没有落进 DAG，
// 下一轮无状态重建会为同一节点再次创建子任务；这些孤儿若不回收，
// 仍会被 functional_call 调度器捞起执行，白白多跑一次 LLM。
// 标记为 stopped（终态、task_temp 被清理）后，它们不会再被调度。
func (a *WorkflowActuator) reapOrphanSubTasks(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if err := a.taskDAO.UpdateTaskStatus(ctx, id, model.TaskStatusStopped, nil, 0); err != nil {
			log.Printf("[workflow-actuator] reap orphan subtask %s failed: %v", id, err)
			continue
		}
		log.Printf("[workflow-actuator] reaped orphan subtask %s (DAG write conflicted)", id)
	}
}

// GetAsyncTaskStatus 周期推进 DAG：扫描子任务终态，驱动下游节点，回写 DAG。
//
// 返回每个工作流任务的异步状态：
//   - 任一节点失败       → FAILED（并触发 Stop 终止其余子任务）
//   - 所有节点完成       → SUCCESS
//   - 否则               → RUNNING
func (a *WorkflowActuator) GetAsyncTaskStatus(
	ctx context.Context, tasks []framework.Task,
) ([]framework.AsyncTaskStatus, error) {
	status := make([]framework.AsyncTaskStatus, len(tasks))
	for i, task := range tasks {
		st, err := a.driveWorkflow(ctx, &task)
		if err != nil {
			// 推进出错不能判失败 —— 单次 DB 抖动不应杀死整个工作流，
			// 报 RUNNING 交由下一轮重试。
			log.Printf("[workflow-actuator][%s] drive workflow error: %v", task.TaskId, err)
			status[i] = framework.AsyncTaskStatus{
				TaskStatus: framework.TASK_STATUS_RUNNING,
			}
			continue
		}
		status[i] = st
	}
	return status, nil
}

// driveWorkflow 单次推进一个工作流任务，返回其异步状态。
func (a *WorkflowActuator) driveWorkflow(
	ctx context.Context, task *framework.Task,
) (framework.AsyncTaskStatus, error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return framework.AsyncTaskStatus{}, fmt.Errorf("task item is not *model.TaskTemp")
	}
	wfTask := &temp.Task

	dag := parseWorkflowOutput(wfTask.TaskOutput, wfTask.ChatID)
	nodes := dag.Nodes
	// 已归档节点（失败分支，见 pruneAbandonedBranch）不参与本轮推进：
	// 它们是上一轮规划的残留，既不该再建子任务，也不该计入成败与进度。
	active := dag.ActiveNodeCount()
	if active == 0 {
		// 空 DAG（脏数据 / 全部节点已归档）不应出现在执行链路，
		// 但也不判失败 —— 直接让它走完（下一次状态轮询会判 SUCCESS）。
		// 全部节点已归档时绑定节点只剩失败/废弃结果，同样要走一次同步，
		// 否则来源对话任务会永远停在「执行中」。
		if err := a.taskDAO.SyncWorkflowSourceTaskStatuses(ctx, dag, model.TaskStatusCompleted); err != nil {
			log.Printf("[workflow-actuator][%s] sync source task statuses (all archived) failed: %v",
				wfTask.TaskID, err)
		}
		// 全部节点已归档同样是收口：为来源对话任务做一次结果汇总（此时绑定
		// 节点只剩失败/废弃，汇总应给出失败原因）。
		a.summarizeWorkflowResults(ctx, wfTask, dag)
		log.Printf("[workflow-actuator][%s] no active node, nothing to run (total=%d)",
			temp.TaskID, len(dag.Nodes))
		return framework.AsyncTaskStatus{TaskStatus: framework.TASK_STATUS_SUCCESS}, nil
	}

	// 节点 id → 下标
	idx := make(map[string]int, len(nodes))
	for i := range nodes {
		idx[nodes[i].NodeID] = i
	}

	// 子任务 id → 子任务
	subTasks, err := a.taskDAO.ListSubTasksByParent(ctx, wfTask.TaskID)
	if err != nil {
		return framework.AsyncTaskStatus{}, fmt.Errorf("list subtasks: %w", err)
	}
	subByID := make(map[string]*model.Task, len(subTasks))
	for _, st := range subTasks {
		subByID[st.TaskID] = st
	}

	changed := false
	anyFailed := false
	anyStopped := false
	// spawned 本轮新建、但尚未写回 DAG 的子任务 ID。若后续 DAG 回写发生
	// 版本冲突（放弃回写），这些子任务会脱离 DAG 成为孤儿，需在冲突时回收，
	// 否则它们仍会被 functional_call 调度器捞起执行（白跑一次 LLM）。
	var spawned []string

	// Phase 1：用子任务终态回填节点状态/输出。
	for i := range nodes {
		node := &nodes[i]
		if node.Archived {
			continue // 已废弃的分支：不再推进，也不参与成败判定
		}
		if node.Status == model.TaskStatusCompleted || node.Status == model.TaskStatusFailed ||
			node.Status == model.TaskStatusStopped || node.Status == model.TaskStatusAbandoned {
			continue // 已终态，不再回填
		}
		if node.TaskID == "" {
			continue // 尚未创建子任务
		}
		st := subByID[node.TaskID]
		if st == nil {
			// 子任务记录丢失（极端：被人工清理）。视为失败避免下游无限等待。
			node.Status = model.TaskStatusFailed
			anyFailed = true
			changed = true
			log.Printf("[workflow-actuator][%s] node %s subtask %s missing, mark failed",
				wfTask.TaskID, node.NodeID, node.TaskID)
			continue
		}
		switch st.Status {
		case model.TaskStatusCompleted:
			node.Status = model.TaskStatusCompleted
			node.Output = parseAgentOutput(st.TaskOutput)
			changed = true
			log.Printf("[workflow-actuator][%s] node %s completed", wfTask.TaskID, node.NodeID)
		case model.TaskStatusFailed, model.TaskStatusPartialFailed:
			node.Status = model.TaskStatusFailed
			anyFailed = true
			changed = true
			log.Printf("[workflow-actuator][%s] node %s failed (subtask status=%d)",
				wfTask.TaskID, node.NodeID, st.Status)
		case model.TaskStatusStopped:
			// 子任务被用户停止（handler.StopTask 级联）→ 节点标 stopped，
			// 单独计入 anyStopped，绝不能归入 failed，否则用户主动停止会被误判为失败。
			node.Status = model.TaskStatusStopped
			anyStopped = true
			changed = true
			log.Printf("[workflow-actuator][%s] node %s stopped (subtask status=%d)",
				wfTask.TaskID, node.NodeID, st.Status)
		case model.TaskStatusRunning:
			// 子任务已被调度器拨到 running，同步节点状态，
			// 让前端 DAG 节点能显示「执行中」而不是停留在 queued。
			if node.Status != model.TaskStatusRunning {
				node.Status = model.TaskStatusRunning
				changed = true
			}
		default:
			// queued：仍在排队
		}
	}

	// Phase 2：为「依赖已全部完成」的节点创建子任务。
	// 节点按声明顺序遍历即可 —— 本轮新建子任务的节点其依赖尚未完成
	// （依赖节点刚创建，status 还是 queued），不会被误判为满足。
	for i := range nodes {
		node := &nodes[i]
		if node.Archived {
			continue
		}
		if node.Status == model.TaskStatusCompleted || node.Status == model.TaskStatusFailed ||
			node.Status == model.TaskStatusStopped || node.Status == model.TaskStatusAbandoned {
			continue
		}
		if node.TaskID != "" {
			continue // 已有子任务
		}
		if !a.depsSatisfied(nodes, idx, node) {
			continue
		}
		subTaskID, err := a.createSubTask(ctx, wfTask, dag, node)
		if err != nil {
			// 创建子任务失败按节点失败处理，避免静默卡死
			node.Status = model.TaskStatusFailed
			anyFailed = true
			changed = true
			log.Printf("[workflow-actuator][%s] create subtask for node %s failed: %v",
				wfTask.TaskID, node.NodeID, err)
			continue
		}
		node.TaskID = subTaskID
		node.Status = model.TaskStatusQueued
		spawned = append(spawned, subTaskID)
		changed = true
		log.Printf("[workflow-actuator][%s] spawned subtask %s for node %s",
			wfTask.TaskID, subTaskID, node.NodeID)
	}

	// 回写 DAG 与进度
	//
	// 分母只算活跃节点：把已归档的失败分支算进来，进度永远到不了 100%，
	// 失败判定也会被上一轮的历史 failed 节点反复触发（工作流刚被续接重启
	// 就立刻再次判失败，续接形同无效）。
	completed := 0
	for i := range nodes {
		if nodes[i].Archived {
			continue
		}
		switch nodes[i].Status {
		case model.TaskStatusCompleted:
			completed++
		case model.TaskStatusFailed, model.TaskStatusPartialFailed:
			anyFailed = true
		case model.TaskStatusStopped:
			anyStopped = true
		}
	}
	progress := float64(completed) / float64(active) * 100.0

	// 工作流整体状态（与本函数末尾返回给框架的判定保持同一口径）：
	// 来源对话任务的状态绑定要用它落规则 3 ——「节点没跑完但工作流已整体失败」。
	wfStatus := model.TaskStatusRunning
	switch {
	case anyFailed:
		wfStatus = model.TaskStatusFailed
	case anyStopped:
		wfStatus = model.TaskStatusStopped
	case completed == active:
		wfStatus = model.TaskStatusCompleted
	}

	// 把来源对话任务的状态绑定到本轮规划的节点上（详见 DeriveBoundTaskStatus）。
	// 放在 DAG 回写之前：即便下面版本冲突放弃回写，节点状态也是从子任务
	// 终态刚回填的真实值，绑定同步依然有效。失败只记日志 —— 绑定是展示层
	// 的增强，不该拖垮工作流推进。
	if err := a.taskDAO.SyncWorkflowSourceTaskStatuses(ctx, dag, wfStatus); err != nil {
		log.Printf("[workflow-actuator][%s] sync source task statuses failed: %v",
			wfTask.TaskID, err)
	}

	// 工作流收口（终态）时，为每个来源对话任务做一次 LLM 结果汇总并落库。
	// 放在 Sync 之后：绑定节点状态刚回填，汇总拿到的就是最终状态。
	// 幂等由 summarizeWorkflowResults 内部保证（已汇总的对话任务跳过）。
	if wfStatus == model.TaskStatusCompleted || wfStatus == model.TaskStatusFailed ||
		wfStatus == model.TaskStatusStopped {
		a.summarizeWorkflowResults(ctx, wfTask, dag)
	}

	if changed {
		dag.Nodes = nodes
		// 聚合各子任务的产物，形成整段工作流的交付清单。
		// 放在回写前重算：本轮可能有节点刚完成并带回新产物。
		dag.OutputFiles = aggregateWorkflowOutputFiles(dag.OutputFiles, nodes)
		dag.Version = wfTask.DAGVersion + 1
		outJSON, err := json.Marshal(dag)
		if err != nil {
			return framework.AsyncTaskStatus{}, fmt.Errorf("marshal workflow dag: %w", err)
		}
		err = a.taskDAO.UpdateWorkflowRun(ctx, wfTask.TaskID, string(outJSON), progress, wfTask.DAGVersion)
		if errors.Is(err, dao.ErrWorkflowVersionConflict) {
			// 版本冲突：appendWorkflowNodes 追加了节点，或另一副本推进了 DAG。
			// 放弃本轮回写 —— 下一轮 GetRunningTask 会从 DB 重新加载最新 DAG，
			// 无状态重建后继续驱动。本轮已创建但未写回 TaskID 的子任务会脱离
			// DAG 成为孤儿，先回收掉，避免它们被调度器捞起白跑一次 LLM。
			a.reapOrphanSubTasks(ctx, spawned)
			log.Printf("[workflow-actuator][%s] persist DAG version conflict (have %d), defer to next round",
				wfTask.TaskID, wfTask.DAGVersion)
			return framework.AsyncTaskStatus{
				TaskStatus: framework.TASK_STATUS_RUNNING,
				Progress:   progress,
			}, nil
		}
		if err != nil {
			return framework.AsyncTaskStatus{}, fmt.Errorf("persist workflow run: %w", err)
		}
	}

	switch {
	case anyFailed:
		// 任一节点失败 → 停掉其余仍在跑的子任务，避免空耗。
		// 停止动作在本次轮询内同步完成，紧接着返回 FAILED 让框架收口工作流。
		if err := a.stopSubTasks(ctx, wfTask.TaskID); err != nil {
			log.Printf("[workflow-actuator][%s] stop remaining subtasks failed: %v",
				wfTask.TaskID, err)
		}
		return framework.AsyncTaskStatus{
			TaskStatus:   framework.TASK_STATUS_FAILED,
			FailedReason: fmt.Errorf("workflow has failed node"),
			Progress:     progress,
		}, nil
	case anyStopped:
		// 子任务被用户停止（handler.StopTask 级联）→ 工作流收口为「已停止」，
		// 而非「失败」。停掉其余仍在跑的子任务，并把父任务标 stopped
		// （幂等兜底，覆盖 handler 竞态窗口内父任务尚未标 stopped 的情况）。
		if err := a.stopSubTasks(ctx, wfTask.TaskID); err != nil {
			log.Printf("[workflow-actuator][%s] stop remaining subtasks failed: %v",
				wfTask.TaskID, err)
		}
		if err := a.taskDAO.UpdateTaskStatus(ctx, wfTask.TaskID,
			model.TaskStatusStopped, nil, progress); err != nil {
			log.Printf("[workflow-actuator][%s] mark stopped failed: %v", wfTask.TaskID, err)
		}
		return framework.AsyncTaskStatus{
			TaskStatus: framework.TASK_STATUS_STOPED,
			Progress:   progress,
		}, nil
	case completed == active:
		return framework.AsyncTaskStatus{
			TaskStatus: framework.TASK_STATUS_SUCCESS,
			Progress:   progress,
		}, nil
	default:
		return framework.AsyncTaskStatus{
			TaskStatus: framework.TASK_STATUS_RUNNING,
			Progress:   progress,
		}, nil
	}
}

// spawnReadyNodes 为依赖已满足的节点创建子任务（Start 阶段调用）。
func (a *WorkflowActuator) spawnReadyNodes(
	ctx context.Context, wfTask *model.Task, dag *model.WorkflowOutputData,
) error {
	idx := make(map[string]int, len(dag.Nodes))
	for i := range dag.Nodes {
		idx[dag.Nodes[i].NodeID] = i
	}

	changed := false
	var spawned []string
	for i := range dag.Nodes {
		node := &dag.Nodes[i]
		if node.Archived {
			continue // 已废弃的分支：不再为它建子任务
		}
		if node.Status == model.TaskStatusCompleted || node.Status == model.TaskStatusFailed ||
			node.Status == model.TaskStatusStopped || node.Status == model.TaskStatusAbandoned {
			continue
		}
		if node.TaskID != "" {
			continue // 已有子任务（重启恢复）
		}
		if !a.depsSatisfied(dag.Nodes, idx, node) {
			continue
		}
		subTaskID, err := a.createSubTask(ctx, wfTask, dag, node)
		if err != nil {
			return err
		}
		node.TaskID = subTaskID
		node.Status = model.TaskStatusQueued
		spawned = append(spawned, subTaskID)
		changed = true
	}

	if !changed {
		return nil
	}
	dag.Version = wfTask.DAGVersion + 1
	outJSON, err := json.Marshal(dag)
	if err != nil {
		return fmt.Errorf("marshal workflow dag: %w", err)
	}
	err = a.taskDAO.UpdateWorkflowRun(ctx, wfTask.TaskID, string(outJSON), 0, wfTask.DAGVersion)
	if errors.Is(err, dao.ErrWorkflowVersionConflict) {
		// Start 阶段冲突：工作流刚被拨到 running 即被并发追加了节点。
		// 放弃本轮回写，交由 driveWorkflow 下一轮无状态重建驱动。
		// 本轮已创建的子任务同样成了孤儿，一并回收。
		a.reapOrphanSubTasks(ctx, spawned)
		log.Printf("[workflow-actuator][%s] start persist DAG version conflict (have %d), defer",
			wfTask.TaskID, wfTask.DAGVersion)
		return nil
	}
	return err
}

// depsSatisfied 判断节点的依赖是否已全部完成。
//
// 依赖 id 不在 DAG 里（规划数据异常）视为已满足，避免下游永远卡住。
// 依赖已归档同理：它永远不会再变成 completed，卡在这里只会让整条工作流
// 停在 running 直到超时。规划期的锚点校验已禁止新节点依赖废弃节点，
// 能走到这里的都是历史数据，放行比卡死更合理。
func (a *WorkflowActuator) depsSatisfied(
	nodes []model.WorkflowNodeData, idx map[string]int, node *model.WorkflowNodeData,
) bool {
	for _, depID := range node.DependsOn {
		i, ok := idx[depID]
		if !ok || nodes[i].Archived {
			continue
		}
		if nodes[i].Status != model.TaskStatusCompleted {
			return false
		}
	}
	return true
}

// createSubTask 为一个节点创建单 Agent 子任务。
//
// 子任务 Stage=functional_call_wait、Phase=executing，会被 functional_call
// 调度器捞起执行；TaskType=workflow_subtask 让它走 workflowSubTaskExecImpl
// 分支组装 prompt（不带对话历史，只带工作流上下文 + 用户附件）。
func (a *WorkflowActuator) createSubTask(
	ctx context.Context, wfTask *model.Task, dag *model.WorkflowOutputData, node *model.WorkflowNodeData,
) (string, error) {
	// 用户附件必须在建任务时就内联进 input_text：子任务的上下文里没有
	// 主对话历史，也就没有「用户在第几轮传了什么」这件事。不带上它们，
	// 要求「按我上传的表格写报告」这类工作流，节点根本不知道有文件存在
	// （文件明明就在同一工作区里，只是没人告诉它路径）。
	files := loadChatInputFiles(ctx, a.taskDAO, wfTask.ChatID)

	subTaskID := uuid.New().String()
	now := time.Now()
	task := &model.Task{
		TaskID:       subTaskID,
		ParentTaskID: wfTask.TaskID,
		ChatID:       wfTask.ChatID,
		TaskType:     model.TaskTypeWorkflowSubTask,
		TaskInput: model.JsonField[*model.TaskInputData]{
			Data: &model.TaskInputData{
				InputText: buildSubTaskPrompt(wfTask, dag, node,
					files, resolveChatWorkspaceDir(wfTask.ChatID)),
				// 节点独占产出目录：并行节点的执行窗口重叠，只靠 mtime
				// 判定归属会把别人的文件算进来（实测三个并行节点各自报出
				// 同样的 24 个文件）。按目录归属是确定的。
				ArtifactDir: buildWorkflowNodeArtifactDir(wfTask.TaskID, node.NodeID),
			},
		},
		Stage:     int(model.StageFunctionalCallWait),
		Phase:     int(model.PhaseExecuting),
		Status:    model.TaskStatusQueued,
		Progress:  0,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := dao.Transaction(func(tx *gorm.DB) error {
		return (&dao.TaskDAO{Tx: tx}).CreateTask(ctx, task)
	}); err != nil {
		return "", fmt.Errorf("create subtask: %w", err)
	}
	return subTaskID, nil
}

// loadChatInputFiles 汇总本对话用户上传过的附件（跨轮次、按路径去重）。
//
// 为什么不直接取「触发工作流的那一轮」的 input_files：
//   - 工作流可以被**追加节点**（用户后续又发了新消息、传了新文件），
//     那时新文件只在新一轮 task 的 input_files 里；
//   - 更早的老数据里，工作流任务的 task_input 压根没抄附件字段。
//
// 因此按对话汇总：附件是工作区里的既有文件，把它们列全比只列一轮更接近
// 用户的真实意图。查询失败返回 nil —— 缺附件清单不该阻断工作流推进。
func loadChatInputFiles(
	ctx context.Context, taskDAO *dao.TaskDAO, chatID string,
) []model.FileInfo {
	if taskDAO == nil || chatID == "" {
		return nil
	}
	tasks, err := taskDAO.ListTasksByChatID(ctx, chatID)
	if err != nil {
		log.Printf("[workflow-actuator] list chat tasks for input files failed (ignore): %v", err)
		return nil
	}
	seen := make(map[string]bool)
	files := make([]model.FileInfo, 0, 4)
	for _, t := range tasks {
		if t.TaskInput.Data == nil {
			continue
		}
		for _, f := range t.TaskInput.Data.InputFiles {
			// 同一份文件在多个 task 里重复出现（用户连续几轮引用同一份）
			key := f.Path
			if key == "" {
				key = f.Name
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			files = append(files, f)
		}
	}
	return files
}

// buildSubTaskPrompt 组装子任务的执行 prompt：
//
//	工作流总体目标 + 本步骤名称/说明/输入 + 前置步骤（depends_on）的输出
//	+ 用户附件清单。
//
// 前置节点的输出在创建子任务时已全部就绪（depsSatisfied 保证），
// 因此可以内联进 input_text，无需执行期再查。
//
// files / wsDir 由调用方传入而不是这里自己查：保持纯函数，单测可以直接
// 构造（见 TestBuildSubTaskPromptIncludesInputFiles）。
func buildSubTaskPrompt(
	wfTask *model.Task, dag *model.WorkflowOutputData, node *model.WorkflowNodeData,
	files []model.FileInfo, wsDir string,
) string {
	var b strings.Builder

	// 用户原始输入：子任务看不到主对话历史，用户到底要什么、提到了哪些文件，
	// 全靠这一段。plan_summary 是模型概括，可能丢关键细节（文件路径、文件名、
	// 格式要求等），所以原文必须独立注入、放在最前面，让节点先锚定用户原话，
	// 再看概括与调研结论。丢失它的直接后果是「规划期读到的文件路径」在节点
	// 手里消失，节点只能凭空重找。
	if userInput := strings.TrimSpace(renderUserInput(wfTask.TaskInput.Data, "")); userInput != "" {
		b.WriteString("【用户原始输入】\n")
		b.WriteString(userInput)
		b.WriteString("\n\n")
	}

	goal := strings.TrimSpace(dag.PlanSummary)
	if goal != "" {
		b.WriteString("【工作流总体目标】\n")
		b.WriteString(goal)
		b.WriteString("\n\n")
	}

	// 规划期调研结论：这是「已经查过的事实」，注入它是为了让本节点不必重搜。
	//
	// 位置放在总体目标之后、本步骤之前：它是背景事实而不是指令，先给事实
	// 再给任务，模型更容易区分「已知的」与「要做的」。
	if section := renderPlanContextSection(dag.PlanContext); section != "" {
		b.WriteString(section)
		b.WriteString("\n")
	}

	// 节点级上下文：这个节点特有的、全局没覆盖到的背景。续接节点的 context
	// 承载「本轮用户新增的诉求」—— 全局的「用户原始输入」是创建轮原文，
	// 续接轮的新输入只有靠这里传进来。
	if s := strings.TrimSpace(node.Context); s != "" {
		b.WriteString("【本步骤背景】\n")
		b.WriteString(s)
		b.WriteString("\n\n")
	}

	b.WriteString("【你负责的步骤】\n")
	b.WriteString("步骤名称：" + node.Name + "\n")
	if s := strings.TrimSpace(node.Description); s != "" {
		b.WriteString("步骤说明：" + s + "\n")
	}
	if s := strings.TrimSpace(node.Input.InputText); s != "" {
		b.WriteString("本步骤输入：" + s + "\n")
	}

	// 前置步骤输出
	if len(node.DependsOn) > 0 {
		byID := make(map[string]*model.WorkflowNodeData, len(dag.Nodes))
		for i := range dag.Nodes {
			byID[dag.Nodes[i].NodeID] = &dag.Nodes[i]
		}
		b.WriteString("\n【前置步骤的输出】\n")
		for _, depID := range node.DependsOn {
			dep, ok := byID[depID]
			if !ok || dep.Output == nil {
				continue
			}
			name := dep.Name
			if name == "" {
				name = depID
			}
			b.WriteString("◆ " + name + " 的输出：\n")
			b.WriteString(truncateRunes(strings.TrimSpace(dep.Output.ReplyText), 4000))
			b.WriteString("\n")
			// 上游产出的文件清单必须带上：上游把长文落盘后，reply_text 里只剩摘要，
			// 下游若不知道文件路径就拿不到完整内容，只能基于摘要臆造。
			if len(dep.Output.OutputFiles) > 0 {
				b.WriteString("  该步骤产出的文件（可用 read_file 读取完整内容）：\n")
				for _, f := range dep.Output.OutputFiles {
					p := f.Path
					if p == "" {
						p = f.Name
					}
					fmt.Fprintf(&b, "  - `%s`", p)
					if f.Size > 0 {
						fmt.Fprintf(&b, "（%s）", humanSize(f.Size))
					}
					b.WriteString("\n")
				}
			}
		}
	}

	// 用户附件：子任务看不到主对话历史，这是它知道「用户传了什么」的唯一途径。
	// 只渲染清单（含可内联的小文件预览），不拼用户输入原文 —— 那是历史。
	if len(files) > 0 {
		b.WriteString("\n")
		b.WriteString(renderInputFileManifest(files, wsDir))
		b.WriteString("\n")
	}

	b.WriteString("\n请完成上述这一个步骤，并产出本步骤的结果作为最终回复。\n")
	b.WriteString(renderSubTaskOutputContract())
	return b.String()
}

// renderSubTaskOutputContract 子任务的输出形态契约。
//
// 两条都是被真实问题逼出来的：
//  1. 「结论必须写在回复文本里」——下游节点读到的是本节点的 reply_text，
//     而落盘文件的完整内容它未必会去读。实测一个选型调研的汇总节点上游里，
//     有节点的回复只剩「见 xx.md」，下游拿到的就是一句空话。
//  2. 「落盘也要给中等详细度结论」——长回复转存后 reply_text 只剩开头，
//     若原本就只写了一句「见文件」，转存后连这一句都可能被结构截断吃掉。
func renderSubTaskOutputContract() string {
	return `【本步骤的输出契约】
- 结果直接写在回复文本里。下游步骤读到的是你的回复文本，只回「详见 xx.md」
  会让下游拿不到任何结论。
- 只有正文确实超过 4000 字（成篇报告、大表格、完整代码）才落盘；
  落盘时回复里仍要给出中等详细度的结论：关键结论、核心数据、要点与依据
  （500~1500 字），末尾再给文件路径与全文规模。
- 文件命名要有意义（如 report-agent-platform.md、compare-table.csv），
  不要用时间戳或随机串；中间产物写到 .scratch/ 下。

请完成上述这一个步骤，并产出本步骤的结果作为最终回复。
`
}

// renderPlanContextSection 把规划期已知信息渲染成节点 prompt 的一段。
//
// 措辞上有两处是刻意的：
//  1. 明确说「已掌握，可直接引用」——不这么说，模型会把这些当成「某个
//     上游的猜测」而重新验证一遍，注入就白做了。
//  2. 同时给出「需要更细或更新时再检索」的口子 —— 已知信息是规划期
//     的快照，节点有时确实需要更新的数据（版本号、价格这类易变信息），
//     硬性禁止检索会让它基于过期事实产出。
func renderPlanContextSection(pc *model.WorkflowPlanContext) string {
	if pc.IsEmpty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("【规划期已知信息】（规划阶段已掌握，可直接引用，不要重复检索）\n")
	// 主字段：模型自由声明的全部已知信息。旧数据没有 Context 时，
	// 回退到旧字段 Summary / KeyFacts 拼一份。
	if ctxText := strings.TrimSpace(pc.Context); ctxText != "" {
		b.WriteString(ctxText)
		b.WriteString("\n")
	} else {
		if s := strings.TrimSpace(pc.Summary); s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
		if len(pc.KeyFacts) > 0 {
			b.WriteString("\n关键事实：\n")
			for _, f := range pc.KeyFacts {
				b.WriteString("- " + strings.TrimSpace(f) + "\n")
			}
		}
	}
	if len(pc.Files) > 0 {
		b.WriteString("\n调研素材文件（需要细节时用 read_file 读）：\n")
		for _, f := range pc.Files {
			p := f.Path
			if p == "" {
				p = f.Name
			}
			fmt.Fprintf(&b, "- `%s`", p)
			if f.Size > 0 {
				fmt.Fprintf(&b, "（%s）", humanSize(f.Size))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("只有当这些信息不足以完成本步骤，或需要更新/更细的数据时，才做补充检索。\n")
	return b.String()
}

// parseAgentOutput 把子任务的 task_output（AgentOutputData JSON）解析成结构体。
// 解析失败返回空壳，不阻断工作流推进（结果仅供下游引用与前端展示）。
func parseAgentOutput(raw string) *model.AgentOutputData {
	out := &model.AgentOutputData{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		log.Printf("[workflow-actuator] parse subtask output failed: %v (raw=%s)",
			err, truncate(raw, 200))
		return &model.AgentOutputData{}
	}
	return out
}

// ============================================================
// 工作流收口后的结果汇总（LLM）
// ============================================================

// summarizeWorkflowResults 在工作流进入终态时，为每个来源对话任务做一次
// LLM 结果汇总，落库到其 agent_output.WorkflowSummary。
//
// 为什么用 LLM 而不是机械拼接节点输出：机械拼接只会罗列中间过程，用户真正
// 要的是「成功→最终产物与结论 / 失败→失败位置与原因」的语义汇总，这需要
// 模型理解各节点产出之间的关系。汇总是一次性动作（工作流收口时），不是
// 高频路径，多一次 LLM 调用可接受。
//
// 幂等：对话任务的 WorkflowSummary 已非空则跳过，重复收口不会重复汇总。
// 汇总失败只记日志不阻断 —— 工作流结果本身仍在 DAG 里可查。
func (a *WorkflowActuator) summarizeWorkflowResults(
	ctx context.Context, wfTask *model.Task, dag *model.WorkflowOutputData,
) {
	if dag == nil || len(dag.Nodes) == 0 || a.llmResolver == nil {
		return
	}
	grouped := make(map[string][]model.WorkflowNodeData)
	for _, n := range dag.Nodes {
		if n.SourceTaskID != "" {
			grouped[n.SourceTaskID] = append(grouped[n.SourceTaskID], n)
		}
	}
	for sourceTaskID, nodes := range grouped {
		a.summarizeForSourceTask(ctx, wfTask, sourceTaskID, nodes)
	}
}

// summarizeForSourceTask 对单个来源对话任务的绑定节点做一次 LLM 汇总并落库。
func (a *WorkflowActuator) summarizeForSourceTask(
	ctx context.Context, wfTask *model.Task, sourceTaskID string, nodes []model.WorkflowNodeData,
) {
	// 只处理对话任务；读出现有 agent_output 做幂等判断。
	task, err := a.taskDAO.GetTaskByID(ctx, sourceTaskID)
	if err != nil || task == nil || task.TaskType != model.TaskTypeConversation {
		return
	}
	var out model.AgentOutputData
	if task.TaskOutput != "" {
		_ = json.Unmarshal([]byte(task.TaskOutput), &out)
	}
	if strings.TrimSpace(out.WorkflowSummary) != "" {
		return // 已汇总过，跳过
	}

	// 尚未到终态（绑定节点还在跑）不汇总。
	bound, _ := model.DeriveBoundTaskStatus(nodes, wfTask.Status)
	if bound == model.TaskStatusRunning || bound == model.TaskStatusQueued {
		return
	}

	summary, err := a.callSummarizeLLM(ctx, wfTask, nodes, bound)
	if err != nil {
		log.Printf("[workflow-actuator][%s] summarize bound workflow for source %s failed: %v",
			wfTask.TaskID, sourceTaskID, err)
		return
	}
	out.WorkflowSummary = summary
	outJSON, err := json.Marshal(out)
	if err != nil {
		log.Printf("[workflow-actuator][%s] marshal summarized output for %s failed: %v",
			wfTask.TaskID, sourceTaskID, err)
		return
	}
	if err := a.taskDAO.UpdateTaskOutput(ctx, sourceTaskID, string(outJSON)); err != nil {
		log.Printf("[workflow-actuator][%s] persist summary for %s failed: %v",
			wfTask.TaskID, sourceTaskID, err)
		return
	}
	log.Printf("[workflow-actuator][%s] summarized bound workflow for source %s (%d nodes, bound=%d)",
		wfTask.TaskID, sourceTaskID, len(nodes), bound)
}

// callSummarizeLLM 调用 LLM 对绑定节点结果做汇总，返回汇总文本。
func (a *WorkflowActuator) callSummarizeLLM(
	ctx context.Context, wfTask *model.Task, nodes []model.WorkflowNodeData, bound model.TaskStatus,
) (string, error) {
	client, err := resolveLLMClientWith(ctx, wfTask.ChatID, a.llmResolver, a.convDAO)
	if err != nil {
		return "", err
	}
	goal := ""
	if wfTask.TaskInput.Data != nil {
		goal = strings.TrimSpace(wfTask.TaskInput.Data.InputText)
	}
	if goal == "" {
		goal = strings.TrimSpace(wfTask.Title)
	}
	msgs := buildWorkflowSummaryPrompt(goal, nodes, bound)
	return client.Chat(ctx, msgs, llm.DefaultOptions())
}

// buildWorkflowSummaryPrompt 组装汇总 prompt。纯函数，便于单测。
//
// 两分支：
//   - 全部完成：让模型直接给最终产物与结论，不要罗列中间过程；
//   - 有失败/停止：让模型指出失败位置、原因与对最终结果的影响。
func buildWorkflowSummaryPrompt(
	goal string, nodes []model.WorkflowNodeData, bound model.TaskStatus,
) []llm.Message {
	var b strings.Builder
	for _, n := range nodes {
		name := strings.TrimSpace(n.Name)
		if name == "" {
			name = n.NodeID
		}
		fmt.Fprintf(&b, "- 【%s】（%s）\n", name, statusText(n.Status))
		if n.Output != nil {
			if s := strings.TrimSpace(n.Output.ReplyText); s != "" {
				fmt.Fprintf(&b, "  输出：%s\n", truncateRunes(s, 2000))
			}
		}
	}

	var instruction string
	if bound == model.TaskStatusCompleted {
		instruction = "所有步骤均已成功完成。请直接给出最终的产出物与核心结论，简洁明了，不要再罗列中间过程。"
	} else {
		instruction = "有步骤失败或未达到预期。请明确指出哪些步骤失败、失败原因是什么、以及对最终结果的影响。"
	}

	system := "你是工作流执行结果的汇总助手，负责把多个步骤的执行结果总结成给用户看的一段结论。" +
		"只输出结论正文，不要输出任何文件下载链接或文件路径（下载入口由系统另行提供）。"
	user := fmt.Sprintf("工作流目标：%s\n\n各步骤执行结果：\n%s\n%s\n请用中文直接输出总结正文，不要加「以下是总结」之类的开场白。",
		goal, b.String(), instruction)
	return []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user},
	}
}
