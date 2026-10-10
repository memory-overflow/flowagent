package actuator

import (
	"context"
	"errors"
	"sync"
	"testing"

	framework "github.com/memory-overflow/light-task-scheduler"
	"github.com/memory-overflow/flowagent/model"
)

// fakeInner 模拟 framework 的 function_actuator 行为：
// 只认识自己 Start 过的任务，对其他任务返回 FAILED("同步任务未找到")，
// 且查到非 RUNNING 状态后删除缓存（一次性读取语义）。
type fakeInner struct {
	mu       sync.Mutex
	started  map[string]framework.AsyncTaskStatus
	startErr error
	// queried 记录内层实际被问到的 taskID，用于断言过滤生效
	queried []string
	// stopped 记录 Stop 被调用的 taskID 顺序
	stopped []string
}

func newFakeInner() *fakeInner {
	return &fakeInner{started: map[string]framework.AsyncTaskStatus{}}
}

func (f *fakeInner) Init(_ context.Context, t *framework.Task) (*framework.Task, error) {
	return t, nil
}

func (f *fakeInner) Start(_ context.Context, t *framework.Task) (*framework.Task, bool, error) {
	if f.startErr != nil {
		return t, false, f.startErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started[t.TaskId] = framework.AsyncTaskStatus{TaskStatus: framework.TASK_STATUS_RUNNING}
	return t, false, nil
}

func (f *fakeInner) GetAsyncTaskStatus(_ context.Context, tasks []framework.Task) (
	[]framework.AsyncTaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]framework.AsyncTaskStatus, 0, len(tasks))
	for _, t := range tasks {
		f.queried = append(f.queried, t.TaskId)
		st, ok := f.started[t.TaskId]
		if !ok {
			// 这正是线上报错的来源
			out = append(out, framework.AsyncTaskStatus{
				TaskStatus:   framework.TASK_STATUS_FAILED,
				FailedReason: errors.New("同步任务未找到"),
			})
			continue
		}
		if st.TaskStatus != framework.TASK_STATUS_RUNNING {
			delete(f.started, t.TaskId) // 一次性语义
		}
		out = append(out, st)
	}
	return out, nil
}

func (f *fakeInner) GetOutput(_ context.Context, _ *framework.Task) (interface{}, error) {
	return nil, nil
}

func (f *fakeInner) Stop(_ context.Context, t *framework.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.started, t.TaskId)
	f.stopped = append(f.stopped, t.TaskId)
	return nil
}

// setStatus 模拟任务执行完成
func (f *fakeInner) setStatus(taskID string, st framework.AsyncTaskStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started[taskID] = st
}

func task(id string) framework.Task { return framework.Task{TaskId: id} }

// TestStageAffine_ForeignTaskNotFailed 核心不变量：
// 本阶段没启动过的任务不能被判失败。
//
// 这是线上 "同步任务未找到" 的直接成因 —— DB 里 stage=doing 是全局可见的，
// 框架会把归属其他阶段的任务也拿来问本阶段 actuator 要状态。
func TestStageAffine_ForeignTaskNotFailed(t *testing.T) {
	inner := newFakeInner()
	a := WrapStageAffine(inner, "functional_call")
	ctx := context.Background()

	// 未经本实例 Start 的任务
	st, err := a.GetAsyncTaskStatus(ctx, []framework.Task{task("foreign")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st) != 1 {
		t.Fatalf("expected 1 status, got %d", len(st))
	}
	if st[0].TaskStatus != framework.TASK_STATUS_RUNNING {
		t.Errorf("foreign task should report RUNNING, got %v (reason=%v)",
			st[0].TaskStatus, st[0].FailedReason)
	}

	// 更关键：不该把它透传给内层，否则内层会返回 FAILED
	if len(inner.queried) != 0 {
		t.Errorf("foreign task should not be forwarded to inner, got %v", inner.queried)
	}
}

// TestStageAffine_OwnedTaskPassThrough 本实例启动过的任务，状态要如实透传
func TestStageAffine_OwnedTaskPassThrough(t *testing.T) {
	inner := newFakeInner()
	a := WrapStageAffine(inner, "planning")
	ctx := context.Background()

	if _, _, err := a.Start(ctx, &framework.Task{TaskId: "mine"}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 运行中
	st, _ := a.GetAsyncTaskStatus(ctx, []framework.Task{task("mine")})
	if st[0].TaskStatus != framework.TASK_STATUS_RUNNING {
		t.Errorf("expected RUNNING, got %v", st[0].TaskStatus)
	}

	// 执行完成 → 成功状态必须如实上报，否则任务永远不会流转
	inner.setStatus("mine", framework.AsyncTaskStatus{
		TaskStatus: framework.TASK_STATUS_SUCCESS, Progress: 100,
	})
	st, _ = a.GetAsyncTaskStatus(ctx, []framework.Task{task("mine")})
	if st[0].TaskStatus != framework.TASK_STATUS_SUCCESS {
		t.Errorf("expected SUCCESS, got %v", st[0].TaskStatus)
	}

	// 真实失败也必须如实上报，不能被 wrapper 吞掉
	if _, _, err := a.Start(ctx, &framework.Task{TaskId: "failing"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	inner.setStatus("failing", framework.AsyncTaskStatus{
		TaskStatus:   framework.TASK_STATUS_FAILED,
		FailedReason: errors.New("llm timeout"),
	})
	st, _ = a.GetAsyncTaskStatus(ctx, []framework.Task{task("failing")})
	if st[0].TaskStatus != framework.TASK_STATUS_FAILED {
		t.Errorf("real failure must be reported, got %v", st[0].TaskStatus)
	}
	if st[0].FailedReason == nil || st[0].FailedReason.Error() != "llm timeout" {
		t.Errorf("failure reason lost: %v", st[0].FailedReason)
	}
}

// TestStageAffine_MixedBatch 混合批次：顺序与归属都要正确。
// 框架依赖 status[i] 与 tasks[i] 一一对应，错位会导致状态串台。
func TestStageAffine_MixedBatch(t *testing.T) {
	inner := newFakeInner()
	a := WrapStageAffine(inner, "tool_call")
	ctx := context.Background()

	a.Start(ctx, &framework.Task{TaskId: "b"})
	a.Start(ctx, &framework.Task{TaskId: "d"})
	inner.setStatus("d", framework.AsyncTaskStatus{TaskStatus: framework.TASK_STATUS_SUCCESS})

	batch := []framework.Task{task("a"), task("b"), task("c"), task("d")}
	st, err := a.GetAsyncTaskStatus(ctx, batch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st) != len(batch) {
		t.Fatalf("status length %d != tasks length %d", len(st), len(batch))
	}

	want := []framework.TaskStatus{
		framework.TASK_STATUS_RUNNING, // a: 非本阶段
		framework.TASK_STATUS_RUNNING, // b: 本阶段，运行中
		framework.TASK_STATUS_RUNNING, // c: 非本阶段
		framework.TASK_STATUS_SUCCESS, // d: 本阶段，已完成
	}
	for i := range want {
		if st[i].TaskStatus != want[i] {
			t.Errorf("task %s: got %v want %v", batch[i].TaskId, st[i].TaskStatus, want[i])
		}
	}
}

// TestStageAffine_StartFailureCleansLedger Start 失败要撤销登记，避免账本泄漏
func TestStageAffine_StartFailureCleansLedger(t *testing.T) {
	inner := newFakeInner()
	inner.startErr = errors.New("boom")
	a := WrapStageAffine(inner, "planning")
	ctx := context.Background()

	if _, _, err := a.Start(ctx, &framework.Task{TaskId: "x"}); err == nil {
		t.Fatal("expected start error")
	}

	// 账本里不应留下 x，否则会被当成本阶段任务去问内层，反而触发原 bug
	st, _ := a.GetAsyncTaskStatus(ctx, []framework.Task{task("x")})
	if st[0].TaskStatus != framework.TASK_STATUS_RUNNING {
		t.Errorf("expected RUNNING (not owned), got %v", st[0].TaskStatus)
	}
	if len(inner.queried) != 0 {
		t.Errorf("failed-start task should not be forwarded, got %v", inner.queried)
	}
}

// TestStageAffine_StopRemovesFromLedger Stop 后不再视为本阶段任务
func TestStageAffine_StopRemovesFromLedger(t *testing.T) {
	inner := newFakeInner()
	a := WrapStageAffine(inner, "planning")
	ctx := context.Background()

	a.Start(ctx, &framework.Task{TaskId: "s"})
	if err := a.Stop(ctx, &framework.Task{TaskId: "s"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	st, _ := a.GetAsyncTaskStatus(ctx, []framework.Task{task("s")})
	if st[0].TaskStatus != framework.TASK_STATUS_RUNNING {
		t.Errorf("stopped task should not be reported FAILED, got %v", st[0].TaskStatus)
	}
}

// TestStageAffine_TerminalStatusClearsLedger 内层查到终态后会删自己的缓存
// （一次性语义），账本要同步移除，否则下次查询会命中账本但内层已无记录 → FAILED
func TestStageAffine_TerminalStatusClearsLedger(t *testing.T) {
	inner := newFakeInner()
	a := WrapStageAffine(inner, "planning")
	ctx := context.Background()

	a.Start(ctx, &framework.Task{TaskId: "t"})
	inner.setStatus("t", framework.AsyncTaskStatus{TaskStatus: framework.TASK_STATUS_SUCCESS})

	// 第一次查询：内层返回 SUCCESS 并删除缓存
	st, _ := a.GetAsyncTaskStatus(ctx, []framework.Task{task("t")})
	if st[0].TaskStatus != framework.TASK_STATUS_SUCCESS {
		t.Fatalf("first query: expected SUCCESS, got %v", st[0].TaskStatus)
	}

	// 第二次查询：内层已无记录。若账本没同步清理，会透传过去拿到 FAILED
	st, _ = a.GetAsyncTaskStatus(ctx, []framework.Task{task("t")})
	if st[0].TaskStatus == framework.TASK_STATUS_FAILED {
		t.Errorf("repeated query must not produce spurious FAILED: %v", st[0].FailedReason)
	}
}

// TestStageAffine_EmptyBatch 空批次不应炸
func TestStageAffine_EmptyBatch(t *testing.T) {
	a := WrapStageAffine(newFakeInner(), "planning")
	st, err := a.GetAsyncTaskStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st) != 0 {
		t.Errorf("expected empty status, got %d", len(st))
	}
}

// TestStageAffine_ConcurrentAccess 状态维护线程与调度线程会并发访问账本
func TestStageAffine_ConcurrentAccess(t *testing.T) {
	inner := newFakeInner()
	a := WrapStageAffine(inner, "functional_call")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		id := string(rune('a' + i%26))
		go func() {
			defer wg.Done()
			a.Start(ctx, &framework.Task{TaskId: id})
		}()
		go func() {
			defer wg.Done()
			a.GetAsyncTaskStatus(ctx, []framework.Task{task(id)})
		}()
	}
	wg.Wait()
}

// TestStageAffine_SkipAlreadyEndedTask 任务在「被调度器捞起」与「真正执行」之间
// 被停止时，必须安静地放弃这一轮。
//
// 这是「工作流停了，deep_search 还在跑」的第二道防线：停止动作在另一条链路上
// 直接把任务置终态，而框架的调度顺序是「先 Start、后 ToRunningStatus」，
// 两者并发时任务可能已经 stopped 才被 Start。
//
// 关键不变量是 ignoreErr=true：框架对非忽略的错误会调 failed() 把任务判失败，
// 那会把用户主动 stopped 的终态覆盖成 failed。
func TestStageAffine_SkipAlreadyEndedTask(t *testing.T) {
	orig := isTaskActive
	isTaskActive = func(_ context.Context, id string) bool { return id != "stopped" }
	defer func() { isTaskActive = orig }()

	inner := newFakeInner()
	a := WrapStageAffine(inner, "functional_call").(*stageAffineActuator)

	_, ignore, err := a.Start(context.Background(), &framework.Task{TaskId: "stopped"})
	if err == nil {
		t.Fatal("expected non-nil error to signal the skip")
	}
	if !ignore {
		t.Error("ignore must be true: otherwise the framework marks it failed and overwrites stopped")
	}

	// 内层不能被启动 —— 否则这一轮 LLM 调用就白跑了
	inner.mu.Lock()
	started := len(inner.started)
	inner.mu.Unlock()
	if started != 0 {
		t.Errorf("inner must not be started for a stopped task, got %d", started)
	}

	// 也不能登记进账本，否则状态查询会对一个已死任务一直报 RUNNING
	a.mu.RLock()
	_, owned := a.owned["stopped"]
	a.mu.RUnlock()
	if owned {
		t.Error("skipped task must not be recorded in the ledger")
	}
}

// TestStageAffine_StartsActiveTask 存活判定通过时，行为与加校验之前完全一致
func TestStageAffine_StartsActiveTask(t *testing.T) {
	orig := isTaskActive
	isTaskActive = func(_ context.Context, _ string) bool { return true }
	defer func() { isTaskActive = orig }()

	inner := newFakeInner()
	a := WrapStageAffine(inner, "functional_call")

	if _, _, err := a.Start(context.Background(), &framework.Task{TaskId: "live"}); err != nil {
		t.Fatalf("active task must start normally: %v", err)
	}
	st, _ := a.GetAsyncTaskStatus(context.Background(), []framework.Task{task("live")})
	if st[0].TaskStatus != framework.TASK_STATUS_RUNNING {
		t.Errorf("expected RUNNING, got %v", st[0].TaskStatus)
	}
}

// runningGuardInner 模拟框架 fucntionActuator 的关键行为：
// Start 时若缓存里已有 RUNNING 就报「task is running」，Stop 才清缓存。
// 用于复现「停止→重试」时 runningTask 缓存残留导致的 Start 报错。
type runningGuardInner struct {
	mu         sync.Mutex
	running    map[string]bool
	stopCalls  int
	startCalls int
}

func newRunningGuardInner() *runningGuardInner {
	return &runningGuardInner{running: map[string]bool{}}
}

func (f *runningGuardInner) Init(_ context.Context, t *framework.Task) (*framework.Task, error) {
	return t, nil
}

func (f *runningGuardInner) Start(_ context.Context, t *framework.Task) (*framework.Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.running[t.TaskId] {
		return t, false, errors.New("task " + t.TaskId + " is running")
	}
	f.running[t.TaskId] = true
	return t, false, nil
}

func (f *runningGuardInner) GetAsyncTaskStatus(_ context.Context, tasks []framework.Task) (
	[]framework.AsyncTaskStatus, error) {
	out := make([]framework.AsyncTaskStatus, 0, len(tasks))
	for _, t := range tasks {
		st := framework.TASK_STATUS_FAILED
		if f.running[t.TaskId] {
			st = framework.TASK_STATUS_RUNNING
		}
		out = append(out, framework.AsyncTaskStatus{TaskStatus: st})
	}
	return out, nil
}

func (f *runningGuardInner) GetOutput(_ context.Context, _ *framework.Task) (interface{}, error) {
	return nil, nil
}

func (f *runningGuardInner) Stop(_ context.Context, t *framework.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	delete(f.running, t.TaskId)
	return nil
}

// TestStageAffine_StartClearsStaleRunning 停止→重试必须清掉 inner 残留的 RUNNING。
//
// 根因回归：StopTask 是 handler 直接改 DB、绕过框架，inner（fucntionActuator）
// 的 runningTask 缓存里 RUNNING 残留（旧 goroutine 卡住未跑完）。重试把
// task_temp 重建为 queued、重新捞起后，inner.Start 命中残留 RUNNING 报
// 「task is running」。修复是 Start 前先 inner.Stop 清残留。
func TestStageAffine_StartClearsStaleRunning(t *testing.T) {
	orig := isTaskActive
	isTaskActive = func(_ context.Context, _ string) bool { return true }
	defer func() { isTaskActive = orig }()

	inner := newRunningGuardInner()
	a := WrapStageAffine(inner, "functional_call")

	ctx := context.Background()
	taskID := "retried-task"

	// 第一次 Start：正常起跑，inner.running = true
	if _, _, err := a.Start(ctx, &framework.Task{TaskId: taskID}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// 模拟「停止」：handler 直接改 DB，不清 inner 缓存 → inner.running 仍为 true
	// （这里什么都不做，保持 running=true）

	// 第二次 Start（重试）：修复前会报 "task is running"，修复后应先 Stop 清残留再 Start
	if _, _, err := a.Start(ctx, &framework.Task{TaskId: taskID}); err != nil {
		t.Fatalf("retry start should clear stale running state, got: %v", err)
	}

	inner.mu.Lock()
	stopCalls := inner.stopCalls
	startCalls := inner.startCalls
	running := inner.running[taskID]
	inner.mu.Unlock()

	if stopCalls < 1 {
		t.Errorf("retry start must call inner.Stop to clear stale state, stopCalls=%d", stopCalls)
	}
	if startCalls != 2 {
		t.Errorf("startCalls = %d, want 2", startCalls)
	}
	if !running {
		t.Error("after successful retry, inner should be running again")
	}
}

// TestStageAffine_WorkflowSkipsStaleClear 工作流任务 Start 前不应调 inner.Stop。
//
// 根因回归：重启恢复（recoverStaleRunningTasks 把 doing 回退到 wait）后，
// workflow 任务重新 Start 时若误调 inner.Stop（WorkflowActuator.Stop → stopSubTasks），
// 会把还在跑的节点子任务全部杀掉，driveWorkflow 见 anyStopped 连带停掉整条工作流。
// workflow actuator 无状态（无 runningTask 缓存），本就不需要这一步清理。
func TestStageAffine_WorkflowSkipsStaleClear(t *testing.T) {
	orig := isTaskActive
	isTaskActive = func(_ context.Context, _ string) bool { return true }
	defer func() { isTaskActive = orig }()

	inner := newFakeInner()
	a := WrapStageAffine(inner, "workflow")
	ctx := context.Background()

	// workflow 任务：Start 不应调 inner.Stop
	wf := framework.Task{
		TaskId:   "wf-1",
		TaskItem: &model.TaskTemp{Task: model.Task{TaskType: model.TaskTypeWorkflow}},
	}
	if _, _, err := a.Start(ctx, &wf); err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if len(inner.stopped) != 0 {
		t.Fatalf("workflow task must skip inner.Stop, got stopped=%v", inner.stopped)
	}

	// 对话任务：Start 仍应正常调 inner.Stop（清理残留运行态）
	conv := framework.Task{
		TaskId:   "conv-1",
		TaskItem: &model.TaskTemp{Task: model.Task{TaskType: model.TaskTypeConversation}},
	}
	if _, _, err := a.Start(ctx, &conv); err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	if len(inner.stopped) != 1 || inner.stopped[0] != "conv-1" {
		t.Fatalf("conversation task should call inner.Stop once, got stopped=%v", inner.stopped)
	}
}
