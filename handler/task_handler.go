package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/stream"
)

// ============================================================
// 终态任务详情缓存
// ============================================================

// 终态任务的 task_output / think_process 不再变化，而前端对工作流页做 5s 轮询
// 详情，每次重复读 DB 里 MB 级 task_output 并解析纯属浪费。只缓存终态任务；
// running 任务实时变化，不走缓存。
var (
	detailCacheMu sync.RWMutex
	detailCache   = map[string]taskDetailCacheEntry{}
)

type taskDetailCacheEntry struct {
	detail   map[string]interface{}
	cachedAt time.Time
}

const (
	detailCacheTTL        = 10 * time.Minute
	detailCacheMaxEntries = 1024
)

func getCachedTaskDetail(taskID string) (map[string]interface{}, bool) {
	detailCacheMu.RLock()
	e, ok := detailCache[taskID]
	detailCacheMu.RUnlock()
	if !ok || time.Since(e.cachedAt) > detailCacheTTL {
		return nil, false
	}
	return e.detail, true
}

func setCachedTaskDetail(taskID string, detail map[string]interface{}) {
	detailCacheMu.Lock()
	defer detailCacheMu.Unlock()
	if len(detailCache) >= detailCacheMaxEntries {
		for k, e := range detailCache {
			if time.Since(e.cachedAt) > detailCacheTTL {
				delete(detailCache, k)
			}
		}
	}
	detailCache[taskID] = taskDetailCacheEntry{detail: detail, cachedAt: time.Now()}
}

// ============================================================
// 请求结构体
// ============================================================

type GetTaskDetailRequest struct {
	TaskID string `json:"task_id"`
}

type StopTaskRequest struct {
	TaskID string `json:"task_id"`
}

// ============================================================
// Handler 函数
// ============================================================

// GetTaskDetail 获取任务详情
func GetTaskDetail(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*GetTaskDetailRequest)

	taskDAO := &dao.TaskDAO{}

	// 终态任务详情稳定，先查缓存：命中则直接返回，跳过 MB 级 task_output 的
	// 读取与解析。先用轻量查询拿 status（不读 longtext），避免「为省读取反而
	// 先读了完整 task_output」。
	if st, err := taskDAO.GetTaskStatusByID(ctx, r.TaskID); err == nil && st != nil && st.IsTerminal() {
		if cached, ok := getCachedTaskDetail(r.TaskID); ok {
			return cached, nil
		}
	}

	task, err := taskDAO.GetTaskByID(ctx, r.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "任务不存在"}
	}

	detail := buildTaskDetailMap(task)
	enrichThinkProcess(ctx, &dao.TaskTurnDAO{}, task, detail)
	// 对话任务：注入其绑定工作流的执行状态与结果（调试页与对话页口径一致）。
	if task.TaskType == model.TaskTypeConversation {
		if out, ok := detail["agent_output"].(model.AgentOutputData); ok && out.WorkflowTaskID != "" {
			if wfTask, err := taskDAO.GetTaskByID(ctx, out.WorkflowTaskID); err == nil && wfTask != nil {
				enrichBoundWorkflow(detail, map[string]*boundWorkflowSnapshot{
					wfTask.TaskID: {dag: decodeWorkflowOutput(wfTask), status: wfTask.Status},
				})
			}
		}
	}

	// 终态任务写入缓存，供后续轮询命中
	if task.IsTerminal() {
		setCachedTaskDetail(r.TaskID, detail)
	}
	return detail, nil
}

// GetTaskDetailBinder 绑定 GetTaskDetail 请求参数
func GetTaskDetailBinder() interface{} {
	return &GetTaskDetailRequest{}
}

// StopTask 停止任务
func StopTask(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*StopTaskRequest)

	taskDAO := &dao.TaskDAO{}
	task, err := taskDAO.GetTaskByID(ctx, r.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "任务不存在"}
	}

	// 只能停止非终态任务
	if task.IsTerminal() {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "任务已处于终态，无法停止"}
	}

	// 对话任务若已把本轮工作移交给工作流（规划判定 workflow 后当轮收口，
	// agent_output.workflow_task_id 非空），停止动作应转发到它绑定的工作流任务：
	// 对话任务自身已收口、无可停止，真正在跑的是工作流。只停对话任务会让工作流
	// 继续推进，下一轮 SyncWorkflowSourceTaskStatuses 又会把对话任务的绑定态
	// （DeriveBoundTaskStatus 跟随工作流节点）刷回 running，表现为「停止没效果」。
	if task.TaskType == model.TaskTypeConversation {
		if wfID := boundWorkflowTaskID(task); wfID != "" && wfID != r.TaskID {
			if wfTask, err := taskDAO.GetTaskByID(ctx, wfID); err != nil {
				return nil, err
			} else if wfTask != nil && !wfTask.IsTerminal() {
				// 递归停工作流：级联停其子任务 + 标 stopped + sync 来源对话任务为 stopped。
				return StopTask(ctx, &StopTaskRequest{TaskID: wfID})
			}
			// 绑定工作流不存在或已终态：落到普通对话任务停止（它本已收口，最多补标 stopped）。
		}
	}

	// 级联停止整棵子树里仍在排队/运行中的后代任务，避免它们被调度器继续捞起执行。
	//
	// 不限定任务类型：对话任务（single_agent 模式）也会挂 deep_search 子任务，
	// 工作流任务则多一层「节点子任务 → deep_search 子任务」的孙层。只停直接
	// 子任务会漏掉孙层，表现为「任务已停止，搜索还在跑」。
	if err := stopTaskDescendants(ctx, taskDAO, r.TaskID); err != nil {
		return nil, err
	}

	if err := taskDAO.UpdateTaskStatus(ctx, r.TaskID, model.TaskStatusStopped, nil, task.Progress); err != nil {
		return nil, err
	}

	// 停止的是工作流：同步把来源对话任务的状态绑定为已终止。
	//
	// 必须在 handler 侧做一次而不仅靠 workflow 执行器：执行器只在轮询中
	// 回填节点状态（节点子任务被级联停止后要等下一个 tick 才会写 DAG），
	// 而本方法已把工作流标为终态、调度器不再轮询它 —— 不在这里补一次，
	// 来源对话任务会永远停在「执行中」。
	if task.TaskType == model.TaskTypeWorkflow {
		var dag model.WorkflowOutputData
		if task.TaskOutput != "" {
			if err := json.Unmarshal([]byte(task.TaskOutput), &dag); err == nil {
				if err := taskDAO.SyncWorkflowSourceTaskStatuses(ctx, &dag, model.TaskStatusStopped); err != nil {
					log.Printf("[task-handler] sync source tasks of stopped workflow %s failed: %v",
						r.TaskID, err)
				}
			}
		}
	}

	stream.GetHub().Publish(stream.Event{
		Type:   stream.EventTypeTaskDone,
		TaskID: r.TaskID,
		Data:   map[string]interface{}{"status": int(model.TaskStatusStopped)},
	})
	return nil, nil
}

// stopTaskDescendants 终止一个任务下整棵子树里仍在排队/运行中的后代任务。
//
// 与 schedule/actuator 里的 WorkflowActuator.stopSubTasks 逻辑一致，但 handler
// 侧不能反向依赖 actuator 包（避免循环依赖），故此处用 TaskDAO 直接复制一份
// 最小编排：任务标记为 stopped（终态、task_temp 被清理）后不再被调度。
// 单个任务停止失败不阻断整体 —— 记录日志后继续处理其余任务。
//
// 必须递归：deep_search 的搜索子任务挂在「工作流节点子任务」之下（孙层），
// 只停直接子任务会让它们在工作流停止后继续空跑。
func stopTaskDescendants(ctx context.Context, taskDAO *dao.TaskDAO, rootTaskID string) error {
	pendings, err := taskDAO.ListActiveDescendants(ctx, rootTaskID)
	if err != nil {
		return fmt.Errorf("list descendants to stop: %w", err)
	}
	for _, st := range pendings {
		if err := taskDAO.UpdateTaskStatus(ctx, st.TaskID, model.TaskStatusStopped, nil, 0); err != nil {
			log.Printf("[task-handler] stop task %s failed: %v", st.TaskID, err)
		}
	}
	if len(pendings) > 0 {
		log.Printf("[task-handler] stopped %d descendant task(s) of %s", len(pendings), rootTaskID)
	}
	return nil
}

// StopTaskBinder 绑定 StopTask 请求参数
func StopTaskBinder() interface{} {
	return &StopTaskRequest{}
}

// boundWorkflowTaskID 返回对话任务在规划判定 workflow 后绑定的工作流任务 ID。
//
// 绑定关系落在 agent_output.workflow_task_id（delegateToWorkflow 收口时写入）。
// 非对话任务、task_output 为空或解析失败时返回空串。
func boundWorkflowTaskID(task *model.Task) string {
	if task == nil || task.TaskType != model.TaskTypeConversation || task.TaskOutput == "" {
		return ""
	}
	var out model.AgentOutputData
	if err := json.Unmarshal([]byte(task.TaskOutput), &out); err != nil {
		return ""
	}
	return out.WorkflowTaskID
}

// ============================================================
// RetryTask — 重试失败/终止的任务
// ============================================================

type RetryTaskRequest struct {
	TaskID string `json:"task_id"`
}

// RetryTask 重试一个失败/终止的任务，按任务类型分派：
//   - 工作流任务：保留已完成节点，重置失败节点及其未执行的下游节点；
//   - 对话任务：清空旧 turn，从头重新生成一次回复。
func RetryTask(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*RetryTaskRequest)
	if r.TaskID == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "task_id 必填"}
	}

	taskDAO := &dao.TaskDAO{}
	task, err := taskDAO.GetTaskByID(ctx, r.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "任务不存在"}
	}

	switch task.TaskType {
	case model.TaskTypeWorkflow:
		if task.Status != model.TaskStatusFailed &&
			task.Status != model.TaskStatusPartialFailed &&
			task.Status != model.TaskStatusStopped {
			return nil, &APIError{Code: http.StatusBadRequest, Msg: "工作流不在失败/终止状态，无需重试"}
		}
		// 通过状态校验后落到下方的 DAG 重置逻辑。

	case model.TaskTypeConversation:
		if task.Status != model.TaskStatusFailed &&
			task.Status != model.TaskStatusStopped {
			return nil, &APIError{Code: http.StatusBadRequest, Msg: "任务不在失败/终止状态，无需重试"}
		}
		// 已收口（stage=finished）的对话任务不允许重试：这是「规划已移交
		// 工作流」的任务，它的失败状态是绑定工作流节点得出的（见
		// DeriveBoundTaskStatus），不是它自身执行失败。按对话任务重试会让
		// 执行器再跑一遍单 Agent 循环、产出一段重复回复，且修不了工作流 ——
		// 正确入口是重试对应的工作流任务。
		if model.TaskStage(task.Stage) == model.StageFinished {
			return nil, &APIError{Code: http.StatusBadRequest,
				Msg: "该轮规划已移交给工作流执行，请到工作流面板重试对应的工作流"}
		}
		return retryConversationTask(ctx, taskDAO, task)

	default:
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "该任务类型不支持重试"}
	}

	// 重试期间工作流已终态，编排调度器不会碰它；但用户可能同时发新消息触发
	// 追加节点（appendWorkflowNodes 也写 DAG），故仍用 dag_version 乐观锁兜底，
	// 冲突时重读最新 DAG 重新重置，最多 maxRetryAttempts 次。
	const maxRetryAttempts = 3
	for attempt := 1; ; attempt++ {
		// 每次重读最新任务，拿到最新 DAG 与 dag_version
		cur, err := taskDAO.GetTaskByID(ctx, r.TaskID)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, &APIError{Code: http.StatusNotFound, Msg: "任务不存在"}
		}

		var dag model.WorkflowOutputData
		if cur.TaskOutput != "" {
			if err := json.Unmarshal([]byte(cur.TaskOutput), &dag); err != nil {
				return nil, &APIError{Code: http.StatusInternalServerError, Msg: "工作流 DAG 解析失败"}
			}
		}

		progress := resetWorkflowNodesForRetry(&dag)
		outJSON, err := json.Marshal(dag)
		if err != nil {
			return nil, err
		}

		err = taskDAO.RetryWorkflow(ctx, dao.RetryWorkflowParams{
			TaskID:        r.TaskID,
			Output:        string(outJSON),
			Progress:      progress,
			ExpectVersion: cur.DAGVersion,
		})
		if err == nil {
			return nil, nil
		}
		if !errors.Is(err, dao.ErrWorkflowVersionConflict) {
			return nil, err
		}
		if attempt >= maxRetryAttempts {
			return nil, &APIError{Code: http.StatusConflict, Msg: "工作流正在被并发修改，请稍后重试"}
		}
	}

}

// RetryTaskBinder 绑定 RetryTask 请求参数
func RetryTaskBinder() interface{} {
	return &RetryTaskRequest{}
}

// RetryWorkflowNodeRequest 单节点重试请求
type RetryWorkflowNodeRequest struct {
	TaskID string `json:"task_id"` // 工作流任务 ID
	NodeID string `json:"node_id"` // 要重试的节点（node_id 或子任务 task_id）
}

// RetryWorkflowNode 重试工作流里的单个失败节点及其下游，保留已完成的上游。
//
// 与 RetryTask（整工作流重试）不同：这里只重置用户指定的节点和它下游的
// 节点，其他分支（包括不相关的失败节点）保持原样，由用户逐个处理。
// 「从失败位置开始」的语义：已完成的上游节点不重跑。
func RetryWorkflowNode(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*RetryWorkflowNodeRequest)
	if r.TaskID == "" || r.NodeID == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "task_id 和 node_id 必填"}
	}

	taskDAO := &dao.TaskDAO{}
	task, err := taskDAO.GetTaskByID(ctx, r.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "任务不存在"}
	}
	if task.TaskType != model.TaskTypeWorkflow {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "只能重试工作流任务的节点"}
	}

	// 乐观锁重试（与 RetryTask 同口径，防并发追加节点）
	const maxRetryAttempts = 3
	for attempt := 1; ; attempt++ {
		cur, err := taskDAO.GetTaskByID(ctx, r.TaskID)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, &APIError{Code: http.StatusNotFound, Msg: "任务不存在"}
		}
		var dag model.WorkflowOutputData
		if cur.TaskOutput != "" {
			if err := json.Unmarshal([]byte(cur.TaskOutput), &dag); err != nil {
				return nil, &APIError{Code: http.StatusInternalServerError, Msg: "工作流 DAG 解析失败"}
			}
		}

		progress, err := resetWorkflowNodeForRetry(&dag, r.NodeID)
		if err != nil {
			return nil, &APIError{Code: http.StatusBadRequest, Msg: err.Error()}
		}
		outJSON, err := json.Marshal(dag)
		if err != nil {
			return nil, err
		}

		err = taskDAO.RetryWorkflow(ctx, dao.RetryWorkflowParams{
			TaskID:        r.TaskID,
			Output:        string(outJSON),
			Progress:      progress,
			ExpectVersion: cur.DAGVersion,
		})
		if err == nil {
			return nil, nil
		}
		if !errors.Is(err, dao.ErrWorkflowVersionConflict) {
			return nil, err
		}
		if attempt >= maxRetryAttempts {
			return nil, &APIError{Code: http.StatusConflict, Msg: "工作流正在被并发修改，请稍后重试"}
		}
	}
}

// RetryWorkflowNodeBinder 绑定 RetryWorkflowNode 请求参数
func RetryWorkflowNodeBinder() interface{} {
	return &RetryWorkflowNodeRequest{}
}

// retryConversationTask 重试一个对话任务：从失败发生的 stage 断点续跑。
//
// 不再清空旧 turn、也不再重置回规划期 —— 失败时 ActionFailed 已把 stage 保留在
// 失败发生的阶段，据此还原续跑起点（见 resumeStageForFailedTask）。旧 turn 由
// 历史重建自然过滤掉非 success 的失败 turn，因此续跑不会把失败那一轮污染进上下文。
func retryConversationTask(ctx *gin.Context, taskDAO *dao.TaskDAO, task *model.Task) (interface{}, error) {
	resumeStage := resumeStageForFailedTask(task.Stage)
	if err := taskDAO.RetryConversationTask(ctx, task.TaskID, resumeStage); err != nil {
		return nil, err
	}
	return nil, nil
}

// resumeStageForFailedTask 根据任务失败时停留的 stage，计算重试应进入的 wait 阶段。
//
// 失败发生在 doing 阶段（ActionFailed 把 stage 保留在失败发生的阶段）：
//   - tool_call_doing（工具执行阶段整体失败）→ tool_call_wait：重新执行上一轮
//     LLM turn 请求的工具调用；
//   - functional_call_doing（LLM 调用失败）→ functional_call_wait：让 LLM 基于
//     已有历史重新推理；
//   - 其他（含 stage=0 的历史数据 / 异常值）→ functional_call_wait，保守从头推理。
func resumeStageForFailedTask(failedStage int) model.TaskStage {
	switch model.TaskStage(failedStage) {
	case model.StageToolCallDoing:
		return model.StageToolCallWait
	default:
		return model.StageFunctionalCallWait
	}
}

// resetWorkflowNodesForRetry 重试前重置 DAG 节点状态，返回重算后的进度。
//
// 规则：
//   - 已完成（completed）节点保留结果，不重跑；
//   - 其余节点（失败/排队/执行中）重置为排队，并清空已派发的子任务引用
//     （TaskID）与输出（Output），让编排调度器为它们重新创建子任务；
//   - 已归档节点（失败分支，见 pruneAbandonedBranch）一律跳过：它们是上一轮
//     规划明确废弃的部分，重试把它们复活等于把用户已经放弃的分支再跑一遍，
//     而且会污染进度分母（进度永远到不了 100%）。
func resetWorkflowNodesForRetry(dag *model.WorkflowOutputData) float64 {
	total := dag.ActiveNodeCount()
	if total == 0 {
		return 0
	}
	completed := 0
	for i := range dag.Nodes {
		n := &dag.Nodes[i]
		if n.Archived {
			continue
		}
		if n.Status == model.TaskStatusCompleted {
			completed++
			continue
		}
		n.Status = model.TaskStatusQueued
		n.TaskID = ""
		n.Output = nil
	}
	return float64(completed) / float64(total) * 100.0
}

// resetWorkflowNodeForRetry 重置指定节点及其下游（依赖它的节点，含传递依赖），
// 保留其余节点。返回重算后的进度。
//
// 与 resetWorkflowNodesForRetry 的区别：后者是「整工作流重试」——保留全部
// 已完成节点、重置所有失败/未完成节点；本函数是「单节点重试」——只重置用户
// 指定的那个节点和它的下游，其他分支（包括不相关的失败节点）保持原样，
// 由用户逐个处理。「从失败位置开始」的语义由此实现：已完成的上游不重跑。
func resetWorkflowNodeForRetry(dag *model.WorkflowOutputData, nodeID string) (float64, error) {
	// 1. 定位目标节点的 NodeID（node_id 或子任务 task_id 均可）
	targetNodeID := ""
	for _, n := range dag.Nodes {
		if n.NodeID == nodeID || n.TaskID == nodeID {
			targetNodeID = n.NodeID
			break
		}
	}
	if targetNodeID == "" {
		return 0, fmt.Errorf("节点不存在: %s", nodeID)
	}

	// 2. 计算下游集合（直接/间接依赖目标节点的节点），迭代到收敛
	downstream := map[string]bool{targetNodeID: true}
	for changed := true; changed; {
		changed = false
		for _, n := range dag.Nodes {
			if downstream[n.NodeID] {
				continue
			}
			for _, dep := range n.DependsOn {
				if downstream[dep] {
					downstream[n.NodeID] = true
					changed = true
					break
				}
			}
		}
	}

	// 3. 重置目标节点 + 下游，保留其余（含已完成上游与其他分支）
	total := dag.ActiveNodeCount()
	if total == 0 {
		return 0, nil
	}
	completed := 0
	for i := range dag.Nodes {
		n := &dag.Nodes[i]
		if n.Archived {
			continue
		}
		if downstream[n.NodeID] {
			n.Status = model.TaskStatusQueued
			n.TaskID = ""
			n.Output = nil
			continue
		}
		if n.Status == model.TaskStatusCompleted {
			completed++
		}
	}
	return float64(completed) / float64(total) * 100.0, nil
}

// ============================================================
// ListTaskTurns — 调试页面用：拉取任务所有 turn
// ============================================================

// ListTaskTurnsRequest 请求参数
type ListTaskTurnsRequest struct {
	TaskID string `json:"task_id"`
}

// ListTaskTurnsBinder 绑定
func ListTaskTurnsBinder() interface{} {
	return &ListTaskTurnsRequest{}
}

// GetTurnDetailRequest 单条 turn 详情请求
type GetTurnDetailRequest struct {
	TaskID  string `json:"task_id"`
	TurnSeq int32  `json:"turn_seq"`
}

// GetTurnDetail 拉取单条 turn 的完整内容（含 input/output 大字段）。
// 调试页默认只加载 turn 元信息列表，点开某条 turn 时再调本接口拉完整 payload，
// 避免列表一次传输 MB 级历史（每条 turn 的 input 都含完整 system prompt + 历史）。
func GetTurnDetail(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*GetTurnDetailRequest)
	if r.TaskID == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "task_id 必填"}
	}
	turnDAO := &dao.TaskTurnDAO{}
	turn, err := turnDAO.GetTurnBySeq(ctx, r.TaskID, r.TurnSeq)
	if err != nil {
		return nil, err
	}
	if turn == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "turn 不存在"}
	}
	return buildTurnDebugView(turn, "", true), nil
}

// GetTurnDetailBinder 绑定 GetTurnDetail 请求参数
func GetTurnDetailBinder() interface{} {
	return &GetTurnDetailRequest{}
}

// TurnDebugView 单条 turn 的调试视图（前端友好格式）
//
// 相比 DB 里的 TaskTurn，这里把 Input/Output 的 JSON 提前解出来变成 object，
// 前端拿到就能直接展示，不用再自己 JSON.parse；同时把 turn_type / status / stage
// 的可读名称也一并给出，减少前端映射代码。
//
// turn_type 与 phase 是两个正交维度，都要返回：
// 前者说明「在调什么」（LLM / 工具），后者说明「处于哪个语义阶段」（规划 / 执行）。
// 规划期调工具做调研就是 (phase=planning, turn_type=tool_call)。
//
// SubTaskID 仅 deep_search 等会派发子任务的工具调用填写，指向由本 turn 派生的
// 子任务 task_id；调试页据此在 turn 行直接挂「打开子任务调试」入口。
type TurnDebugView struct {
	ID           int64  `json:"id"`
	TaskID       string `json:"task_id"`
	TurnSeq      int32  `json:"turn_seq"`
	ChatID       string `json:"chat_id"`
	ParentTaskID string `json:"parent_task_id,omitempty"`
	TurnType     int    `json:"turn_type"`
	TurnTypeName string `json:"turn_type_name"`
	Phase        int    `json:"phase"`
	PhaseName    string `json:"phase_name"`
	Stage        int    `json:"stage"`
	StageName    string `json:"stage_name"`
	Status       int    `json:"status"`
	StatusName   string `json:"status_name"`
	Round        int32  `json:"round"`
	Model        string `json:"model,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
	ServerName   string `json:"server_name,omitempty"`
	TotalTokens  int    `json:"total_tokens"`
	// PromptTokens / CompletionTokens 输入 / 输出拆分。
	//
	// 输入侧才是上下文水位的度量 —— 系统提示、历史消息、工具 schema 全算在里面；
	// 输出侧只反映回答长度。两列分开落库后，前端可直接判断
	// 「这一轮是上下文撑大了，还是回答写长了」。
	PromptTokens     int         `json:"prompt_tokens"`
	CompletionTokens int         `json:"completion_tokens"`
	Input            interface{} `json:"input,omitempty"`
	InputRaw         string      `json:"input_raw,omitempty"`
	Output           interface{} `json:"output,omitempty"`
	OutputRaw        string      `json:"output_raw,omitempty"`
	ErrorMsg         string      `json:"error_msg,omitempty"`
	StartTs          int64       `json:"start_ts"`
	EndTs            int64       `json:"end_ts"`
	TimeCostMs       int64       `json:"time_cost_ms"`
	// SubTaskID 由该 turn 派生的子任务 task_id（如 deep_search 子任务）。
	// 调试页把它放在 turn 行尾部作为子任务调试入口；老数据无映射时为空。
	SubTaskID string `json:"sub_task_id,omitempty"`
}

// ListTaskTurns 返回任务的完整 turn 列表（用于调试页面）
func ListTaskTurns(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ListTaskTurnsRequest)
	if r.TaskID == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "task_id 必填"}
	}
	return buildTaskTurnsSnapshot(ctx, r.TaskID)
}

// buildTaskTurnsSnapshot 拉取任务摘要 + 全量 turn 调试视图。
// 供轮询接口 ListTaskTurns 与 SSE 连接的 snapshot 事件共用，保证两处口径一致。
func buildTaskTurnsSnapshot(ctx context.Context, taskID string) (gin.H, error) {
	taskDAO := &dao.TaskDAO{}
	task, err := taskDAO.GetTaskByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "任务不存在"}
	}

	turnDAO := &dao.TaskTurnDAO{}
	// 列表视图用轻量查询：不加载 input/output 大字段，避免一次拉 MB 级历史。
	// 完整 payload 由 GetTurnDetail 按需拉取。
	turns, err := turnDAO.ListTurnsByTaskIDLight(ctx, taskID)
	if err != nil {
		return nil, err
	}

	// 拉子任务列表构建 parent_turn_id → sub_task_id 索引，
	// 调试页就能把子任务入口直接挂在对应的 turn 行上，而不是堆在顶部列表里。
	// 子任务拉取失败不阻断主流程（顶层字段缺失时前端不显示入口即可）。
	//
	// 一个子任务可能对应多个 turn：同一检索目标被并发/重放复用时，每个 turn
	// 都登记进 parent_turn_ids，这里全部建索引，避免只有首个 turn 有调试入口。
	//
	// 兼容旧数据：改动前跑完的子任务没写 parent_turn_id，无法挂到 turn 行，
	// 退回顶层 unmapped_sub_tasks 列表兜底展示，避免入口完全消失。
	subTasks, _ := taskDAO.ListSubTasksByParent(ctx, taskID)
	turnToSubTask := make(map[int64]string, len(subTasks))
	unmappedSubTasks := make([]gin.H, 0)
	for _, st := range subTasks {
		if st == nil {
			continue
		}
		parentTurnIDs := st.TaskInput.Data.ParentTurnIDList()
		if len(parentTurnIDs) == 0 {
			unmappedSubTasks = append(unmappedSubTasks, buildTaskDebugSummary(ctx, taskDAO, turnDAO, st))
			continue
		}
		for _, turnID := range parentTurnIDs {
			turnToSubTask[turnID] = st.TaskID
		}
	}

	views := make([]*TurnDebugView, 0, len(turns))
	for _, t := range turns {
		views = append(views, buildTurnDebugView(t, turnToSubTask[t.ID], false))
	}

	// 任务摘要：抽到 buildTaskDebugSummary 里供 ListSubTasks 复用
	summary := buildTaskDebugSummary(ctx, taskDAO, turnDAO, task)
	return gin.H{
		"task":               summary,
		"turns":              views,
		"unmapped_sub_tasks": unmappedSubTasks,
	}, nil
}

// buildTaskDebugSummary 拼装单个任务的调试摘要（顶层 task 块）。
//
// 抽出来给 ListTaskTurns 与 ListSubTasks 共用，保证「父任务摘要」与
// 「父任务页里子任务卡片」展示字段一致，避免两处口径分裂。
func buildTaskDebugSummary(
	ctx context.Context, taskDAO *dao.TaskDAO, turnDAO *dao.TaskTurnDAO, task *model.Task,
) gin.H {
	// token 以「任务树」为单位统计：本任务自身 turn + 所有后代子任务。
	//
	// 只看自身会严重低估真实开销 —— deep_search 这类工具会派生独立子任务
	// 去做多轮「搜索 → 抓正文」，消耗全记在子任务名下，父任务这边一无所知。
	// 而 deep_search 恰恰是单次任务里最容易烧 token 的地方。
	breakdown, bErr := dao.SumTaskTokensTree(ctx, taskDAO, turnDAO, task.TaskID)
	if bErr != nil || breakdown == nil {
		// 降级：树统计失败时退回只算自身，保证顶层字段不缺失
		breakdown = &dao.TaskTokenBreakdown{}
		if own, e := turnDAO.SumTokensByTaskID(ctx, task.TaskID); e == nil {
			breakdown.OwnTokens, breakdown.TotalTokens = own, own
		}
	}

	var inputText string
	// parentTurnIDs 本子任务关联的全部父 turn id。空数组而非 nil，
	// 保证 JSON 里是 [] 而不是 null，前端遍历/取长度不踩空。
	parentTurnIDs := make([]int64, 0, 1)
	if task.TaskInput.Data != nil {
		inputText = task.TaskInput.Data.InputText
		parentTurnIDs = append(parentTurnIDs, task.TaskInput.Data.ParentTurnIDList()...)
	}

	return gin.H{
		"task_id":        task.TaskID,
		"chat_id":        task.ChatID,
		"parent_task_id": task.ParentTaskID,
		"task_type":      int(task.TaskType),
		"task_type_name": task.TaskType.String(),
		"task_status":    int(task.Status),
		"status_name":    taskStatusName(task.Status),
		"stage":          task.Stage,
		"stage_name":     model.TaskStage(task.Stage).String(),
		// 任务当前的语义阶段。与 stage 是两个维度：
		// stage 说明「调度到哪一步」，phase 说明「在规划还是在执行」。
		"phase":      task.Phase,
		"phase_name": model.TaskPhase(task.Phase).String(),
		"turn_count": task.TurnCount,
		"progress":   task.Progress,
		"create_ts":  task.CreatedAt.UnixMilli(),
		"start_ts":   timeOrZero(task.StartTime),
		"end_ts":     timeOrZero(task.EndTime),
		// total_tokens 是含子任务的总量（自身 + 全部后代）。
		// 拆开给前端是为了让人一眼看出「钱烧在哪」：
		// deep_search 子任务通常占大头，混在总数里完全看不出来。
		"total_tokens":       breakdown.TotalTokens,
		"own_tokens":         breakdown.OwnTokens,
		"sub_task_tokens":    breakdown.SubTaskTokens,
		"deep_search_tokens": breakdown.DeepSearchTokens,
		// 输入 / 输出拆分。prompt 是上下文水位的度量对象，
		// 后续做遮蔽 / 摘要都盯着它。
		"prompt_tokens":     breakdown.PromptTokens,
		"completion_tokens": breakdown.CompletionTokens,
		"input_text":        inputText,
		"failed_reason":     task.FailedReason.Data,
		"output_text":       task.TaskOutput,
		// 子任务专属标记：deep_search 子任务用 SubTaskKind=SubTaskKindDeepSearch
		// 区分于普通 workflow 节点子任务，前端可据此染色或加图标。
		// SubTaskKind 写在 TaskInputData 里，Task 本体无该字段。
		"sub_task_kind": func() string {
			if task.TaskInput.Data != nil {
				return task.TaskInput.Data.SubTaskKind
			}
			return ""
		}(),
		// parent_turn_id：首个派发本子任务的父任务 turn id（task_turn.id）。
		// parent_turn_ids：本子任务关联的全部 turn —— 复用同一子任务时会有多个。
		// 调试页 /tasks/turns 用它们构建 turn → sub_task 映射，把子任务入口
		// 直接挂在对应的 turn 行上。老数据没有这些字段（退化为 0 / []）。
		"parent_turn_id": func() int64 {
			if len(parentTurnIDs) > 0 {
				return parentTurnIDs[0]
			}
			return 0
		}(),
		"parent_turn_ids": parentTurnIDs,
	}
}

// ============================================================
// ListSubTasks — 调试页面用：拉取父任务下的所有子任务摘要
// ============================================================
//
// 与 /tasks/turns 的差别：后者按 task_id 拉「本任务的 turn」；本端点按
// parent_task_id 拉「本任务派生的子任务列表」，让父任务调试页能列出
// 所有 deep_search / workflow 节点子任务并跳转到各自调试页。
//
// 返回字段与 /tasks/turns 里的 task 块完全一致（复用 buildTaskDebugSummary），
// 方便前端用同一组件渲染。

// ListSubTasksRequest 请求参数
type ListSubTasksRequest struct {
	ParentTaskID string `json:"parent_task_id"`
}

// ListSubTasksBinder 绑定
func ListSubTasksBinder() interface{} {
	return &ListSubTasksRequest{}
}

// ListSubTasks 返回父任务下的子任务摘要列表
func ListSubTasks(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ListSubTasksRequest)
	if r.ParentTaskID == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "parent_task_id 必填"}
	}

	taskDAO := &dao.TaskDAO{}
	turnDAO := &dao.TaskTurnDAO{}

	// 确认父任务存在 —— 不存在直接 404，避免前端拿空 list 误判「无子任务」。
	parent, err := taskDAO.GetTaskByID(ctx, r.ParentTaskID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "父任务不存在"}
	}

	subTasks, err := taskDAO.ListSubTasksByParent(ctx, r.ParentTaskID)
	if err != nil {
		return nil, err
	}

	items := make([]gin.H, 0, len(subTasks))
	for _, st := range subTasks {
		if st == nil {
			continue
		}
		items = append(items, buildTaskDebugSummary(ctx, taskDAO, turnDAO, st))
	}
	return gin.H{
		"parent_task_id": r.ParentTaskID,
		"list":           items,
		"total":          len(items),
	}, nil
}

func buildTurnDebugView(t *model.TaskTurn, subTaskID string, includePayload bool) *TurnDebugView {
	v := &TurnDebugView{
		ID:               t.ID,
		TaskID:           t.TaskID,
		TurnSeq:          t.TurnSeq,
		ChatID:           t.ChatID,
		ParentTaskID:     t.ParentTaskID,
		TurnType:         int(t.TurnType),
		TurnTypeName:     t.TurnType.String(),
		Phase:            t.Phase,
		PhaseName:        t.TurnPhase().String(),
		Stage:            t.Stage,
		StageName:        model.TaskStage(t.Stage).String(),
		Status:           int(t.Status),
		StatusName:       t.Status.String(),
		Round:            t.Round,
		Model:            t.Model,
		ToolName:         t.ToolName,
		ServerName:       t.ServerName,
		TotalTokens:      t.TotalTokens,
		PromptTokens:     t.PromptTokens,
		CompletionTokens: t.CompletionTokens,
		ErrorMsg:         t.ErrorMsg,
		StartTs:          t.StartTime.UnixMilli(),
		TimeCostMs:       t.TimeCostMs,
		SubTaskID:        subTaskID,
	}
	if t.EndTime != nil {
		v.EndTs = t.EndTime.UnixMilli()
	}
	// 列表视图不加载 payload（input/output 大字段），只返回元信息；
	// 完整 payload 由 GetTurnDetail 按需拉取，避免调试页一次传输 MB 级历史。
	if !includePayload {
		return v
	}
	v.InputRaw = t.Input
	v.OutputRaw = t.Output
	// 尝试把 JSON 解开，方便前端直接渲染
	if t.Input != "" {
		var obj interface{}
		if err := json.Unmarshal([]byte(t.Input), &obj); err == nil {
			v.Input = obj
		}
	}
	if t.Output != "" {
		var obj interface{}
		if err := json.Unmarshal([]byte(t.Output), &obj); err == nil {
			v.Output = obj
		}
	}

	// 存量兜底：prompt / completion 列上线前的记录，两列都是 0，
	// 但值其实一直写在 output 快照 JSON 里。这里回读补上，
	// 否则老任务在调试页会显示成「输入 0 / 输出 0」，看上去像没消耗。
	if v.PromptTokens == 0 && v.CompletionTokens == 0 {
		if m, ok := v.Output.(map[string]interface{}); ok {
			v.PromptTokens = jsonIntValue(m["prompt_tokens"])
			v.CompletionTokens = jsonIntValue(m["completion_tokens"])
		}
	}
	return v
}

// jsonIntValue 从 JSON 反序列化后的值里取整数。
//
// encoding/json 把数字统一解成 float64，这里做类型兜底；
// 取不到时返回 0（表示未知），不报错 —— 缺一个展示字段不该让整行 turn 挂掉。
func jsonIntValue(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// taskStatusName 返回任务状态的可读名称
func taskStatusName(s model.TaskStatus) string {
	switch s {
	case model.TaskStatusQueued:
		return "queued"
	case model.TaskStatusRunning:
		return "running"
	case model.TaskStatusCompleted:
		return "completed"
	case model.TaskStatusFailed:
		return "failed"
	case model.TaskStatusStopped:
		return "stopped"
	case model.TaskStatusPartialFailed:
		return "partial_failed"
	default:
		return "unknown"
	}
}

// timeOrZero 返回 *time.Time 的毫秒时间戳；nil 或零值返回 0
func timeOrZero(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
