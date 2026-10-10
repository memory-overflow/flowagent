package model

import (
	"strings"
	"testing"
)

// TestBuildBoundWorkflowView_Empty 无绑定节点（旧数据无 source_task_id）返回零值。
func TestBuildBoundWorkflowView_Empty(t *testing.T) {
	v := BuildBoundWorkflowView(nil, TaskStatusRunning)
	if v.ExecutionStep != nil {
		t.Fatalf("empty nodes should yield no execution step, got %+v", v.ExecutionStep)
	}
}

// TestBuildBoundWorkflowView_Running 执行中：追加 running 的 think 条目。
func TestBuildBoundWorkflowView_Running(t *testing.T) {
	nodes := []WorkflowNodeData{
		{NodeID: "n1", Name: "检索", Status: TaskStatusCompleted},
		{NodeID: "n2", Name: "分析", Status: TaskStatusCompleted},
		{NodeID: "n3", Name: "汇总", Status: TaskStatusRunning},
	}
	v := BuildBoundWorkflowView(nodes, TaskStatusRunning)
	if v.ExecutionStep == nil {
		t.Fatal("running should yield an execution think step")
	}
	if !v.ExecutionStep.Running {
		t.Error("running step should have Running=true")
	}
	if !strings.Contains(v.ExecutionStep.Summary, "2/3") {
		t.Errorf("summary should carry progress 2/3, got %q", v.ExecutionStep.Summary)
	}
}

// TestBuildBoundWorkflowView_Completed 全部完成：非 running 的完成条目。
func TestBuildBoundWorkflowView_Completed(t *testing.T) {
	nodes := []WorkflowNodeData{
		{NodeID: "n1", Name: "检索", Status: TaskStatusCompleted},
		{NodeID: "n2", Name: "汇总", Status: TaskStatusCompleted},
	}
	v := BuildBoundWorkflowView(nodes, TaskStatusCompleted)
	if v.ExecutionStep == nil || v.ExecutionStep.Running {
		t.Fatalf("completed step should be non-running, got %+v", v.ExecutionStep)
	}
	if !strings.Contains(v.ExecutionStep.Summary, "2/2") {
		t.Errorf("summary should say 2/2, got %q", v.ExecutionStep.Summary)
	}
}

// TestBuildBoundWorkflowView_Failed 有失败节点：失败条目。
func TestBuildBoundWorkflowView_Failed(t *testing.T) {
	nodes := []WorkflowNodeData{
		{NodeID: "n1", Name: "检索", Status: TaskStatusCompleted},
		{NodeID: "n2", Name: "分析", Status: TaskStatusFailed},
	}
	v := BuildBoundWorkflowView(nodes, TaskStatusFailed)
	if v.ExecutionStep == nil || !strings.Contains(v.ExecutionStep.Summary, "失败") {
		t.Fatalf("failed step should mark failure, got %+v", v.ExecutionStep)
	}
}

// TestBuildBoundWorkflowView_Stopped 用户停止：终止条目。
func TestBuildBoundWorkflowView_Stopped(t *testing.T) {
	nodes := []WorkflowNodeData{
		{NodeID: "n1", Name: "检索", Status: TaskStatusCompleted},
		{NodeID: "n2", Name: "分析", Status: TaskStatusStopped},
	}
	v := BuildBoundWorkflowView(nodes, TaskStatusStopped)
	if v.ExecutionStep == nil || !strings.Contains(v.ExecutionStep.Summary, "终止") {
		t.Fatalf("stopped step should say 终止, got %+v", v.ExecutionStep)
	}
}
