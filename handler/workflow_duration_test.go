package handler

import (
	"testing"
	"time"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
)

// 工作流节点时长的四个要点：
//  1. 节点时长要含它派生的 deep_search 孙任务（与 token 同口径，都是「这棵子树」）
//  2. 运行中的任务以当前时间为终点，且 end_ts = 0（前端据此本地秒跳）
//  3. 还没被调度到（无 start_time）时排队时间也要算进去，用 created_time 兜底
//  4. 终态却没有 end_time 的历史脏数据用 updated_time，不能让耗时无限上涨

var (
	baseTime     = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	nowForTest   = baseTime.Add(60 * time.Second)
	ptr          = func(t time.Time) *time.Time { return &t }
	testChildren = []dao.TaskTimeRange{
		{
			TaskID: "t1", ParentTaskID: "wf", Status: model.TaskStatusCompleted,
			StartTime: ptr(baseTime), EndTime: ptr(baseTime.Add(10 * time.Second)),
			CreatedAt: baseTime, UpdatedAt: baseTime.Add(10 * time.Second),
		},
		{
			TaskID: "t2", ParentTaskID: "wf", Status: model.TaskStatusRunning,
			StartTime: ptr(baseTime.Add(5 * time.Second)), EndTime: nil,
			CreatedAt: baseTime.Add(5 * time.Second), UpdatedAt: baseTime.Add(20 * time.Second),
		},
		{
			// 已派发但还没被调度：无 start_time，排队时长（5s）应算进去
			TaskID: "t3", ParentTaskID: "wf", Status: model.TaskStatusQueued,
			StartTime: nil, EndTime: nil,
			CreatedAt: baseTime.Add(55 * time.Second), UpdatedAt: baseTime.Add(55 * time.Second),
		},
	}
	testGrandchildren = []dao.TaskTimeRange{
		{
			// deep_search 子任务跑得比父节点还久，节点时长应被它撑开
			TaskID: "d1", ParentTaskID: "t1", Status: model.TaskStatusCompleted,
			StartTime: ptr(baseTime.Add(2 * time.Second)), EndTime: ptr(baseTime.Add(12 * time.Second)),
			CreatedAt: baseTime.Add(2 * time.Second), UpdatedAt: baseTime.Add(12 * time.Second),
		},
	}
)

func TestBuildWorkflowDurations(t *testing.T) {
	got := buildWorkflowDurations(testChildren, testGrandchildren, nowForTest)

	// t1：自身 0~10s，被孙任务 d1 的结束时间（12s）撑开 → 12s，已终态
	d1 := got["t1"]
	if d1.DurationMs != 12_000 {
		t.Errorf("t1 时长应含 deep_search 孙任务，期望 12000ms，实际 %dms", d1.DurationMs)
	}
	if d1.Running || d1.EndTs == 0 {
		t.Errorf("t1 已终态，应有结束时间且 running=false，实际 %+v", d1)
	}

	// t2：运行中，终点取 now → 60s - 5s = 55s
	d2 := got["t2"]
	if d2.DurationMs != 55_000 {
		t.Errorf("t2 运行中应以当前时间为终点，期望 55000ms，实际 %dms", d2.DurationMs)
	}
	if !d2.Running || d2.EndTs != 0 {
		t.Errorf("t2 运行中：running 应为 true、end_ts 应为 0，实际 %+v", d2)
	}

	// t3：排队中，用 created_time 起算 → 60s - 55s = 5s
	d3 := got["t3"]
	if d3.DurationMs != 5_000 || !d3.Running {
		t.Errorf("t3 排队时间应计入，期望 5000ms/running，实际 %+v", d3)
	}
}

// 终态却没有 end_time（历史脏数据）：退回 updated_time，不能一直涨。
func TestBuildWorkflowDurationsTerminalWithoutEndTime(t *testing.T) {
	children := []dao.TaskTimeRange{{
		TaskID: "t1", ParentTaskID: "wf", Status: model.TaskStatusFailed,
		StartTime: ptr(baseTime), EndTime: nil,
		CreatedAt: baseTime, UpdatedAt: baseTime.Add(8 * time.Second),
	}}
	got := buildWorkflowDurations(children, nil, nowForTest)

	d := got["t1"]
	if d.DurationMs != 8_000 {
		t.Errorf("终态缺 end_time 应退回 updated_time，期望 8000ms，实际 %dms", d.DurationMs)
	}
	if d.Running || d.EndTs == 0 {
		t.Errorf("终态任务不应标记 running，实际 %+v", d)
	}
}

// 结束早于开始的数据异常：截断到 0，别让前端显示负数。
func TestBuildWorkflowDurationsNegativeSpan(t *testing.T) {
	children := []dao.TaskTimeRange{{
		TaskID: "t1", ParentTaskID: "wf", Status: model.TaskStatusCompleted,
		StartTime: ptr(baseTime.Add(10 * time.Second)), EndTime: ptr(baseTime),
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}}
	got := buildWorkflowDurations(children, nil, nowForTest)

	if got["t1"].DurationMs != 0 {
		t.Errorf("异常时间区间应截断到 0，实际 %dms", got["t1"].DurationMs)
	}
}

// 时长要真正挂到节点视图上，未派发节点（无 task_id）不该带。
func TestBuildWorkflowNodeViewsDuration(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", TaskID: "t1", Status: model.TaskStatusCompleted},
		{NodeID: "n2", TaskID: "t2", Status: model.TaskStatusRunning},
		{NodeID: "n3", Status: model.TaskStatusQueued},
	}
	durations := buildWorkflowDurations(testChildren, testGrandchildren, nowForTest)

	views, _ := buildWorkflowNodeViews(nodes, nil, "wf", durations)
	if len(views) != 3 {
		t.Fatalf("节点视图数量应为 3，实际 %d", len(views))
	}
	if got, ok := views[0]["duration"].(WorkflowDuration); !ok || got.DurationMs != 12_000 {
		t.Errorf("n1 应挂上 12000ms 时长，实际 %v", views[0]["duration"])
	}
	if got, ok := views[1]["duration"].(WorkflowDuration); !ok || !got.Running {
		t.Errorf("n2 应挂上运行时长，实际 %v", views[1]["duration"])
	}
	if _, ok := views[2]["duration"]; ok {
		t.Error("未派发节点（无 task_id）不应带 duration")
	}
}
