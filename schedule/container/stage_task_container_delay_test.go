package container

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"

	framework "github.com/memory-overflow/light-task-scheduler"
)

// initDelayTestDB 起一个临时 sqlite 库并建表。
//
// 用真库而不是 mock：要验的正是 SQL 里的 delay_until 过滤条件
// （含 NULL 语义），mock 容器接口等于把被测对象替掉了。
func initDelayTestDB(t *testing.T) {
	t.Helper()
	cfg := config.DatabaseConfig{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "delay.db"),
	}
	if err := dao.Init(cfg); err != nil {
		t.Fatalf("init sqlite: %v", err)
	}
	if err := dao.Migrate(&model.Task{}, &model.TaskTemp{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// resetTempTasks 清空待调度表：本文件的用例共用同一个 sqlite 库
// （dao.Init 进程内只生效一次），不清会互相看到对方造的行。
func resetTempTasks(t *testing.T) {
	t.Helper()
	if err := dao.DB().Unscoped().Where("1 = 1").Delete(&model.TaskTemp{}).Error; err != nil {
		t.Fatalf("reset task_temp: %v", err)
	}
}

// newTemp 造一条待调度的任务行。
func newTemp(taskID string, delayUntil *time.Time) *model.TaskTemp {
	temp := &model.TaskTemp{Task: model.Task{
		TaskID:     taskID,
		ChatID:     "chat-1",
		Title:      "t",
		TaskType:   model.TaskTypeConversation,
		Status:     model.TaskStatusQueued,
		Stage:      int(model.StageFunctionalCallWait),
		DelayUntil: delayUntil,
	}}
	return temp
}

// TestToSuccessStatus_DeferredRequeues 延后重排的容器侧闭环：
// doing + Deferred → 回到本阶段 wait（不是终态、不是下一阶段），
// 且延后期间扫不到、到期后自动回到队列。
func TestToSuccessStatus_DeferredRequeues(t *testing.T) {
	initDelayTestDB(t)
	resetTempTasks(t)
	ctx := context.Background()
	db := dao.DB().WithContext(ctx)

	// 模拟「任务已进入 functional_call_doing」——延后就发生在这个阶段
	temp := &model.TaskTemp{Task: model.Task{
		TaskID:   "t-defer-cycle",
		ChatID:   "chat-1",
		Title:    "t",
		TaskType: model.TaskTypeConversation,
		Status:   model.TaskStatusRunning,
		Stage:    int(model.StageFunctionalCallDoing),
	}}
	if err := db.Create(temp).Error; err != nil {
		t.Fatalf("create temp task: %v", err)
	}
	taskDAO := &dao.TaskDAO{}
	if err := taskDAO.SetTaskDelay(ctx, temp.TaskID, time.Now().Add(20*time.Second), 1); err != nil {
		t.Fatalf("set delay: %v", err)
	}
	GetTransitionStore().SetDeferred(temp.TaskID)

	c := NewStageTaskContainer(model.StageFunctionalCallWait, "test", nil)
	frameworkTask := c.toFrameworkTask(temp, framework.TASK_STATUS_SUCCESS)
	if _, err := c.ToSuccessStatus(ctx, &frameworkTask); err != nil {
		t.Fatalf("to success status: %v", err)
	}

	var after model.TaskTemp
	if err := db.Where("task_id = ?", temp.TaskID).First(&after).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if model.TaskStage(after.Stage) != model.StageFunctionalCallWait {
		t.Fatalf("stage = %s, want functional_call_wait（回到本阶段重新排队）",
			model.TaskStage(after.Stage))
	}
	if after.Status != model.TaskStatusQueued {
		t.Fatalf("status = %v, want queued", after.Status)
	}
	if after.DelayUntil == nil {
		t.Fatalf("delay_until 应保留，否则下一轮扫描会立刻再次唤醒任务")
	}

	// 延后期间扫不到
	got, err := c.GetWaitingTask(ctx, 10)
	if err != nil {
		t.Fatalf("get waiting: %v", err)
	}
	for _, tk := range got {
		if tk.TaskId == temp.TaskID {
			t.Fatalf("延后未到期不应被扫到")
		}
	}

	// 到期后自动回到队列（无需任何唤醒动作）
	if err := db.Model(&model.TaskTemp{}).Where("task_id = ?", temp.TaskID).
		Update("delay_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatalf("expire delay: %v", err)
	}
	got, err = c.GetWaitingTask(ctx, 10)
	if err != nil {
		t.Fatalf("get waiting after expiry: %v", err)
	}
	found := false
	for _, tk := range got {
		if tk.TaskId == temp.TaskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("delay 到期后任务应重新进入调度队列")
	}
}

// TestGetWaitingTask_SkipsDelayedUntilDue 验证调度扫描跳过未到期的延后任务。
//
// 场景：同一个对话里用户连发两个问题，后一个问题的上下文压缩要等前一个
// 跑完，于是被延后。若不跳过，调度器每 1s 扫一次就会每 1s 唤醒它一次，
// 每轮都重建整段对话历史并计量 token —— 纯粹的空转。
func TestGetWaitingTask_SkipsDelayedUntilDue(t *testing.T) {
	initDelayTestDB(t)
	resetTempTasks(t)
	ctx := context.Background()
	db := dao.DB().WithContext(ctx)

	future := time.Now().Add(30 * time.Second)
	past := time.Now().Add(-time.Minute)
	rows := []*model.TaskTemp{
		newTemp("t-no-delay", nil),
		newTemp("t-delay-future", &future),
		newTemp("t-delay-past", &past),
	}
	for _, r := range rows {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("create temp task %s: %v", r.TaskID, err)
		}
	}

	c := NewStageTaskContainer(model.StageFunctionalCallWait, "test", nil)
	got, err := c.GetWaitingTask(ctx, 10)
	if err != nil {
		t.Fatalf("get waiting task: %v", err)
	}

	ids := map[string]bool{}
	for _, tk := range got {
		ids[tk.TaskId] = true
	}
	if len(got) != 2 {
		t.Fatalf("waiting tasks = %v, want 2 (未延后 + 已到期)", ids)
	}
	if !ids["t-no-delay"] {
		t.Fatalf("未延后的任务应被扫到, got %v", ids)
	}
	if !ids["t-delay-past"] {
		t.Fatalf("已到期的延后任务应被扫到, got %v", ids)
	}
	if ids["t-delay-future"] {
		t.Fatalf("未到期的延后任务不应被扫到")
	}
}
