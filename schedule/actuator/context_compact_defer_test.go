package actuator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/schedule/container"
)

// ============================================================
// 延后重排：退避时长
// ============================================================

// TestNextDeferDelay 等待时长按指数增长并封顶。
//
// 指数退避的取舍：前置任务可能一秒内就收尾，也可能要跑几分钟。短间隔起步
// 保证前者几乎无感，指数增长避免后者被高频唤醒（每轮都要重建历史 + 计量
// token），封顶保证「前置任务一收尾，本任务能在 30s 内重新被扫到」。
func TestNextDeferDelay(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 30 * time.Second}, // 32s 被封到 30s
		{9, 30 * time.Second},
		{100, 30 * time.Second},
		{-1, 2 * time.Second}, // 脏数据兜底
	}
	for _, c := range cases {
		if got := nextDeferDelay(c.count); got != c.want {
			t.Fatalf("nextDeferDelay(%d) = %v, want %v", c.count, got, c.want)
		}
	}
}

// ============================================================
// 延后重排：落库、hint、退避递增
// ============================================================

// openDeferTestDB 开一个独立的 sqlite 连接（不碰 dao 全局连接）。
//
// 走 TaskDAO.Tx 注入而不是 dao.Init：dao 全局连接是进程级的，本包还有
// 依赖真实库的用例（realdata），抢全局会让两边互相干扰。
func openDeferTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "defer.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := d.AutoMigrate(&model.Task{}, &model.TaskTemp{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return d
}

// TestDeferTaskForCompression 延后重排要同时落库、打 hint，并按次数放大等待。
func TestDeferTaskForCompression(t *testing.T) {
	db := openDeferTestDB(t)
	ctx := context.Background()

	temp := &model.TaskTemp{Task: model.Task{
		TaskID:   "task-new",
		ChatID:   "chat-1",
		Title:    "t",
		TaskType: model.TaskTypeConversation,
		Status:   model.TaskStatusQueued,
		Stage:    int(model.StageFunctionalCallDoing),
	}}
	if err := db.Create(temp).Error; err != nil {
		t.Fatalf("create temp task: %v", err)
	}
	// 终态表同样双写一份（线上就是两表都有），否则测不到「两表同步」
	if err := db.Create(&model.Task{
		TaskID:   "task-new",
		ChatID:   "chat-1",
		Title:    "t",
		TaskType: model.TaskTypeConversation,
		Status:   model.TaskStatusQueued,
		Stage:    int(model.StageFunctionalCallDoing),
	}).Error; err != nil {
		t.Fatalf("create task row: %v", err)
	}

	// funcCallRegistry 是包级的（真实进程里由 NewFuncCallActuator 注入一次），
	// 这里换上去就必须还原 —— 否则后续用例会连到这个临时 sqlite 上。
	prevDAO := funcCallRegistry.taskDAO
	t.Cleanup(func() { funcCallRegistry.taskDAO = prevDAO })
	funcCallRegistry.taskDAO = &dao.TaskDAO{Tx: db}
	container.GetTransitionStore().ClearHint("task-new")

	// 第一次延后：2s
	before := time.Now()
	if err := deferTaskForCompression(ctx, temp, &deferWaitError{TaskIDs: []string{"task-old"}}); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if temp.DeferCount != 1 || temp.DelayUntil == nil {
		t.Fatalf("defer count = %d, until = %v, want 1 / non-nil", temp.DeferCount, temp.DelayUntil)
	}
	if d := temp.DelayUntil.Sub(before); d < 2*time.Second || d > 3*time.Second {
		t.Fatalf("first delay = %v, want ~2s", d)
	}

	// 两表都要写到：调度扫描读 task_temp，前端/调试读 task 表
	var tempRow model.TaskTemp
	if err := db.Where("task_id = ?", "task-new").First(&tempRow).Error; err != nil {
		t.Fatalf("reload temp: %v", err)
	}
	if tempRow.DeferCount != 1 || tempRow.DelayUntil == nil {
		t.Fatalf("task_temp defer = %d / %v, want 1 / non-nil", tempRow.DeferCount, tempRow.DelayUntil)
	}
	var taskRow model.Task
	if err := db.Where("task_id = ?", "task-new").First(&taskRow).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if taskRow.DeferCount != 1 || taskRow.DelayUntil == nil {
		t.Fatalf("task defer = %d / %v, want 1 / non-nil", taskRow.DeferCount, taskRow.DelayUntil)
	}

	// hint：容器据此把任务拨回本阶段 wait，而不是推进或收口
	hint := container.GetTransitionStore().GetHint("task-new")
	if hint == nil || !hint.Deferred {
		t.Fatalf("hint = %+v, want Deferred=true", hint)
	}

	// 第二次延后：退避翻倍到 4s
	before = time.Now()
	if err := deferTaskForCompression(ctx, temp, &deferWaitError{TaskIDs: []string{"task-old"}}); err != nil {
		t.Fatalf("defer twice: %v", err)
	}
	if temp.DeferCount != 2 {
		t.Fatalf("defer count = %d, want 2", temp.DeferCount)
	}
	if d := temp.DelayUntil.Sub(before); d < 4*time.Second || d > 5*time.Second {
		t.Fatalf("second delay = %v, want ~4s", d)
	}
}

// TestDeferredHintLifecycle 延后标记必须能随下一轮执行清掉。
//
// hint 常驻内存（还要带着 ToolCallRound 等跨轮状态），若延后标记不清理，
// 下一轮即便正常执行完，容器仍会把它拨回 wait 重排 —— 任务从此原地打转，
// 表现是「用户的问题永远不执行、也不报错」。
func TestDeferredHintLifecycle(t *testing.T) {
	store := container.GetTransitionStore()
	taskID := "defer-lifecycle"
	store.ClearHint(taskID)

	store.SetDeferred(taskID)
	if h := store.GetHint(taskID); h == nil || !h.Deferred {
		t.Fatalf("SetDeferred 后 hint = %+v", h)
	}
	// 清理只动 Deferred，不能连带丢掉其它跨轮决策
	store.SetHint(taskID, &model.StageTransitionHint{Deferred: true, ToolCallRound: 7})
	store.ClearDeferred(taskID)
	h := store.GetHint(taskID)
	if h.Deferred {
		t.Fatalf("ClearDeferred 后仍为 Deferred")
	}
	if h.ToolCallRound != 7 {
		t.Fatalf("ClearDeferred 不应清掉其它字段, ToolCallRound = %d", h.ToolCallRound)
	}
	store.ClearHint(taskID)
}

// ============================================================
// 退避预算用尽后的降级：跳过、绝不压未完成的前置任务
// ============================================================

// TestExcludeSegments 未完成的前置任务段要从可压缩集合里剔除。
//
// 这是「等不下去了」时的兜底：继续等会让用户的新问题永远不执行，但把还在跑
// 的任务压进摘要更糟 —— 它之后产生的 turn 会被 covered 永久过滤掉。
// 剔除之后它们的原文仍留在上下文里（只是不参与压缩），最多是触到超底线报错，
// 不会静默丢掉用户已经等到的结果。
func TestExcludeSegments(t *testing.T) {
	segs := []historySegment{
		{TaskID: "old-1", Messages: []llm.Message{{Role: llm.RoleUser, Content: "a"}}},
		{TaskID: "running", Messages: []llm.Message{{Role: llm.RoleUser, Content: "b"}}},
		{TaskID: "old-2", Messages: []llm.Message{{Role: llm.RoleUser, Content: "c"}}},
	}
	got := excludeSegments(segs, []string{"running"})
	if len(got) != 2 {
		t.Fatalf("excludeSegments len = %d, want 2", len(got))
	}
	for _, s := range got {
		if s.TaskID == "running" {
			t.Fatalf("未完成的前置任务段必须被剔除")
		}
	}
	// 空排除集 = 原样返回
	if len(excludeSegments(segs, nil)) != 3 {
		t.Fatalf("nil exclude 应保持原样")
	}
}
