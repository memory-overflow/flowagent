package model

import "testing"

// TestDeriveBoundTaskStatus 验证「规划任务状态绑定工作流节点」的推导规则。
//
// 规则口径（产品需求，顺序即优先级）：
//  1. 绑定所有节点已完成 → 当前任务完成
//  2. 绑定所有节点有任何失败 → 当前任务失败
//  3. 绑定所有节点均在等待/执行中，但工作流整体失败 → 当前任务失败
//  4. 否则 → 执行中
func TestDeriveBoundTaskStatus(t *testing.T) {
	nodes := func(statuses ...TaskStatus) []WorkflowNodeData {
		out := make([]WorkflowNodeData, len(statuses))
		for i, s := range statuses {
			out[i] = WorkflowNodeData{NodeID: string(rune('a' + i)), Status: s}
		}
		return out
	}

	cases := []struct {
		name     string
		nodes    []WorkflowNodeData
		wfStatus TaskStatus
		want     TaskStatus
	}{
		// 规则 1：全部完成 → 完成
		{"全部完成", nodes(TaskStatusCompleted, TaskStatusCompleted), TaskStatusCompleted, TaskStatusCompleted},
		// 规则 1 的边界：全部完成但工作流因**别的轮次**的节点失败而整体失败
		// —— 绑定按本轮节点算，本轮全绿就是完成
		{"全部完成但工作流失败", nodes(TaskStatusCompleted, TaskStatusCompleted), TaskStatusFailed, TaskStatusCompleted},
		// 规则 2：任一绑定节点失败 → 失败（哪怕其余全绿、工作流已完成）
		{"任一失败", nodes(TaskStatusCompleted, TaskStatusFailed), TaskStatusCompleted, TaskStatusFailed},
		{"部分失败状态同为失败", nodes(TaskStatusPartialFailed), TaskStatusCompleted, TaskStatusFailed},
		// 废弃节点：永远不会再执行，按失败口径绑定
		{"废弃视为失败", nodes(TaskStatusCompleted, TaskStatusAbandoned), TaskStatusRunning, TaskStatusFailed},
		// 规则 3：节点未全部完成 + 工作流整体失败 → 失败
		{"未完成但工作流失败", nodes(TaskStatusRunning, TaskStatusQueued), TaskStatusFailed, TaskStatusFailed},
		{"部分完成但工作流失败", nodes(TaskStatusCompleted, TaskStatusQueued), TaskStatusPartialFailed, TaskStatusFailed},
		// 规则 4：其余 → 执行中
		{"执行中", nodes(TaskStatusRunning, TaskStatusCompleted), TaskStatusRunning, TaskStatusRunning},
		{"全部排队", nodes(TaskStatusQueued), TaskStatusQueued, TaskStatusRunning},
		{"未派发子任务", nodes(TaskStatusQueued, TaskStatusQueued), TaskStatusRunning, TaskStatusRunning},
		// 用户主动停止 → 终止（不是失败）
		{"节点被停止", nodes(TaskStatusCompleted, TaskStatusStopped), TaskStatusStopped, TaskStatusStopped},
		{"工作流被停止", nodes(TaskStatusRunning), TaskStatusStopped, TaskStatusStopped},
		// 已归档节点不豁免：归档只改调度口径，执行结果是既成事实
		{"归档的失败节点仍计失败", nodes(TaskStatusFailed /*archived 由调用方语义保证*/), TaskStatusCompleted, TaskStatusFailed},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, progress := DeriveBoundTaskStatus(c.nodes, c.wfStatus)
			if got != c.want {
				t.Fatalf("状态 = %d, want %d", got, c.want)
			}
			if progress < 0 || progress > 100 {
				t.Fatalf("进度越界: %v", progress)
			}
		})
	}
}

// TestDeriveBoundTaskStatusProgress 验证进度按完成比例计算。
func TestDeriveBoundTaskStatusProgress(t *testing.T) {
	nodes := []WorkflowNodeData{
		{NodeID: "n1", Status: TaskStatusCompleted},
		{NodeID: "n2", Status: TaskStatusCompleted},
		{NodeID: "n3", Status: TaskStatusRunning},
		{NodeID: "n4", Status: TaskStatusQueued},
	}
	_, progress := DeriveBoundTaskStatus(nodes, TaskStatusRunning)
	if progress != 50 {
		t.Fatalf("进度 = %.0f, want 50", progress)
	}

	// 空节点集（旧数据 / 无绑定）兜底为完成
	st, progress := DeriveBoundTaskStatus(nil, TaskStatusRunning)
	if st != TaskStatusCompleted || progress != 100 {
		t.Fatalf("空节点集 = (%d, %.0f), want (completed, 100)", st, progress)
	}
}
