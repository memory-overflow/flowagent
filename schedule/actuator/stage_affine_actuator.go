// Package actuator — 阶段亲和性包装器
//
// 解决一个跨调度器竞态导致的偶发 "同步任务未找到"。
//
// # 问题
//
// 框架的调度流程是（task_scheduler.go:300-312）：
//
//	Actuator.Start(task)        // 先执行
//	Container.ToRunningStatus()  // 后把 DB stage 改成 doing
//
// 而状态维护线程独立轮询（updateOnce）：
//
//	Container.GetRunningTask()      // 按 DB 里 stage=doing 捞任务
//	Actuator.GetAsyncTaskStatus()   // 查执行器内存缓存
//
// 每个阶段有独立的 actuator 实例，各自维护自己的 runningTask 缓存。
// 于是存在这样的窗口：
//
//  1. planning 完成 → ToSuccessStatus 把 stage 写成 functional_call_wait
//  2. functional_call 调度器捞起它，Start() 起 goroutine 开始跑
//  3. ToRunningStatus() 把 stage 改成 functional_call_doing
//  4. functional_call 的状态维护线程 GetRunningTask() 捞到它，
//     GetAsyncTaskStatus() 查自己的缓存
//
// 步骤 4 若发生在 2 和 3 之间（DB 已 doing 但 Start 还没登记完），
// 或者上一轮查询已经把缓存删掉了（framework 的 function_actuator.go:167
// 查到非 RUNNING 状态就 Delete，是一次性读取语义），
// 就会拿不到状态 → 框架判定 "同步任务未找到" → 任务失败。
//
// # 修复
//
// 包一层：自己维护「本实例启动过哪些任务」的账本。
//   - Start 时登记
//   - GetAsyncTaskStatus 时，对账本里没有的任务返回 RUNNING 而不是 FAILED
//     —— 这些任务要么归别的阶段管，要么正处于登记窗口，
//     交给框架的 TaskTimeout 兜底，比直接判失败安全得多
//   - 内层返回非 RUNNING 时才从账本移除（与内层的一次性语义对齐）
package actuator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	framework "github.com/memory-overflow/light-task-scheduler"
	"gorm.io/gorm"
)

// stageAffineActuator 为 actuator 增加「阶段归属」判定。
type stageAffineActuator struct {
	inner framework.TaskActuator
	name  string

	mu    sync.RWMutex
	owned map[string]time.Time // taskID → 启动时刻
}

// WrapStageAffine 包装 actuator，消除跨阶段调度器的状态查询竞态。
//
// 每个阶段调度器都应该用它包一层：框架会拿 DB 里 stage=doing 的任务
// 去问本阶段的 actuator 要状态，而这些任务可能并不属于本阶段。
func WrapStageAffine(inner framework.TaskActuator, name string) framework.TaskActuator {
	return &stageAffineActuator{
		inner: inner,
		name:  name,
		owned: make(map[string]time.Time),
	}
}

func (a *stageAffineActuator) Init(ctx context.Context, task *framework.Task) (*framework.Task, error) {
	return a.inner.Init(ctx, task)
}

// isTaskActive 判断任务在调度意义上是否仍然「活着」：临时表里还在、且未终态。
//
// 抽成包级变量是为了单测可替换 —— 默认实现走 DAO，单测环境没有 DB。
var isTaskActive = func(ctx context.Context, taskID string) bool {
	db := dao.DB()
	if db == nil {
		return true // 未接 DB（单测）：无从判断，按活跃处理
	}
	var temp model.TaskTemp
	if err := db.WithContext(ctx).Where("task_id = ?", taskID).First(&temp).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false
		}
		log.Printf("[stage-affine] check task %s active failed: %v", taskID, err)
		return true // 查询失败按活跃处理：一次 DB 抖动不该让任务卡死到超时
	}
	return !temp.IsTerminal()
}

// Start 启动任务并登记到账本。
//
// 先登记再调用内层：内层 Start 会起 goroutine，若先调用内层，
// goroutine 可能在登记完成前就跑完并触发状态查询。
//
// 启动前会再验一次任务是否还活着：框架的调度顺序是「先 Start、后
// ToRunningStatus」，而停止动作（handler.StopTask）在另一条链路上直接把
// 任务置终态并删掉临时表行。两者并发时，任务可能在被捞起之后、真正执行
// 之前就已经 stopped —— 没有这道校验，它还会再跑完一次 LLM 调用才停。
func (a *stageAffineActuator) Start(ctx context.Context, task *framework.Task) (
	*framework.Task, bool, error) {
	if !isTaskActive(ctx, task.TaskId) {
		// ignoreErr=true：框架不会把它判失败（否则会覆盖掉 stopped 终态），
		// 只是安静地放弃这一轮。
		log.Printf("[%s-affine] task %s already ended before start, skip",
			a.name, task.TaskId)
		return task, true, fmt.Errorf("task %s already ended", task.TaskId)
	}

	// 清理 inner 的残留运行态。
	//
	// 停止任务是 handler 直接改 DB、绕过框架（server 与 scheduler 是不同进程，
	// handler 拿不到 scheduler 的 actuator 实例）。停止后 task_temp 被删、
	// 框架不再轮询它，inner（fucntionActuator）的 runningTask 缓存里那条
	// RUNNING 就永久残留 —— 旧 goroutine 还卡在 LLM 调用/等待里没跑完。
	// 重试把 task_temp 重建为 queued、重新捞起后，inner.Start 会命中缓存里
	// 的 RUNNING 报「task is running」。这里先 Stop 一次把残留清掉（含 cancel
	// 旧 goroutine），再 Start 才能正常起跑。
	//
	// 但这条清理只对「有 runningTask 缓存/goroutine 的 actuator」（function /
	// tool_call）有意义。workflow actuator 无状态，且它的 Stop 会 stopSubTasks
	// 停掉所有 active 子任务 —— 重启恢复（recoverStaleRunningTasks 把 doing
	// 回退到 wait）后重新 Start 时若误调 Stop，会把还在跑的节点子任务全部杀掉，
	// 进而 driveWorkflow 见 anyStopped 把整条工作流连带标 stopped。故对 workflow
	// 任务跳过这一步（它本就没有可清理的进程内残留）。
	if !isWorkflowTask(task) {
		if err := a.inner.Stop(ctx, task); err != nil {
			log.Printf("[%s-affine] clear stale running state for %s: %v",
				a.name, task.TaskId, err)
		}
	}

	a.mu.Lock()
	a.owned[task.TaskId] = time.Now()
	a.mu.Unlock()

	newTask, ignoreErr, err := a.inner.Start(ctx, task)
	if err != nil {
		// 启动失败则撤销登记，避免账本泄漏
		a.mu.Lock()
		delete(a.owned, task.TaskId)
		a.mu.Unlock()
	}
	return newTask, ignoreErr, err
}

// GetAsyncTaskStatus 查询状态，对不属于本阶段的任务返回 RUNNING。
func (a *stageAffineActuator) GetAsyncTaskStatus(
	ctx context.Context, tasks []framework.Task,
) ([]framework.AsyncTaskStatus, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	// 拆成「本阶段持有的」与「其他阶段的」，只向内层查询前者。
	// 全量透传会让内层对不认识的任务返回 FAILED（同步任务未找到）。
	mine := make([]framework.Task, 0, len(tasks))
	mineIdx := make([]int, 0, len(tasks))

	a.mu.RLock()
	for i, t := range tasks {
		if _, ok := a.owned[t.TaskId]; ok {
			mine = append(mine, t)
			mineIdx = append(mineIdx, i)
		}
	}
	a.mu.RUnlock()

	// 默认全部视为运行中。
	//
	// 对本实例没启动过的任务，宁可报 RUNNING 也不能报 FAILED：
	//   - 它可能归别的阶段调度器管（DB stage 是全局可见的）
	//   - 也可能正处于 Start 与 ToRunningStatus 之间的登记窗口
	// 真正卡死的任务由框架的 TaskTimeout 收口，代价只是多等一个超时周期，
	// 而误判失败会直接丢掉一个正常任务。
	result := make([]framework.AsyncTaskStatus, len(tasks))
	for i := range result {
		result[i] = framework.AsyncTaskStatus{
			TaskStatus: framework.TASK_STATUS_RUNNING,
			Progress:   0.0,
		}
	}

	if len(mine) == 0 {
		return result, nil
	}

	innerStatus, err := a.inner.GetAsyncTaskStatus(ctx, mine)
	if err != nil {
		return nil, err
	}
	if len(innerStatus) != len(mine) {
		log.Printf("[%s-affine] inner returned %d status for %d tasks, treat as running",
			a.name, len(innerStatus), len(mine))
		return result, nil
	}

	for k, st := range innerStatus {
		result[mineIdx[k]] = st
		// 内层查到终态后会删自己的缓存（一次性语义），
		// 账本同步移除，保持两边一致
		if st.TaskStatus != framework.TASK_STATUS_RUNNING {
			a.mu.Lock()
			delete(a.owned, mine[k].TaskId)
			a.mu.Unlock()
		}
	}
	return result, nil
}

func (a *stageAffineActuator) GetOutput(ctx context.Context, task *framework.Task) (interface{}, error) {
	return a.inner.GetOutput(ctx, task)
}

// isWorkflowTask 判断任务是否为工作流任务（TaskTypeWorkflow）。
//
// workflow actuator 无状态（无 runningTask 缓存/goroutine），Start 前无需
// 「清理残留运行态」；且它的 Stop 会 stopSubTasks 停掉所有 active 子任务，
// 若在重启恢复重新 Start 时误调，会把还在跑的节点子任务杀掉，进而
// anyStopped 级联拖停整条工作流。故这里单独识别出来跳过。
func isWorkflowTask(task *framework.Task) bool {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return false
	}
	return temp.TaskType == model.TaskTypeWorkflow
}

func (a *stageAffineActuator) Stop(ctx context.Context, task *framework.Task) error {
	a.mu.Lock()
	delete(a.owned, task.TaskId)
	a.mu.Unlock()
	return a.inner.Stop(ctx, task)
}
