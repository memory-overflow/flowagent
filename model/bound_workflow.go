// Package model — 绑定工作流的展示增强
//
// 对话任务规划判定 workflow 后当轮收口，它的思考过程（think_layers）停在
// 「规划完成」那一刻，但真正的活由它规划出的节点在工作流里继续执行。
// 本文件生成一段「工作流执行状态」的 think 条目，让用户在规划任务的
// 思考过程里看到工作流正在执行 / 已完成 / 失败 / 终止。
//
// 工作流的「执行结果总结」不在这里拼 —— 那需要模型理解各节点产出并做
// 语义汇总，机械拼接只会罗列中间过程。它由 workflow_actuator 在工作流收口
// 时调用 LLM 生成，落库到 AgentOutputData.WorkflowSummary。
package model

import "fmt"

// BoundWorkflowView 绑定工作流在对话任务思考过程中的展示增强。
type BoundWorkflowView struct {
	// ExecutionStep 追加到 think_layers.execution 的条目；nil 表示无需追加。
	ExecutionStep *ThinkStep
}

// BuildBoundWorkflowView 由绑定节点状态 + 工作流整体状态，生成 think 状态条目。
//
// nodes 是该对话任务规划出的节点（SourceTaskID == 该任务 ID，由调用方过滤）。
// wfStatus 是工作流任务当前的整体状态，用于与 DeriveBoundTaskStatus 保持
// 「执行中 / 完成 / 失败 / 停止」的判定口径一致。
//
// nodes 为空时返回零值（无增强）—— 旧数据没有 source_task_id，不该凭空造状态。
func BuildBoundWorkflowView(nodes []WorkflowNodeData, wfStatus TaskStatus) BoundWorkflowView {
	if len(nodes) == 0 {
		return BoundWorkflowView{}
	}

	total := len(nodes)
	completed := 0
	failed := 0
	stopped := 0
	for i := range nodes {
		switch nodes[i].Status {
		case TaskStatusCompleted:
			completed++
		case TaskStatusFailed, TaskStatusPartialFailed, TaskStatusAbandoned:
			failed++
		case TaskStatusStopped:
			stopped++
		}
	}

	bound, _ := DeriveBoundTaskStatus(nodes, wfStatus)

	switch bound {
	case TaskStatusRunning, TaskStatusQueued:
		inProgress := total - completed - failed - stopped
		return BoundWorkflowView{
			ExecutionStep: &ThinkStep{
				Summary: fmt.Sprintf("工作流执行中：已完成 %d/%d 个节点（%d 个进行中）",
					completed, total, inProgress),
				Running: true,
			},
		}
	case TaskStatusCompleted:
		return BoundWorkflowView{
			ExecutionStep: &ThinkStep{
				Summary: fmt.Sprintf("工作流执行完成：%d/%d 个节点全部完成", completed, total),
			},
		}
	case TaskStatusFailed:
		return BoundWorkflowView{
			ExecutionStep: &ThinkStep{
				Summary: fmt.Sprintf("工作流执行失败：%d 个节点失败", failed),
			},
		}
	case TaskStatusStopped:
		return BoundWorkflowView{
			ExecutionStep: &ThinkStep{
				Summary: "工作流已终止",
			},
		}
	default:
		return BoundWorkflowView{}
	}
}
