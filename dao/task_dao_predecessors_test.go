package dao

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/model"
)

// initPredecessorTestDB 起一个临时 sqlite 库并建表（仅本文件用例使用）。
func initPredecessorTestDB(t *testing.T) {
	t.Helper()
	if err := Init(config.DatabaseConfig{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "pred.db"),
	}); err != nil {
		t.Fatalf("init sqlite: %v", err)
	}
	if err := Migrate(&model.Task{}, &model.TaskTemp{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// TestListUnfinishedPredecessors 只返回「更早、仍在进行、且是对话任务」的前置任务。
//
// 压缩要等的就是这一批：它们还在写 turn，此时把它们压进摘要，后续产生的
// 交互会被 covered 过滤永久丢掉。反之，下面这些都不该拦住压缩：
//   - 工作流任务：跑满一个 DAG 可能几十分钟，等它收尾不现实；而且它在
//     历史里以「工作流摘要段」呈现，本就不参与详情压缩；
//   - 子任务：执行细节，turn 走父任务的摘要段；
//   - 已收尾的对话任务：它的 turn 不会再增加，压它是安全的；
//   - 更晚创建的任务：它不是「前置」，它自己的压缩由它自己负责。
func TestListUnfinishedPredecessors(t *testing.T) {
	initPredecessorTestDB(t)
	ctx := context.Background()
	db := DB().WithContext(ctx)

	base := time.Now().Add(-time.Hour)
	mk := func(id string, ttype model.TaskType, status model.TaskStatus, offset time.Duration) *model.Task {
		return &model.Task{
			TaskID:    id,
			ChatID:    "chat-1",
			Title:     id,
			TaskType:  ttype,
			Status:    status,
			CreatedAt: base.Add(offset),
		}
	}

	// 更晚创建的对话任务（本任务的"后继"），不该被算作前置
	now := base.Add(30 * time.Minute)

	// 运行中的任务在 temp 表；终态的只留在 task 表（与线上双表写入一致）
	temps := []*model.TaskTemp{
		{Task: *mk("conv-older-running", model.TaskTypeConversation, model.TaskStatusRunning, 0)},
		{Task: *mk("wf-older-running", model.TaskTypeWorkflow, model.TaskStatusRunning, time.Minute)},
		{Task: *mk("sub-older-running", model.TaskTypeWorkflowSubTask, model.TaskStatusRunning, 2*time.Minute)},
		{Task: *mk("conv-newer-running", model.TaskTypeConversation, model.TaskStatusRunning, 40*time.Minute)},
	}
	for _, r := range temps {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("create temp %s: %v", r.TaskID, err)
		}
	}
	finals := []*model.Task{
		mk("conv-older-done", model.TaskTypeConversation, model.TaskStatusCompleted, 3*time.Minute),
		mk("conv-other-chat", model.TaskTypeConversation, model.TaskStatusRunning, 4*time.Minute),
	}
	finals[1].ChatID = "chat-2"
	for _, r := range finals {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("create task %s: %v", r.TaskID, err)
		}
	}

	d := &TaskDAO{}
	got, err := d.ListUnfinishedPredecessors(ctx, "chat-1", now)
	if err != nil {
		t.Fatalf("list unfinished predecessors: %v", err)
	}
	if len(got) != 1 || got[0].TaskID != "conv-older-running" {
		ids := make([]string, 0, len(got))
		for _, g := range got {
			ids = append(ids, g.TaskID)
		}
		t.Fatalf("predecessors = %v, want [conv-older-running]", ids)
	}

	// 前置任务收尾后不再阻塞 —— 这是延后重排能收敛的前提
	if err := db.Model(&model.TaskTemp{}).
		Where("task_id = ?", "conv-older-running").
		Update("status", model.TaskStatusCompleted).Error; err != nil {
		t.Fatalf("finish predecessor: %v", err)
	}
	got, err = d.ListUnfinishedPredecessors(ctx, "chat-1", now)
	if err != nil {
		t.Fatalf("list after finish: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("前置任务收尾后应返回空, got %d", len(got))
	}
}
