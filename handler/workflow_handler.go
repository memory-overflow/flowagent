package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// 工作流模块（v1.4：一个对话可绑定多个工作流）
// ============================================================
//
// 产品背景：之前一个 chat 只绑定一条工作流，后面的任务即使和它无关也
// 必须强行规划进去，越用越别扭。现在改成：
//   - 一个 chat 可以有多条工作流，每条是一段相对独立的 DAG；
//   - 规划期由模型在候选清单里选「续接已有工作流」还是「新开工作流」，
//     用户也可以通过 chat 接口的 force_new_workflow 显式要求新开；
//   - 前端用列表切换查看，切换只是改 conversation.workflow_task_id
//     这个「当前激活」指针，不动 DAG 本身。

// WorkflowListItem 工作流列表项（切换器用，刻意比完整 DAG 轻）。
//
// 只带「够用户区分和选择」的信息：标题、序号、状态、进度、节点统计。
// 完整 DAG 走 conversations/detail 的 workflow 字段按需加载。
type WorkflowListItem struct {
	TaskID string `json:"task_id"`
	// Index 1-based 展示序号，前端可直接显示「工作流 2」。
	// 由创建时间升序推导，与列表顺序一致，稳定不随状态变化跳变。
	Index int `json:"index"`
	// Title 工作流短标题；模型没给出时回落到「工作流 N」
	Title      string  `json:"title"`
	TaskStatus int     `json:"task_status"`
	StatusName string  `json:"status_name"`
	Progress   float64 `json:"progress"`
	// Active 是否为对话当前激活的工作流（前端默认高亮这条）
	Active bool `json:"active"`
	// NodeTotal 节点总数（含已归档的失败分支 —— 它们仍在 DAG 里灰显展示）
	NodeTotal int `json:"node_total"`
	// NodeActive 未归档节点数，即真正参与调度和进度分母的那些
	NodeActive int `json:"node_active"`
	NodeDone   int `json:"node_done"`
	NodeFailed int `json:"node_failed"`
	// NodeArchived 已归档节点数（失败节点及其后代），前端可据此提示
	// 「该工作流有 N 个节点已废弃」
	NodeArchived int `json:"node_archived"`
	// CanAppend 是否可作为下一轮规划的续接对象。
	// 用户主动停止的工作流、或没有任何可复用节点的工作流为 false。
	CanAppend bool `json:"can_append"`
	// AppendReason 不可续接的原因，前端可直接展示（空表示可续接）
	AppendReason string `json:"append_reason,omitempty"`
	// ReusableCount 可复用节点数，续接时能直接吃到的历史成果
	ReusableCount int    `json:"reusable_count"`
	PlanSummary   string `json:"plan_summary,omitempty"`
	CreateTs      int64  `json:"create_ts"`
	UpdateTs      int64  `json:"update_ts"`
}

// --- 工作流列表 ---
type ListChatWorkflowsRequest struct {
	ChatID string `json:"chat_id"`
}

type ListChatWorkflowsResponse struct {
	ChatID string `json:"chat_id"`
	// ActiveTaskID 当前激活的工作流任务 ID，无工作流时为空
	ActiveTaskID string              `json:"active_workflow_task_id"`
	List         []*WorkflowListItem `json:"list"`
	Total        int                 `json:"total"`
}

// --- 切换激活的工作流 ---
type SetActiveWorkflowRequest struct {
	ChatID string `json:"chat_id"`
	// TaskID 要切换过去的工作流任务 ID。
	// 传空串表示「取消激活」（前端收起 DAG 面板），不删除任何数据。
	TaskID string `json:"task_id"`
}

type SetActiveWorkflowResponse struct {
	ChatID       string `json:"chat_id"`
	ActiveTaskID string `json:"active_workflow_task_id"`
}

// ListChatWorkflows 拉取一个对话下的全部工作流（按创建时间升序）。
//
// 与 conversations/detail 里的 workflow_list 是同一份数据：detail 顺带返回，
// 前端切换工作流后只需局部刷新时直接调本接口，不必重拉整个对话详情。
func ListChatWorkflows(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ListChatWorkflowsRequest)
	if r.ChatID == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "chat_id 不能为空"}
	}

	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "对话不存在"}
	}

	taskDAO := &dao.TaskDAO{}
	wfs, err := taskDAO.ListWorkflowTasksByChatID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	activeID := resolveActiveWorkflowID(wfs, conv.WorkflowTaskID)

	return &ListChatWorkflowsResponse{
		ChatID:       r.ChatID,
		ActiveTaskID: activeID,
		List:         buildWorkflowList(wfs, activeID),
		Total:        len(wfs),
	}, nil
}

// ListChatWorkflowsBinder 绑定 ListChatWorkflows 请求参数
func ListChatWorkflowsBinder() interface{} {
	return &ListChatWorkflowsRequest{}
}

// SetActiveWorkflow 切换对话「当前激活」的工作流。
//
// 纯指针操作：只改 conversation.workflow_task_id，不动 DAG、不改任务状态。
// 用户可以随时切回任意一条历史工作流查看，包括已经被别的任务续接过的。
func SetActiveWorkflow(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*SetActiveWorkflowRequest)
	if r.ChatID == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "chat_id 不能为空"}
	}

	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "对话不存在"}
	}

	// 传空串 = 取消激活，跳过归属校验直接落库
	if r.TaskID != "" {
		taskDAO := &dao.TaskDAO{}
		t, err := taskDAO.GetTaskByID(ctx, r.TaskID)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, &APIError{Code: http.StatusNotFound, Msg: "工作流任务不存在"}
		}
		// 归属校验：既防止把别家对话的工作流挂过来，也防止误传子任务 ID
		// （子任务 task_type=3，不是工作流任务）
		if t.ChatID != r.ChatID || t.TaskType != model.TaskTypeWorkflow {
			return nil, &APIError{Code: http.StatusBadRequest, Msg: "该工作流任务不属于此对话"}
		}
	}

	if err := convDAO.SetActiveWorkflowTask(ctx, r.ChatID, r.TaskID); err != nil {
		return nil, err
	}
	return &SetActiveWorkflowResponse{ChatID: r.ChatID, ActiveTaskID: r.TaskID}, nil
}

// SetActiveWorkflowBinder 绑定 SetActiveWorkflow 请求参数
func SetActiveWorkflowBinder() interface{} {
	return &SetActiveWorkflowRequest{}
}

// ============================================================
// 内部辅助
// ============================================================

// resolveActiveWorkflowID 决定对话当前该展示哪条工作流。
//
// 以 conversation.workflow_task_id 为准，但它在两种情况下会失效：
//   - 存量对话从未写过这一列（空串）；
//   - 那条任务已被清理 / 从未出现在本次查到的全集里。
//
// 这两种都回落到「最后创建的那条」——用户刚用完的工作流最可能是他想看的，
// 比返回空让前端整块 DAG 面板消失更符合直觉。
func resolveActiveWorkflowID(wfs []*model.Task, stored string) string {
	if len(wfs) == 0 {
		return ""
	}
	for _, t := range wfs {
		if t.TaskID == stored {
			return stored
		}
	}
	// ListWorkflowTasksByChatID 按 created_time 升序，末位即最新
	return wfs[len(wfs)-1].TaskID
}

// buildWorkflowList 把工作流任务拼成切换器要的轻量列表。
//
// 需要解析 task_output 才能算出节点统计和可追加性。这里不担心性能：
// 单个对话的工作流数量是个位数，而列表页只在这一次请求里算。
// 解析失败不拖垮整个列表 —— 退化成只带基础字段的一项。
func buildWorkflowList(wfs []*model.Task, activeID string) []*WorkflowListItem {
	list := make([]*WorkflowListItem, 0, len(wfs))
	for i, t := range wfs {
		item := &WorkflowListItem{
			TaskID:     t.TaskID,
			Index:      i + 1,
			Title:      t.Title,
			TaskStatus: int(t.Status),
			StatusName: taskStatusName(t.Status),
			Progress:   t.Progress,
			Active:     t.TaskID == activeID,
			CreateTs:   t.CreatedAt.UnixMilli(),
			UpdateTs:   t.UpdatedAt.UnixMilli(),
		}

		if dag := decodeWorkflowOutput(t); dag != nil {
			item.PlanSummary = dag.PlanSummary
			if item.Title == "" {
				item.Title = dag.Title
			}
			for _, n := range dag.Nodes {
				item.NodeTotal++
				if n.Archived {
					item.NodeArchived++
					continue
				}
				item.NodeActive++
				switch n.Status {
				case model.TaskStatusCompleted:
					item.NodeDone++
				case model.TaskStatusFailed, model.TaskStatusPartialFailed:
					item.NodeFailed++
				}
			}
			policy := dag.EvaluateAppendPolicy(t.Status)
			item.CanAppend = policy.Appendable
			item.AppendReason = policy.Reason
			item.ReusableCount = policy.ReusableCount
		}
		if item.Title == "" {
			item.Title = fmt.Sprintf("工作流 %d", item.Index)
		}
		list = append(list, item)
	}
	return list
}

// decodeWorkflowOutput 解析工作流任务的 DAG，失败返回 nil（调用方降级即可）。
func decodeWorkflowOutput(t *model.Task) *model.WorkflowOutputData {
	if t == nil || t.TaskOutput == "" {
		return nil
	}
	var dag model.WorkflowOutputData
	if err := json.Unmarshal([]byte(t.TaskOutput), &dag); err != nil {
		log.Printf("[handler] parse workflow task %s output failed: %v", t.TaskID, err)
		return nil
	}
	return &dag
}
