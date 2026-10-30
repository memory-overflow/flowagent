// Package schedule — Stage 调度器工厂
//
// 基于 light-task-scheduler 的 MakeScheduler 为每个 stage 创建独立的调度器实例。
//
// 与硬编码流转路径的方式不同，本实现采用**动态状态流转**：
//   - 容器只需绑定自己负责扫描的 wait 阶段
//   - 下一阶段由 model.TaskStage.NextStage(hint) 状态机计算
//   - 执行器通过 StageTransitionStore 写入决策 hint，容器读取后决定流转方向
//   - 任何阶段失败 → 任务统一变为 TaskStatusFailed 终态
//
// 执行路径（由状态机自动分支，无需在调度器中配置）：
//
//	functional_call → [tool_call → functional_call]×N → finished
package schedule

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
	"github.com/memory-overflow/flowagent/schedule/actuator"
	"github.com/memory-overflow/flowagent/schedule/container"

	framework "github.com/memory-overflow/light-task-scheduler"
)

// ============================================================
// StageScheduler — 所有 stage 调度器的管理器
// ============================================================

// StageScheduler 管理所有 stage 的调度器实例，控制生命周期。
type StageScheduler struct {
	ctx    context.Context
	cancel context.CancelFunc

	// schedulers 各阶段调度器，key 为阶段名称
	schedulers map[string]*framework.TaskScheduler
}

// schedulePollInterval 所有调度器的轮询间隔（任务扫描 + 状态维护）。
//
// 取 1s 是延迟与 DB 压力的折中：agent loop 一次完整交互要串起
// functional_call → (tool_call → functional_call)×N 多个阶段，
// 每个阶段都要等一次调度轮询，间隔放大后用户感知的等待会成倍累积。
//
// DB 压力：每个调度器每轮询周期发 3 条查询（GetRunningTaskCount /
// GetWaitingTask / GetRunningTask），2 个阶段合计约 6 QPS，可以接受。
const schedulePollInterval = 1 * time.Second

// 各阶段任务超时（light-task-scheduler 的任务级看门狗，超时即判失败并强杀）。
//
// 关键点：TaskTimeout 是「单次进入 doing 阶段」的超时，不是任务生命周期。
// 任务每流转一次 functional_call ↔ tool_call 都会重设 start_time，
// 因此这里约束的是「单次 LLM 调用 / 单次工具调用」的耗时上限。
const (
	// functionalCallTimeout 对话任务单次 LLM 调用超时。
	//
	// 对话任务虽以「短问答」为主，但执行期可能承接两类重活：
	//   - 用户追问「文章写得怎么样了」等，要求把历史工作流的完整产物（数千字全文）
	//     重新输出，一次 completion 的生成时间不再秒级；
	//   - 规划期绑定大工作流上下文后，LLM 首包 + 生成都会变慢。
	// 曾多次出现执行期 turn total_tokens=0 却在 3 分钟被看门狗强杀（任务3m0s超时）。
	// 故放宽到与工作流子任务一致，避免误杀，卡死任务仍由看门狗兜底。
	functionalCallTimeout = 15 * time.Minute

	// workflowSubTaskTimeout 工作流子任务单次 LLM 调用超时。
	//
	// 子任务常被用于「一次性生成完整大文件」（如整份 HTML 动画：SVG+CSS+JS），
	// 单次 completion 可能远超 3 分钟（曾实测 gpt-5.6-sol 生成整份动画 >3min
	// 被 3 分钟看门狗强杀）。故给更长上限，专供这类重活。
	workflowSubTaskTimeout = 15 * time.Minute

	// toolCallTimeout 工具调用阶段超时（对话任务与子任务共用）。
	// 单个工具内部另有各自超时（native 工具默认 60s、MCP 工具自限），
	// 但 deep_search 这类「内部要跑一个完整搜索子任务」的重活工具单次调用
	// 可长达 15 分钟（deep_search 的 ToolDef.Timeout），本看门狗必须放宽到
	// 不小于该值，否则子任务还在跑、外层 tool_call 已被强杀。故统一 15 分钟。
	toolCallTimeout = 15 * time.Minute
)

// stageSchedulerSpec 单个 stage 调度器的规格定义
type stageSchedulerSpec struct {
	// name 调度器名称
	name string
	// waitStage 本调度器负责扫描的 wait 阶段
	waitStage model.TaskStage
	// actuator 执行器
	actuator framework.TaskActuator
	// timeout 任务超时时间
	timeout time.Duration
	// limit 并发限制
	limit int32
	// maxRetries 最大重试次数（0 表示不重试，失败即终态）
	maxRetries int32
	// taskType 若非 nil，本调度器只处理该类型的任务（见容器的 taskTypeFilter）。
	// 用于把同一 stage 按任务类型拆成多个调度器实例（不同超时）。
	taskType *model.TaskType
}

// NewStageScheduler 创建所有 stage 调度器并启动。
//
// 依赖：
//   - cfg 提供 LLM 网关配置
func NewStageScheduler(cfg *config.Config) (*StageScheduler, error) {
	if cfg == nil {
		return nil, fmt.Errorf("NewStageScheduler: nil config")
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &StageScheduler{
		ctx:        ctx,
		cancel:     cancel,
		schedulers: make(map[string]*framework.TaskScheduler),
	}

	// 进程重启恢复：把上次运行遗留的 doing+running 任务回退到 wait+queued，
	// 让它们立即重新进入调度队列，而不是等 TaskTimeout 判「超时失败」。
	recoverStaleRunningTasks(ctx, time.Now())

	// 构建 LLM 客户端解析器：按对话的 model_id 动态选模型，空/缺省回退 config.LLM。
	//
	// 模型配置来自 DB 的 model_config 表（前端「模型」页增删改）；config.LLM
	// 仅作为「DB 里没有任何模型」时的兜底，保证未做前端配置的部署照常运行。
	modelConfigDAO := &dao.ModelConfigDAO{}
	llmResolver := llmreg.NewResolverWithFallbackSpec(&modelConfigLoader{dao: modelConfigDAO}, fallbackModelSpec(cfg))
	log.Printf("[stage-scheduler] llm resolver ready (fallback provider=%s model=%s)",
		cfg.LLM.Provider, cfg.LLM.Model)

	// 各 actuator 的共享依赖
	taskDAO := &dao.TaskDAO{}
	convDAO := &dao.ConversationDAO{}
	turnDAO := &dao.TaskTurnDAO{}
	convMCPDAO := &dao.ConversationMCPDAO{}
	mcpServerDAO := &dao.MCPServerDAO{}
	convSkillDAO := &dao.ConversationSkillDAO{}
	skillDAO := &dao.SkillDAO{}
	searchConfigDAO := &dao.SearchConfigDAO{}

	// MCP 工具注册表。functional_call 与 tool_call 共享同一实例，
	// 保证前者暴露给 LLM 的工具名后者能反查到。
	toolRegistry := mcppkg.NewRegistry(mcppkg.DefaultCacheTTL)

	// 工作区管理器：内置 native 文件工具的执行沙箱根目录。
	wsManager := workspacepkg.NewManager(cfg.Workspace.Root)

	// 框架级 MCP server：联网抓取（web_fetch）走 mcp-server-fetch，以取得
	// HTML→Markdown 结构化、charset 转码、robots 合规等完整抓取效果。
	//
	// 主 agent 与 deep_search 子任务都会注入它（子任务额外注入 web_search 做
	// 两段式检索）。主 agent 侧的抓取受配额与长度上限约束，见 fetch_quota.go ——
	// 之所以给主 agent，是因为不给它就会退回 run_command + curl，更不可控。
	// 清单缺失 / 加载失败时降级为空（仅 native 工具），不阻断调度器启动。
	var defaultMCP []*mcppkg.ServerConfig
	if fetch := mcppkg.LoadBuiltinServer(mcpBuiltinsDir(), "fetch"); fetch != nil {
		defaultMCP = append(defaultMCP, fetch)
		log.Printf("[stage-scheduler] fetch MCP loaded (main agent + deep_search subtask): %s",
			fetch.ServerName)
	} else {
		log.Printf("[stage-scheduler] fetch MCP manifest not found, fetch disabled")
	}

	// functional_call 执行器工厂：对话任务与工作流子任务共用同一套 funcCallImpl
	// （内部按 task_type 分派到不同的 prompt 组装），但各自持有独立实例，
	// 避免 runningTask 缓存与阶段亲和账本在两个调度器之间串扰。
	newFuncCallActuator := func() framework.TaskActuator {
		return actuator.NewFuncCallActuator(
			llmResolver, taskDAO, convDAO, turnDAO, convMCPDAO, mcpServerDAO, toolRegistry,
			convSkillDAO, skillDAO, wsManager, defaultMCP, modelConfigDAO, searchConfigDAO,
		)
	}

	// task_type 分流指针：functional_call 阶段拆成两个调度器实例（见下）。
	conversationType := model.TaskTypeConversation
	subTaskType := model.TaskTypeWorkflowSubTask

	// 各阶段调度器规格。
	// 每个规格只需声明自己负责扫描的 wait 阶段，
	// 流转方向由 model.TaskStage.NextStage 状态机动态决策。
	specs := []stageSchedulerSpec{
		{
			// 对话任务的 functional_call 调度器：短超时，快速问答节奏。
			name:      "functional_call",
			waitStage: model.StageFunctionalCallWait,
			actuator:  newFuncCallActuator(),
			timeout:   functionalCallTimeout,
			limit:     10,
			// LLM 调用失败（网络抖动 / 网关超时 / 5xx）自动重试 3 次再判失败。
			// 框架按 MaxFailedAttempts 在 actuator 返回 error 后重新 Start；
			// 失败的 functional_call turn 不进入对话历史（turnToMessage 过滤非 success），
			// 因此重试不会污染上下文。
			maxRetries: 3,
			taskType:   &conversationType,
		},
		{
			// 工作流子任务的 functional_call 调度器：与上面的调度器扫描同一个
			// functional_call 阶段，靠 task_type 分流，赋予更长的单次 LLM 调用
			// 超时 —— 子任务常被用于一次性生成完整大文件（如整份 HTML 动画），
			// 单次 completion 可能远超 3 分钟。
			name:      "functional_call_subtask",
			waitStage: model.StageFunctionalCallWait,
			actuator:  newFuncCallActuator(),
			timeout:   workflowSubTaskTimeout,
			limit:     10,
			// 与对话任务一致：子任务单次 LLM 调用失败同样重试 3 次。
			maxRetries: 3,
			taskType:   &subTaskType,
		},
		{
			name:      "tool_call",
			waitStage: model.StageToolCallWait,
			actuator: actuator.NewToolCallActuator(
				turnDAO, convMCPDAO, mcpServerDAO, toolRegistry, convSkillDAO, skillDAO, wsManager, defaultMCP,
				llmResolver, convDAO, modelConfigDAO, taskDAO, searchConfigDAO,
			),
			timeout:    toolCallTimeout,
			limit:      10,
			maxRetries: 0, // 工具失败已作为结果回喂 LLM，无需框架层重试
		},
		{
			// 工作流编排调度器：扫描 StageWorkflowWait，把 DAG 节点展开为
			// 单 Agent 子任务并按 depends_on 驱动推进（见 workflow_actuator.go）。
			//
			// timeout 设长：工作流要串起多个子任务，单个子任务本身又受
			// functional_call(15min)/tool_call(3min) 各自的超时约束，父任务只需
			// 兜住「子任务永远不结束」的异常，不参与正常推进节奏。
			name:       "workflow",
			waitStage:  model.StageWorkflowWait,
			actuator:   actuator.NewWorkflowActuator(taskDAO, llmResolver, convDAO),
			timeout:    2 * time.Hour,
			limit:      10,
			maxRetries: 0, // 子任务失败即工作流失败，不在编排层重试
		},
	}

	for _, spec := range specs {
		// 容器只绑定 wait 阶段，doing 阶段自动推导，下一阶段动态计算。
		// spec.taskType 为 nil 时不限类型，非 nil 时按 task_type 分流。
		taskContainer := container.NewStageTaskContainer(spec.waitStage, spec.name, spec.taskType)

		config := framework.Config{
			TaskTimeout:       spec.timeout,
			TaskLimit:         spec.limit,
			MaxFailedAttempts: spec.maxRetries,
			// 两个轮询都用 1s：agent loop 一次交互要跨多个阶段
			// （functional_call → tool_call → functional_call ...），
			// 每个阶段都要等一次调度 + 一次状态维护，间隔越大累积等待越明显。
			SchedulingPollInterval: schedulePollInterval,
			StatePollInterval:      schedulePollInterval,
			DisableStatePoll:       false,
			EnableStateCallback:    false,
		}

		// 包一层阶段亲和性：框架会拿 DB 里所有 stage=doing 的任务
		// 来问本阶段 actuator 要状态，但这些任务可能归别的阶段管
		// （DB stage 全局可见，而 actuator 缓存是每实例独立的）。
		// 不包装会偶发 "同步任务未找到" 导致任务被误判失败。
		affineActuator := actuator.WrapStageAffine(spec.actuator, spec.name)

		scheduler, err := framework.MakeScheduler(taskContainer, affineActuator, nil, config)
		if err != nil {
			s.closeAll()
			cancel()
			return nil, fmt.Errorf("create %s scheduler: %w", spec.name, err)
		}

		s.schedulers[spec.name] = scheduler
		typeLabel := "any"
		if spec.taskType != nil {
			typeLabel = spec.taskType.String()
		}
		log.Printf("[stage-scheduler] %s scheduler started (wait=%s, doing=%s, limit=%d, timeout=%v, type=%s)",
			spec.name, spec.waitStage, spec.waitStage.ToDoingStage(), spec.limit, spec.timeout, typeLabel)
	}

	log.Printf("[stage-scheduler] all %d stage schedulers started", len(s.schedulers))
	return s, nil
}

// Close 停止所有调度器
func (s *StageScheduler) Close() {
	s.closeAll()
	s.cancel()
	log.Printf("[stage-scheduler] all schedulers stopped")
}

// mcpBuiltinsDir 返回内置 MCP 清单目录，与 cmd/scheduler、handler 层保持同一约定：
// 优先读 env MCP_BUILTINS_DIR，否则回落到相对工作目录的 ./mcp/builtins。
func mcpBuiltinsDir() string {
	if v := os.Getenv("MCP_BUILTINS_DIR"); v != "" {
		return v
	}
	return "./mcp/builtins"
}

// closeAll 关闭已启动的调度器
func (s *StageScheduler) closeAll() {
	for name, scheduler := range s.schedulers {
		if scheduler != nil {
			scheduler.Close()
			log.Printf("[stage-scheduler] %s scheduler closed", name)
		}
	}
}

// GetScheduler 获取指定阶段的调度器
func (s *StageScheduler) GetScheduler(name string) *framework.TaskScheduler {
	return s.schedulers[name]
}

// recoverStaleRunningTasks 进程启动时，把上次运行遗留的 doing+running 任务
// 回退到 wait+queued，让它们重新进入调度队列。
//
// 为什么需要：runningTask 缓存是进程内存态（framework.fucntionActuator.runningTask，
// 以及 stageAffineActuator.owned 账本）。进程重启后这些内存态清空，但 DB 里
// 遗留的 doing+running 任务不会被任何调度器接管：
//
//	stageAffineActuator.GetAsyncTaskStatus 对本实例没启动过的任务一律报 RUNNING
//	（宁可等超时也不误判失败），于是这些任务只能干等到 TaskTimeout
//	（functional_call 15 分钟）被 updateOnce 的 RUNNING 分支判「任务超时失败」。
//	而超时分支直接 s.failed() 收口、不重试（重试逻辑只在 FAILED 状态分支），
//	任务因此彻底丢失（attempt_times 保持 0，turn_count 保持 0）。
//
// 与其让用户多等一个超时周期再看到「任务超时失败」，不如启动时把这些遗留任务
// 回退到 wait 队列，让它们立刻被重新调度。重跑是幂等的：functional_call /
// tool_call / workflow 各阶段都从 task_turn 重建上下文，重复执行不会产生脏数据。
func recoverStaleRunningTasks(ctx context.Context, startedAt time.Time) {
	db := dao.DB().WithContext(ctx)

	doingStages := make([]int, 0, len(model.AllDoingStages()))
	for _, s := range model.AllDoingStages() {
		doingStages = append(doingStages, int(s))
	}

	var temps []model.TaskTemp
	if err := db.Where("status = ?", model.TaskStatusRunning).
		Where("stage IN ?", doingStages).
		Find(&temps).Error; err != nil {
		log.Printf("[stage-scheduler] recover stale running tasks: query error: %v", err)
		return
	}
	if len(temps) == 0 {
		return
	}

	now := time.Now()
	recovered := 0
	for i := range temps {
		t := temps[i]

		// 多副本保护：只重置「本进程启动之前」就进入执行的任务。
		// 本进程启动后才开始的任务可能属于其他存活副本，不应打断。
		// 进入执行时间优先取 stage_start_time（单次进入 doing），老数据无该列时回落到 start_time。
		enteredAt := t.StageStartTime
		if enteredAt == nil {
			enteredAt = t.StartTime
		}
		if enteredAt != nil && enteredAt.After(startedAt) {
			continue
		}

		waitStage := model.TaskStage(t.Stage).ToWaitStage()
		updates := map[string]interface{}{
			"stage":            int(waitStage),
			"status":           model.TaskStatusQueued,
			"stage_start_time": nil, // 看门狗重新计时
			"updated_time":     now,
		}
		if err := db.Model(&model.TaskTemp{}).Where("task_id = ?", t.TaskID).Updates(updates).Error; err != nil {
			log.Printf("[stage-scheduler] recover task %s: update task_temp error: %v", t.TaskID, err)
			continue
		}
		if err := db.Model(&model.Task{}).Where("task_id = ?", t.TaskID).Updates(updates).Error; err != nil {
			log.Printf("[stage-scheduler] recover task %s: sync task error: %v", t.TaskID, err)
		}

		// 清理该任务卡在 running 的孤儿 turn：上一进程退出时没机会回填状态，
		// 这些 turn 会永远停在 running（end_time 为空），前端一直显示「执行中」。
		// 收口为 interrupted 后前端才能正确展示，也避免干扰轮数统计。
		turnDAO := &dao.TaskTurnDAO{}
		if n, err := turnDAO.MarkRunningTurnsInterrupted(ctx, t.TaskID); err != nil {
			log.Printf("[stage-scheduler] recover task %s: mark running turns interrupted error: %v",
				t.TaskID, err)
		} else if n > 0 {
			log.Printf("[stage-scheduler] recover task %s: marked %d running turn(s) interrupted", t.TaskID, n)
		}

		recovered++
		log.Printf("[stage-scheduler] recovered stale running task %s: %s → %s (requeued)",
			t.TaskID, model.TaskStage(t.Stage), waitStage)
	}
	if recovered > 0 {
		log.Printf("[stage-scheduler] recovered %d stale running tasks", recovered)
	}
}
