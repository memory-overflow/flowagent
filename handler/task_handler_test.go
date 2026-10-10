package handler

import (
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// TestResetWorkflowNodesForRetry 重试时已完成节点保留、其余节点复位重跑。
func TestResetWorkflowNodesForRetry(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusCompleted, TaskID: "sub-1"},
		{NodeID: "n2", Status: model.TaskStatusFailed, TaskID: "sub-2", DependsOn: []string{"n1"}},
		{NodeID: "n3", Status: model.TaskStatusRunning, TaskID: "sub-3", DependsOn: []string{"n2"}},
	}}

	progress := resetWorkflowNodesForRetry(dag)
	if progress < 33.33 || progress > 33.34 {
		t.Errorf("progress = %v, want ~33.33", progress)
	}
	if dag.Nodes[0].Status != model.TaskStatusCompleted || dag.Nodes[0].TaskID != "sub-1" {
		t.Errorf("completed node must be kept as-is, got %+v", dag.Nodes[0])
	}
	for _, i := range []int{1, 2} {
		if dag.Nodes[i].Status != model.TaskStatusQueued || dag.Nodes[i].TaskID != "" {
			t.Errorf("node %d must be reset to queued and detached, got %+v", i, dag.Nodes[i])
		}
	}
}

// TestResetWorkflowNodesForRetry_SkipsArchived 已归档的失败分支不参与重试。
//
// 把它们复活等于把用户上一轮明确放弃的分支再跑一遍，而且会污染进度分母 ——
// 分母含归档节点时进度永远到不了 100%，工作流看着永远没跑完。
func TestResetWorkflowNodesForRetry_SkipsArchived(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusCompleted},
		{NodeID: "n2", Status: model.TaskStatusFailed, Archived: true, DependsOn: []string{"n1"}},
		{NodeID: "n3", Status: model.TaskStatusAbandoned, Archived: true, DependsOn: []string{"n2"}},
		// 续接进来的新节点：失败，需要重跑
		{NodeID: "n4", Status: model.TaskStatusFailed, DependsOn: []string{"n1"}},
	}}

	progress := resetWorkflowNodesForRetry(dag)
	// 活跃节点只有 n1、n4 → 1/2
	if progress != 50 {
		t.Errorf("progress = %v, want 50", progress)
	}
	if dag.Nodes[1].Status != model.TaskStatusFailed || dag.Nodes[2].Status != model.TaskStatusAbandoned {
		t.Errorf("archived nodes must not be revived, got %+v / %+v", dag.Nodes[1], dag.Nodes[2])
	}
	if dag.Nodes[3].Status != model.TaskStatusQueued {
		t.Errorf("active failed node must be reset, got %+v", dag.Nodes[3])
	}
}

// TestResetWorkflowNodesForRetry_AllArchived 全部节点都已归档时进度按 0 处理，
// 不能出现除零（NaN）导致前端进度条异常。
func TestResetWorkflowNodesForRetry_AllArchived(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusFailed, Archived: true},
	}}
	if got := resetWorkflowNodesForRetry(dag); got != 0 {
		t.Errorf("progress = %v, want 0", got)
	}
}
