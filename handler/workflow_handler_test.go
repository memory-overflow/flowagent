package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/memory-overflow/flowagent/model"
)

// ------------------------------------------------------------
// 测试辅助
// ------------------------------------------------------------

// wfTask 构造一条工作流任务（task_type=workflow），nodes 为 DAG 节点。
func wfTask(id string, status model.TaskStatus, title string, nodes ...model.WorkflowNodeData) *model.Task {
	dag := model.WorkflowOutputData{
		Nodes:       nodes,
		PlanSummary: "plan of " + id,
		Title:       title,
	}
	out, err := json.Marshal(dag)
	if err != nil {
		panic(err)
	}
	return &model.Task{
		TaskID:     id,
		ChatID:     "chat-1",
		TaskType:   model.TaskTypeWorkflow,
		Title:      title,
		Status:     status,
		Progress:   50,
		TaskOutput: string(out),
		CreatedAt:  time.UnixMilli(1000),
		UpdatedAt:  time.UnixMilli(2000),
	}
}

func node(id string, status model.TaskStatus) model.WorkflowNodeData {
	return model.WorkflowNodeData{NodeID: id, Status: status}
}

func archivedNode(id string, status model.TaskStatus) model.WorkflowNodeData {
	n := node(id, status)
	n.Archived = true
	return n
}

// ------------------------------------------------------------
// resolveActiveWorkflowID
// ------------------------------------------------------------

func TestResolveActiveWorkflowID_NoWorkflow(t *testing.T) {
	if got := resolveActiveWorkflowID(nil, "wf-1"); got != "" {
		t.Fatalf("没有任何工作流时应返回空串，got %q", got)
	}
}

func TestResolveActiveWorkflowID_PrefersStored(t *testing.T) {
	wfs := []*model.Task{
		wfTask("wf-1", model.TaskStatusCompleted, "A"),
		wfTask("wf-2", model.TaskStatusRunning, "B"),
	}
	if got := resolveActiveWorkflowID(wfs, "wf-1"); got != "wf-1" {
		t.Fatalf("存储值仍在全集中时应原样返回，got %q", got)
	}
}

// 存储的工作流已被清理 / 不在本次查到的全集里：回落到最新那条，
// 而不是返回空让前端整块 DAG 面板消失。
func TestResolveActiveWorkflowID_FallsBackToLatest(t *testing.T) {
	wfs := []*model.Task{
		wfTask("wf-1", model.TaskStatusCompleted, "A"),
		wfTask("wf-2", model.TaskStatusRunning, "B"),
	}
	if got := resolveActiveWorkflowID(wfs, "wf-9"); got != "wf-2" {
		t.Fatalf("存储值失效时应回落到最后创建的工作流，got %q", got)
	}
	if got := resolveActiveWorkflowID(wfs, ""); got != "wf-2" {
		t.Fatalf("存量对话未写过该列时应回落到最后创建的工作流，got %q", got)
	}
}

// ------------------------------------------------------------
// buildWorkflowList
// ------------------------------------------------------------

// 列表要给切换器足够的区分信息：序号、标题、进度、节点统计、可追加性。
func TestBuildWorkflowList_BasicFields(t *testing.T) {
	wfs := []*model.Task{
		wfTask("wf-1", model.TaskStatusCompleted, "选题调研",
			node("n1", model.TaskStatusCompleted),
			node("n2", model.TaskStatusCompleted),
		),
		wfTask("wf-2", model.TaskStatusRunning, "数据整理",
			node("n3", model.TaskStatusCompleted),
			node("n4", model.TaskStatusRunning),
			node("n5", model.TaskStatusQueued),
		),
	}
	list := buildWorkflowList(wfs, "wf-2")

	if len(list) != 2 {
		t.Fatalf("期望 2 项，got %d", len(list))
	}
	first, second := list[0], list[1]

	if first.Index != 1 || second.Index != 2 {
		t.Fatalf("序号应为 1-based 且与列表顺序一致，got %d / %d", first.Index, second.Index)
	}
	if first.Title != "选题调研" || second.Title != "数据整理" {
		t.Fatalf("标题应取自 task.title，got %q / %q", first.Title, second.Title)
	}
	if first.Active || !second.Active {
		t.Fatalf("active 标记应只落在 wf-2 上，got %v / %v", first.Active, second.Active)
	}
	if first.NodeTotal != 2 || first.NodeDone != 2 || first.NodeActive != 2 {
		t.Fatalf("wf-1 节点统计错误: total=%d done=%d active=%d",
			first.NodeTotal, first.NodeDone, first.NodeActive)
	}
	if second.NodeTotal != 3 || second.NodeActive != 3 || second.NodeDone != 1 {
		t.Fatalf("wf-2 节点统计错误: total=%d active=%d done=%d",
			second.NodeTotal, second.NodeActive, second.NodeDone)
	}
	if !first.CanAppend || !second.CanAppend {
		t.Fatalf("成功与执行中的工作流都应可追加，got %v / %v",
			first.CanAppend, second.CanAppend)
	}
	if second.StatusName == "" || second.CreateTs != 1000 {
		t.Fatalf("状态名/时间戳缺失: %+v", second)
	}
}

// 标题可能来自模型，也可能压根没有（早期数据）。
// 优先 task.title，其次 DAG 内的 title，都没有才回落到「工作流 N」。
func TestBuildWorkflowList_TitleFallback(t *testing.T) {
	// task.title 为空、DAG 里有标题
	fromDAG := wfTask("wf-1", model.TaskStatusCompleted, "", node("n1", model.TaskStatusCompleted))
	fromDAG.TaskOutput = mustJSON(model.WorkflowOutputData{
		Title: "DAG 标题",
		Nodes: []model.WorkflowNodeData{node("n1", model.TaskStatusCompleted)},
	})
	// 两处都没有
	noTitle := wfTask("wf-2", model.TaskStatusCompleted, "")
	noTitle.TaskOutput = mustJSON(model.WorkflowOutputData{
		Nodes: []model.WorkflowNodeData{node("n2", model.TaskStatusCompleted)},
	})

	list := buildWorkflowList([]*model.Task{fromDAG, noTitle}, "wf-1")
	if list[0].Title != "DAG 标题" {
		t.Fatalf("应回落到 DAG 内的 title，got %q", list[0].Title)
	}
	if list[1].Title != "工作流 2" {
		t.Fatalf("都没有时应回落到「工作流 N」，got %q", list[1].Title)
	}
}

// 用户主动停止的工作流不可续接 —— 这是产品硬要求，
// 前端要据此把「继续在此工作流上规划」的入口置灰。
func TestBuildWorkflowList_StoppedNotAppendable(t *testing.T) {
	stopped := wfTask("wf-1", model.TaskStatusStopped, "被叫停的规划",
		node("n1", model.TaskStatusCompleted),
		node("n2", model.TaskStatusStopped),
	)
	list := buildWorkflowList([]*model.Task{stopped}, "wf-1")
	if len(list) != 1 {
		t.Fatalf("期望 1 项，got %d", len(list))
	}
	if list[0].CanAppend {
		t.Fatalf("用户主动停止的工作流不应可追加")
	}
	if list[0].AppendReason == "" {
		t.Fatalf("不可追加时应给出原因供前端展示")
	}
}

// 失败工作流仍可续接，且只统计「失败节点上游」那些可复用的已完成节点。
func TestBuildWorkflowList_FailedWorkflowReusableCount(t *testing.T) {
	failed := wfTask("wf-1", model.TaskStatusFailed, "半途失败的调研",
		node("n1", model.TaskStatusCompleted),
		node("n2", model.TaskStatusCompleted),
		node("n3", model.TaskStatusFailed),    // 失败节点
		node("n4", model.TaskStatusAbandoned), // 失败节点的后代，已废弃
	)
	list := buildWorkflowList([]*model.Task{failed}, "wf-1")
	if !list[0].CanAppend {
		t.Fatalf("有可复用节点时失败工作流应可续接，reason=%q", list[0].AppendReason)
	}
	if list[0].ReusableCount != 2 {
		t.Fatalf("可复用节点应为 n1/n2 两个，got %d", list[0].ReusableCount)
	}
	if list[0].NodeFailed != 1 {
		t.Fatalf("失败节点应计 1 个（n4 是 abandoned 不计 failed），got %d", list[0].NodeFailed)
	}
	if list[0].NodeTotal != 4 || list[0].NodeActive != 4 {
		t.Fatalf("节点统计错误: total=%d active=%d", list[0].NodeTotal, list[0].NodeActive)
	}
}

// 已归档节点（失败分支被剪枝后）仍算在 total 里，因为它们还在 DAG 上灰显展示，
// 但不计入 active —— 与执行器的进度分母口径保持一致。
func TestBuildWorkflowList_ArchivedNodesCounted(t *testing.T) {
	wf := wfTask("wf-1", model.TaskStatusCompleted, "续接后的工作流",
		node("n1", model.TaskStatusCompleted),
		archivedNode("n2", model.TaskStatusFailed),
		archivedNode("n3", model.TaskStatusAbandoned),
		node("n4", model.TaskStatusCompleted),
	)
	list := buildWorkflowList([]*model.Task{wf}, "wf-1")
	item := list[0]
	if item.NodeTotal != 4 {
		t.Fatalf("归档节点仍应在 total 内，got %d", item.NodeTotal)
	}
	if item.NodeArchived != 2 || item.NodeActive != 2 {
		t.Fatalf("归档/活跃节点统计错误: archived=%d active=%d",
			item.NodeArchived, item.NodeActive)
	}
	if item.NodeDone != 2 {
		t.Fatalf("归档节点不应计入已完成，got %d", item.NodeDone)
	}
}

// DAG 解析失败不应该让整条列表挂掉 —— 少几个统计字段而已，
// 用户仍然要能切换查看这条工作流。
func TestBuildWorkflowList_BadOutputStillListed(t *testing.T) {
	broken := wfTask("wf-1", model.TaskStatusRunning, "坏数据")
	broken.TaskOutput = "{not-json"

	list := buildWorkflowList([]*model.Task{broken}, "wf-1")
	if len(list) != 1 {
		t.Fatalf("解析失败也应保留该条，got %d", len(list))
	}
	if list[0].Title != "坏数据" || list[0].TaskID != "wf-1" || !list[0].Active {
		t.Fatalf("基础字段缺失: %+v", list[0])
	}
	if list[0].NodeTotal != 0 || list[0].CanAppend {
		t.Fatalf("无 DAG 时统计应为零、不应误判为可追加: %+v", list[0])
	}
}

// ------------------------------------------------------------
// 辅助
// ------------------------------------------------------------

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
