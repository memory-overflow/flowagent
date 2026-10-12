// Package actuator — ToolCall 阶段执行器
//
// ToolCallActuator 执行上一轮 functional_call 中 LLM 请求的工具调用
// （内置 native 工具 / MCP 工具 / command 型 skill 三类）。
//
// 单次执行流程：
//  1. 从 task_turn 读回最近一次 functional_call 的 LLM 输出，取出 tool_calls
//  2. 加载对话工具集，把 LLM 侧工具名反查回真实 server + 原始工具名
//  3. 逐个调用（每个工具一条独立 turn，便于精确回溯）
//  4. 递增 hint.ToolCallRound，状态机据此流转回 functional_call_wait 形成 loop
//
// 同步执行器：与 functional_call 保持一致。MCP 调用是阻塞请求且状态全落 DB，
// 天然支持多副本与进程重启恢复。
//
// 容错原则 —— 工具失败不等于任务失败：
// 单个工具报错（参数不对、服务不可用、工具内部异常）时，把错误信息作为工具结果
// 回喂给 LLM，让它自己决定是换参数重试、换工具，还是向用户说明。
// 这比直接判任务失败更符合 agent 的工作方式。
// 只有「一个工具都没成功进入执行」这种无法推进的情况才判失败。
package actuator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
	skillpkg "github.com/memory-overflow/flowagent/pkg/skill"
	"github.com/memory-overflow/flowagent/pkg/stream"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
	"github.com/memory-overflow/flowagent/schedule/container"

	framework "github.com/memory-overflow/light-task-scheduler"
	fact "github.com/memory-overflow/light-task-scheduler/actuator"
)

// toolCallDeps tool_call actuator 的依赖
type toolCallDeps struct {
	turnDAO      *dao.TaskTurnDAO
	toolProvider *toolProvider
	// ws 工作区管理器：执行内置 native 工具时解析任务工作目录。
	ws *workspacepkg.Manager
	// llmResolver / convDAO 用于构造图片理解回调（analyze_image 工具执行时
	// 调当前对话模型生成描述）。
	llmResolver *llmreg.Resolver
	convDAO     *dao.ConversationDAO
	// taskDAO 用于构造 deep_search 的搜索执行器（创建/轮询搜索子任务）。
	taskDAO *dao.TaskDAO
}

// toolCallRegistry 包级依赖桥，同其他 actuator 的处理方式
var (
	toolCallRegistry toolCallDeps
	toolCallOnce     sync.Once
)

// ============================================================
// 执行逻辑
// ============================================================

// toolCallImpl tool_call 阶段的实际逻辑（同步执行）
func toolCallImpl(ctx context.Context, task *framework.Task) (data interface{}, err error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return nil, fmt.Errorf("tool-call-actuator: task item is not *model.TaskTemp")
	}
	if toolCallRegistry.turnDAO == nil || toolCallRegistry.toolProvider == nil {
		return nil, fmt.Errorf("tool-call-actuator: dependencies not initialized")
	}

	log.Printf("[tool-call-actuator][%s] start (chat=%s turn_count=%d)",
		temp.TaskID, temp.ChatID, temp.TurnCount)

	// 1. 取出待执行的工具调用，同时拿回承载它们的 LLM turn 序号（幂等判断用）
	// 与模型给出的说明文本（思考过程默认折叠展示用）。
	pending, summary, llmTurnSeq, err := loadPendingToolCalls(ctx, toolCallRegistry.turnDAO, temp.TaskID)
	if err != nil {
		return nil, fmt.Errorf("tool-call-actuator: %w", err)
	}
	if len(pending) == 0 {
		// 没有待执行工具却进到本阶段，说明 functional_call 的决策与实际不一致。
		// 直接判失败而非静默跳过 —— 静默跳过会造成 loop 空转。
		return nil, fmt.Errorf("tool-call-actuator: no pending tool calls for task %s", temp.TaskID)
	}
	log.Printf("[tool-call-actuator][%s] %d tool call(s) pending: %v",
		temp.TaskID, len(pending), callNames(pending))

	// 恢复流转 hint（进程重启后内存态丢失）。
	// 任务停在 tool_call_wait 时，框架恢复执行不经过 ToSuccessStatus，
	// resolveHint 不会被触发，GetHint 返回 nil。若不在此恢复，下方
	// IncrToolCallRound 会从 0 重新计数，导致轮次编号跳回（如 22 → 1）。
	if container.GetTransitionStore().GetHint(temp.TaskID) == nil {
		phase := taskPhaseOf(temp)
		if round, err := toolCallRegistry.turnDAO.CountToolCallRoundsByPhase(ctx, temp.TaskID, phase); err == nil {
			container.GetTransitionStore().SetHint(temp.TaskID, &model.StageTransitionHint{
				MaxToolCallRound: model.DefaultMaxToolCallRound,
				ToolCallRound:    int(round),
			})
			log.Printf("[tool-call-actuator][%s] hint recovered from DB: round=%d (phase=%s)",
				temp.TaskID, round, phase)
		}
	}

	// 幂等：这批工具是否已经在上一进程执行过。
	//
	// 上次进程在「工具执行完、但轮次 hint / 状态流转」之前崩溃时，task_turn 里
	// 已经留下了这批工具的执行记录（turn_seq > 承载它们的 LLM turn 序号）。
	// 此时若再执行一遍，会产生重复的 tool_call turn，工具副作用也会重放。
	// 命中则跳过执行直接返回成功：hint 已在上方从 DB 恢复，框架据此把任务
	// 流转回 functional_call_wait，进入下一轮推理。
	if alreadyExecuted, err := toolCallRegistry.turnDAO.HasToolCallTurnAfter(ctx, temp.TaskID, llmTurnSeq); err != nil {
		return nil, fmt.Errorf("tool-call-actuator: check idempotency: %w", err)
	} else if alreadyExecuted {
		log.Printf("[tool-call-actuator][%s] idempotent skip: tool calls for llm turn #%d already executed, "+
			"skip duplicate execution and flow back to functional_call",
			temp.TaskID, llmTurnSeq)
		return nil, nil
	}

	// 2. 加载工具集用于反查
	toolSet, err := toolCallRegistry.toolProvider.Load(ctx, temp.ChatID)
	if err != nil {
		return nil, fmt.Errorf("tool-call-actuator: load tools failed: %w", err)
	}
	// deep_search 子任务：隐藏 deep_search（防递归嵌套）+ 注入 web_search（定位）
	// + 注入 fetch（抓正文精读），与 functional_call 侧保持一致。工具集里不含
	// deep_search、含 web_search 与 fetch；主 agent 不暴露 fetch。
	if isDeepSearchSubTask(temp) {
		toolSet = toolCallRegistry.toolProvider.applyDeepSearchSubTaskTools(ctx, toolSet)
	}

	// 2.5 构造图片理解回调：analyze_image 工具执行时调当前对话模型生成描述。
	// 只有模型 supports_image=1 时 analyze_image 才会出现在 toolSet 里，
	// 因此这里 resolve 出的 client 一定对应多模态模型；失败只降级（该工具
	// 调用会报错回喂 LLM），不阻断整批工具执行。
	var visionAnalyzer workspacepkg.VisionAnalyzerFunc
	if llmClient, rErr := resolveLLMClientWith(ctx, temp.ChatID, toolCallRegistry.llmResolver, toolCallRegistry.convDAO); rErr != nil {
		log.Printf("[tool-call-actuator][%s] resolve llm client for vision failed: %v", temp.TaskID, rErr)
	} else if llmClient != nil {
		visionAnalyzer = buildVisionAnalyzer(llmClient)
	}

	// 3. 并发执行。多个工具调用之间无依赖，并行可显著降低多工具场景的延迟。
	//
	// 并发安全性：BeginTurn 用事务 + turn_count 原子递增保证 turn_seq 不重复；
	// CallTool 每次独立建立 stdio 连接，无共享可变状态；toolSet/temp 只读。
	type callOutcome struct {
		call      llm.ToolCall
		succeeded bool
		infraErr  error
	}
	outcomes := make([]callOutcome, len(pending))
	var wg sync.WaitGroup
	for i := range pending {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, execErr := executeOneToolCall(ctx, temp, toolSet, pending[idx], summary, visionAnalyzer, idx)
			outcomes[idx] = callOutcome{call: pending[idx], succeeded: ok, infraErr: execErr}
		}(i)
	}
	wg.Wait()

	executed, failed := 0, 0
	for _, oc := range outcomes {
		if oc.infraErr != nil {
			// 连 turn 都没能记录成功（DB 异常），属于基础设施问题
			log.Printf("[tool-call-actuator][%s] tool %s not executed: %v",
				temp.TaskID, oc.call.Name, oc.infraErr)
			continue
		}
		if oc.succeeded {
			executed++
		} else {
			failed++
		}
	}

	// 一个都没成功进入执行（全是基础设施异常）→ 无法推进，判失败
	if executed == 0 && failed == 0 {
		return nil, fmt.Errorf(
			"tool-call-actuator: none of %d tool calls could be executed", len(pending))
	}

	store := container.GetTransitionStore()

	// 4. 更新连续失败熔断计数：按「调用指纹」（工具名+参数）逐个记账 ——
	// 同一指纹失败一次 +1、成功即清零；换工具或换参数是新的指纹、从 1 起算，
	// 不背旧账（那是模型在换方法尝试，不该被打断）。
	// 熔断在 functional_call 阶段消费（见 resolveLoopState）。
	for _, oc := range outcomes {
		if oc.infraErr != nil {
			continue
		}
		fp := toolCallFingerprint(oc.call)
		if oc.succeeded {
			store.ResetConsecutiveFailures(temp.TaskID, fp)
		} else {
			store.IncrConsecutiveFailures(temp.TaskID, fp)
		}
	}

	// 5. 递增轮次，状态机据此判断是否超出 MaxToolCallRound
	store.IncrToolCallRound(temp.TaskID)

	// 6. 死循环检测：记录本轮工具调用指纹，维护连续重复计数。
	// 计数在 functional_call 阶段消费（见 resolveLoopState 的 repeated 字段）。
	store.UpdateToolCallFingerprint(temp.TaskID, toolCallsFingerprint(pending))

	hint := store.GetHint(temp.TaskID)

	round, maxRound := 0, model.DefaultMaxToolCallRound
	if hint != nil {
		round = hint.ToolCallRound
		if hint.MaxToolCallRound > 0 {
			maxRound = hint.MaxToolCallRound
		}
	}
	log.Printf("[tool-call-actuator][%s] %d/%d tool(s) executed (%d failed), round %d/%d → functional_call",
		temp.TaskID, executed, len(pending), failed, round, maxRound)

	return nil, nil
}

// ============================================================
// 待执行工具调用的获取
// ============================================================

// loadPendingToolCalls 从最近一次成功的 LLM turn 里取出模型请求的工具调用。
//
// 为什么从 DB 读而不是内存传递：
// functional_call 与 tool_call 是两个独立调度器，中间隔着一次 DB 状态流转，
// 可能由不同副本接管。task_turn 是唯一可靠的传递媒介。
//
// 不需要区分规划期与执行期：两者的 LLM 调用 turn_type 都是 functional_call，
// 差别只在 phase 列，一次查询就能拿到真正的「上一步」。
//
// 返回的 llmTurnSeq 是承载这些工具调用的 LLM turn 序号，供调用方做幂等判断
// （判断该 LLM turn 之后是否已有 tool_call turn）。
// 返回的 summary 是模型在发起工具调用时给出的说明文本（assistant content），
// 为空表示模型未给出说明，展示层退化为「调用工具 X」。
func loadPendingToolCalls(
	ctx context.Context, turnDAO *dao.TaskTurnDAO, taskID string,
) (calls []llm.ToolCall, summary string, llmTurnSeq int32, err error) {
	turn, err := turnDAO.GetLatestLLMTurn(ctx, taskID)
	if err != nil {
		return nil, "", 0, err
	}
	if turn == nil {
		return nil, "", 0, fmt.Errorf("no llm turn found for task %s", taskID)
	}
	if turn.Status != model.TurnStatusSuccess {
		return nil, "", 0, fmt.Errorf("latest llm turn #%d (%s) is not success (status=%s)",
			turn.TurnSeq, turn.TurnPhase(), turn.Status)
	}
	if turn.Output == "" {
		return nil, "", 0, fmt.Errorf("latest llm turn #%d (%s) has empty output",
			turn.TurnSeq, turn.TurnPhase())
	}

	var out model.TurnLLMOutput
	if err := json.Unmarshal([]byte(turn.Output), &out); err != nil {
		return nil, "", 0, fmt.Errorf("unmarshal llm turn #%d output: %w", turn.TurnSeq, err)
	}

	calls = make([]llm.ToolCall, 0, len(out.ToolCalls))
	for _, tc := range out.ToolCalls {
		// submit_plan 是规划期的协议工具，由 functional_call 执行器自己拦截处理，
		// 不是真实可执行工具。若不跳过，这里会当成「未知工具」记一条失败 turn，
		// 白白污染历史并触发熔断计数。
		if tc.Name == model.SubmitPlanToolName {
			continue
		}
		calls = append(calls, llm.ToolCall{
			Name:      tc.Name,
			Arguments: tc.Arguments,
			Signature: tc.Signature,
		})
	}
	return calls, out.Text, turn.TurnSeq, nil
}

// taskPhaseOf 取任务当前的语义阶段，缺省（历史数据 / DB 默认值异常）按执行期处理，
// 与 funcCallImpl 的分派规则保持一致。
func taskPhaseOf(temp *model.TaskTemp) model.TaskPhase {
	if model.TaskPhase(temp.Phase) == model.PhasePlanning {
		return model.PhasePlanning
	}
	return model.PhaseExecuting
}

// ============================================================
// 单个工具的执行
// ============================================================

// executeOneToolCall 执行一次工具调用，并记录成一条独立 turn。
//
// fallbackSummary 是模型在 functional_call 阶段给出的伴随文本（assistant content）。
// B 方案下 summary 已成为工具 schema 的必填参数，模型会把它填在 args 里 ——
// 这里优先取 args 里的 summary，缺失时才退回伴随文本，两者都落进 turn 输入快照，
// 供思考过程默认折叠展示。
//
// 返回 error 仅代表「turn 未能记录」这类基础设施异常。
// 工具本身失败会作为 turn 的失败结果落库并回喂 LLM，此处返回 nil。
func executeOneToolCall(
	ctx context.Context,
	temp *model.TaskTemp,
	toolSet *ToolSet,
	call llm.ToolCall,
	fallbackSummary string,
	visionAnalyzer workspacepkg.VisionAnalyzerFunc,
	index int,
) (succeeded bool, err error) {
	// 从 args 提取 summary 并剥离。剥离必须在转发真实工具之前完成：
	// summary 是框架注入的元参数，MCP server 会因未知字段报错。
	argsSummary, cleanArgs := extractToolSummary(call.Arguments)
	summary := argsSummary
	if summary == "" {
		summary = fallbackSummary
	}
	call.Arguments = cleanArgs

	// 反查真实工具
	entry, found := toolSet.Lookup(call.Name)

	// 工具不存在也要记一条 turn —— 否则 LLM 收不到任何反馈，
	// 下一轮很可能重复请求同一个不存在的工具，陷入死循环。
	// 记 turn 失败才是基础设施异常；记成功则是「工具调用失败」（succeeded=false）。
	if !found {
		if recErr := recordUnknownTool(ctx, temp, call, toolSet, summary, index); recErr != nil {
			return false, recErr
		}
		return false, nil
	}

	// 按来源分发执行，但落库逻辑共享：MCP 与 Skill 都归一成
	// (content, isError, raw, execErr) 四元组，再统一 Finish/Fail。
	serverName := entryServerName(entry)
	turn, err := toolCallRegistry.turnDAO.BeginTurn(ctx, dao.BeginTurnParams{
		TaskID:       temp.TaskID,
		ChatID:       temp.ChatID,
		ParentTaskID: temp.ParentTaskID,
		TurnType:     model.TurnTypeToolCall,
		// 工具调用同样要记 phase：规划期的调研与执行期的干活都是 tool_call，
		// 只有 phase 能把两者分开（调试页与轮次预算都依赖这个区分）。
		Phase:      taskPhaseOf(temp),
		Stage:      model.TaskStage(temp.Stage),
		Round:      currentToolCallRoundForTool(ctx, temp.TaskID, taskPhaseOf(temp)),
		ToolName:   call.Name,
		ServerName: serverName,
		Input:      buildToolTurnInput(call.Name, serverName, summary, call.Arguments, index),
	})
	if err != nil {
		return false, fmt.Errorf("begin turn: %w", err)
	}

	// turn 已落库、工具尚未执行 —— 立刻广播「正在调用」事件，
	// 让前端在思考过程里实时显示「正在调用 XXX 工具…」，缓解工具执行耗时
	// 较长（如 deep_search 内部多轮检索、MCP 慢调用）时用户只能干等的体验。
	publishToolCalling(temp.TaskID, turn.ID, turn.TurnSeq, call.Name)

	// 把当前 turn.ID 透传给 deep_search 子任务；写入子任务 TaskInput.Data.ParentTurnID，
	// 调试页就能把子任务入口直接挂在对应的 tool_call 行上。
	deepSearchRunner := buildDeepSearchRunner(toolCallRegistry.taskDAO, temp, turn.ID)
	// 网页抓取回调：只给 deep_search 子任务注入 —— 它的 web_search 会据此自动
	// 抓取前几条结果的正文（见 workspacepkg.WebSearchTool 的 AutoFetchTopN）。
	// 其他任务不暴露 fetch 工具，回调保持 nil，web_search 行为不变。
	var pageFetcher workspacepkg.PageFetcherFunc
	if isDeepSearchSubTask(temp) {
		pageFetcher = buildPageFetcher(toolSet)
	}

	// 历史取回回调：把当前 turn.ID 透传进去，取回过程中的 LLM 调用（按 task_id
	// 提取时可能有多次）会记进 turn 快照的 parent_turn_id，调试页据此归组。
	contextRecaller := buildContextRecaller(recallDeps{
		taskDAO: toolCallRegistry.taskDAO,
		turnDAO: toolCallRegistry.turnDAO,
	}, temp, turn.ID)

	// 主 agent 抓网页的护栏（见 fetch_quota.go 的包注释）：
	// deep_search 子任务是专门干检索的，它的多轮抓取是本配额要保护的对象之外，
	// 因此只对「主 agent / DAG 节点子任务」计数与限长。
	// 配额拦截发生在 turn 已落库之后 —— 这样超配额那次调用也会在调试页留痕，
	// 用户能看到「模型想抓第 N 次被拦下了」，而不是凭空少了一次工具调用。
	if isTopLevelFetch(entry, temp) {
		if used, allowed := takeFetchQuota(temp.TaskID); !allowed {
			log.Printf("[tool-call-actuator][%s] fetch quota exceeded (used=%d), reject",
				temp.TaskID, used)
			msg := fetchQuotaExceededMessage()
			if ferr := toolCallRegistry.turnDAO.FinishTurn(ctx, turn.ID, dao.FinishTurnParams{
				Output: buildToolTurnOutput(msg, false, nil),
			}); ferr != nil {
				log.Printf("[tool-call-actuator][%s] finish turn %d error: %v",
					temp.TaskID, turn.ID, ferr)
			}
			publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)
			return true, nil
		}
		clampFetchMaxLength(call.Arguments)
	} else if isBuiltinFetchEntry(entry) {
		// deep_search 子任务：不占主 agent 的次数配额，但同样要限长 ——
		// 它一轮能并行抓多个页面，无限长时单次可返回 200KB 级正文。
		clampFetchMaxLengthTo(call.Arguments, fetchMaxLengthDeepSearch)
	}

	content, isError, raw, execErr := runToolEntry(ctx, entry, call, resolveWorkspaceDir(temp),
		resolveTaskArtifactDir(temp),
		visionAnalyzer, deepSearchRunner, pageFetcher, contextRecaller)
	if execErr != nil {
		msg := fmt.Sprintf("工具调用失败: %v", execErr)
		if ferr := toolCallRegistry.turnDAO.FailTurn(ctx, turn.ID, execErr,
			buildToolTurnOutput(msg, true, nil)); ferr != nil {
			log.Printf("[tool-call-actuator][%s] fail turn %d error: %v", temp.TaskID, turn.ID, ferr)
		}
		publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)
		log.Printf("[tool-call-actuator][%s] turn #%d %s failed: %v",
			temp.TaskID, turn.TurnSeq, call.Name, execErr)
		return false, nil
	}

	// 业务错误（仅 MCP 工具可能有；skill 的失败一律走 execErr）
	if isError {
		if strings.TrimSpace(content) == "" {
			content = "工具返回错误但未提供详情"
		}
		if ferr := toolCallRegistry.turnDAO.FailTurn(ctx, turn.ID,
			fmt.Errorf("tool returned error: %s", truncate(content, 500)),
			buildToolTurnOutput(content, true, raw)); ferr != nil {
			log.Printf("[tool-call-actuator][%s] fail turn %d error: %v", temp.TaskID, turn.ID, ferr)
		}
		publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)
		log.Printf("[tool-call-actuator][%s] turn #%d %s returned business error",
			temp.TaskID, turn.TurnSeq, call.Name)
		return false, nil
	}

	// 抓取类工具的内容级限长：参数 clamp 管不住不守约的 server，
	// 这里兜住「单次抓取到底能带进来多少正文」的上限。
	if isBuiltinFetchEntry(entry) {
		content = capFetchedContent(content, fetchLengthLimitFor(temp))
	}

	// 空/近空结果标准化：检索类工具「调用成功却什么都没拿到」时，
	// 原样回喂只是一条空消息，模型不知道该换关键词还是换来源，
	// 于是继续换词再搜（实测 25.7% 的搜索返回 <100B）。
	content = normalizeLowValueToolResult(call.Name, content)

	// 成功
	if err := toolCallRegistry.turnDAO.FinishTurn(ctx, turn.ID, dao.FinishTurnParams{
		Output: buildToolTurnOutput(content, false, raw),
	}); err != nil {
		log.Printf("[tool-call-actuator][%s] finish turn %d error: %v", temp.TaskID, turn.ID, err)
	}
	publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)
	log.Printf("[tool-call-actuator][%s] turn #%d %s ok (%d chars)",
		temp.TaskID, turn.TurnSeq, call.Name, len(content))
	return true, nil
}

// entryServerName 返回工具条目在 turn 记录里的「来源名」。
// MCP 用 server 名，Skill 用 skill 名（命名空间 skill 已体现在 ToolName 里），
// 内置 native 工具默认标注 builtin；若工具声明了 Label（如 web_search 的检索引擎名），
// 则优先用 Label，便于调试页直接看出用了哪个具体来源。
func entryServerName(entry *ToolEntry) string {
	if entry == nil {
		return ""
	}
	switch entry.Kind {
	case toolKindMCP:
		if entry.MCP != nil {
			return entry.MCP.Server.ServerName
		}
	case toolKindSkill:
		if entry.Skill != nil {
			return entry.Skill.Name
		}
	case toolKindBuiltin:
		if entry.Builtin != nil && entry.Builtin.Label != "" {
			return entry.Builtin.Label
		}
		return "builtin"
	}
	return ""
}

// runToolEntry 按工具来源真正执行一次调用，返回归一化结果。
// execErr 表示基础设施层失败（连不上 / 握手失败 / 子进程非零退出 / 超时）。
// wsDir 是当前任务的工作区绝对目录，仅内置 native 工具使用（可为空）。
// artifactDir 是本任务的产出目录（相对工作区，可为空），write_file 收到
// 纯文件名时落到它下面，使产物归属按目录确定。
// deepSearchRunner 是 deep_search 工具的执行回调（可为 nil，非 deep_search 工具用不到）。
// pageFetcher 是网页正文抓取回调（可为 nil，非 deep_search 子任务用不到）。
// contextRecaller 是历史取回回调（可为 nil，非 recall_context 工具用不到）。
func runToolEntry(
	ctx context.Context, entry *ToolEntry, call llm.ToolCall, wsDir string, artifactDir string,
	visionAnalyzer workspacepkg.VisionAnalyzerFunc,
	deepSearchRunner workspacepkg.DeepSearchRunnerFunc,
	pageFetcher workspacepkg.PageFetcherFunc,
	contextRecaller workspacepkg.ContextRecallerFunc,
) (content string, isError bool, raw interface{}, execErr error) {
	switch entry.Kind {
	case toolKindMCP:
		callCtx, cancel := context.WithTimeout(ctx, mcppkg.DefaultTimeout)
		result, callErr := mcppkg.CallTool(callCtx, entry.MCP.Server, entry.MCP.OriginalName, call.Arguments)
		cancel()
		if callErr != nil {
			return "", false, nil, callErr
		}
		return materializeMCPImages(result.Content, result.Images, wsDir), result.IsError, mcppkg.SanitizeRaw(result.Raw), nil

	case toolKindSkill:
		input := skillInputFromArgs(call.Arguments)
		callCtx, cancel := context.WithTimeout(ctx, skillpkg.DefaultTimeout)
		out, execErr := skillpkg.Execute(callCtx, entry.Skill, input)
		cancel()
		if execErr != nil {
			return "", false, nil, execErr
		}
		return out, false, nil, nil

	case toolKindBuiltin:
		if entry.Builtin == nil {
			return "", false, nil, fmt.Errorf("builtin tool %s missing def", call.Name)
		}
		// 超时优先取工具自身声明的 Timeout（如 deep_search 内部要等一个完整
		// 子任务跑完，需要 15 分钟）；<=0 回落到默认 60s。
		timeout := entry.Builtin.Timeout
		if timeout <= 0 {
			timeout = workspacepkg.DefaultTimeout
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		// 项目根 = 工作区根的上一级，供内置 run_command 激活项目级 venv。
		projectRoot := ""
		if toolCallRegistry.ws != nil {
			projectRoot = filepath.Dir(toolCallRegistry.ws.Root())
		}
		wc := &workspacepkg.Context{
			WorkspaceDir:     wsDir,
			ProjectRoot:      projectRoot,
			ArtifactDir:      artifactDir,
			VisionAnalyzer:   visionAnalyzer,
			DeepSearchRunner: deepSearchRunner,
			PageFetcher:      pageFetcher,
			ContextRecaller:  contextRecaller,
		}
		out, bErr := entry.Builtin.Handler(callCtx, wc, call.Arguments)
		cancel()
		if bErr != nil {
			return "", false, nil, bErr
		}
		return out, false, nil, nil

	default:
		return "", false, nil, fmt.Errorf("unknown tool kind for %s", call.Name)
	}
}

// buildVisionAnalyzer 构造图片理解回调：analyze_image 工具执行时调用当前
// 对话的多模态模型生成图片描述。
//
// question 为空时使用默认的「全量结构化描述」提示词；非空时直接用该问题。
// 返回的 client.Chat 是阻塞调用，受上层 callCtx 的 DefaultTimeout 约束
// （analyze_image 走内置工具的 60s 超时，见 runToolEntry）。
func buildVisionAnalyzer(client llm.Client) workspacepkg.VisionAnalyzerFunc {
	return func(ctx context.Context, imageData []byte, mimeType string, question string) (string, error) {
		prompt := strings.TrimSpace(question)
		if prompt == "" {
			prompt = "请详细、完整地描述这张图片的内容：逐字转录图中出现的所有文字，并描述其中的物体、人物、场景、布局结构、图表数据等关键信息，方便后续基于你的描述继续处理。"
		}
		msg := llm.Message{
			Role:    llm.RoleUser,
			Content: prompt,
			Images:  []llm.ImagePart{{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(imageData)}},
		}
		return client.Chat(ctx, []llm.Message{msg}, llm.DefaultOptions())
	}
}

// mcpFetchToolName 框架级 MCP 抓取工具的原始工具名（mcp-server-fetch 暴露的 fetch）。
const mcpFetchToolName = "fetch"

// isTopLevelFetch 判断本次调用是否属于「主 agent 抓网页」，即要走配额护栏的那些。
//
// 三个条件缺一不可：
//  1. 工具是 MCP 的 fetch；
//  2. 来源是框架内置（ServerID 为空 —— 内置清单不进 mcp_server 表，
//     而用户显式绑定的同名 server 有 DB 主键，那是用户主动接的能力，
//     不该被框架的配额约束）；
//  3. 当前任务不是 deep_search 子任务（它的多轮抓取是设计意图，且结论会被
//     压成摘要再回到主对话，不占主对话上下文）。
func isTopLevelFetch(entry *ToolEntry, temp *model.TaskTemp) bool {
	return isBuiltinFetchEntry(entry) && !isDeepSearchSubTask(temp)
}

// isBuiltinFetchEntry 判断本次调用是否是「框架内置 MCP 的 fetch」，
// 即所有需要长度治理的抓取 —— 不论调用方是主 agent 还是 deep_search 子任务。
//
// 与 isTopLevelFetch 的区别：后者还要求「不是 deep_search 子任务」，用于配额
// （次数）护栏；长度治理对两者都要生效，故单独抽出来。
func isBuiltinFetchEntry(entry *ToolEntry) bool {
	return entry != nil && entry.Kind == toolKindMCP && entry.MCP != nil &&
		entry.MCP.OriginalName == mcpFetchToolName && entry.MCP.Server != nil &&
		entry.MCP.Server.ServerID == ""
}

// buildPageFetcher 构造网页正文抓取回调：web_search 自动抓取结果正文时调用。
//
// 走 MCP fetch（与暴露给模型的 fetch 工具同一条通道），而不是在 workspace 包里
// 另写一套抓取：fetch 的 HTML → Markdown 转换质量是现成的，无需重复实现，
// 且抓取行为（UA、robots、重试）与模型手动 fetch 完全一致，不会出现
// 「自动抓得到、手动抓不到」的割裂。
//
// 找不到 fetch 工具（MCP 未安装 / 启动失败降级）时返回 nil：web_search 自动
// 退化为只返回摘要，模型仍可像以前一样逐个 fetch，功能不缺失。
func buildPageFetcher(toolSet *ToolSet) workspacepkg.PageFetcherFunc {
	if toolSet == nil {
		return nil
	}
	var entry *ToolEntry
	for i := range toolSet.tools {
		e := &toolSet.tools[i]
		if e.Kind == toolKindMCP && e.MCP != nil &&
			(e.MCP.OriginalName == mcpFetchToolName || strings.Contains(e.MCP.ExposedName, mcpFetchToolName)) {
			entry = e
			break
		}
	}
	if entry == nil {
		log.Printf("[tool-call-actuator] page fetcher unavailable: no mcp fetch tool in toolset")
		return nil
	}
	return func(ctx context.Context, url string, maxChars int) (string, error) {
		args := map[string]interface{}{
			"url":        url,
			"max_length": maxChars,
		}
		result, err := mcppkg.CallTool(ctx, entry.MCP.Server, entry.MCP.OriginalName, args)
		if err != nil {
			return "", err
		}
		if result.IsError {
			return "", fmt.Errorf("%s", truncateRunes(result.Content, 200))
		}
		return result.Content, nil
	}
}

// resolveWorkspaceDir 计算当前任务的工作区绝对目录。
//
// 工作区 key 的选择：始终使用 ChatID（同对话内所有任务共享同一工作区），
// 让对话产物能在多轮任务、工作流子任务之间流转；之前按 TaskID /
// ParentTaskID 分目录会让新对话任务看不到此前工作流任务的产物。
func resolveWorkspaceDir(temp *model.TaskTemp) string {
	if toolCallRegistry.ws == nil || temp == nil {
		return ""
	}
	key := temp.ChatID
	if key == "" {
		// 兜底：理论上 task.chat_id NOT NULL 不会为空，但万一缺失/迁移中
		// 数据异常时不希望返回空（Manager.Dir("") 会落到 root 破坏隔离），
		// 用 TaskID 维持原本单任务隔离的行为。
		key = temp.TaskID
	}
	return toolCallRegistry.ws.Dir(key)
}

// skillInputFromArgs 从 skill 工具调用入参里取出 input 文本（经 stdin 传给子进程）。
func skillInputFromArgs(args map[string]interface{}) string {
	if args == nil {
		return ""
	}
	if v, ok := args["input"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// extractToolSummary 从工具调用入参里提取框架注入的 summary 元参数，
// 并返回剥离 summary 后的干净参数。
//
// summary 是 B 方案注入到工具 schema 的必填参数（见 tool_provider.go 的
// injectSummaryParam），它不是真实工具的参数，转发给 MCP server / 内置 handler
// 前必须剥离，否则会被当成未知字段报错。
//
// 无 summary 时返回原始 args 引用（不复制，避免无谓开销）；有 summary 时
// 返回一份去掉该 key 的新 map（原 map 不变，供死循环指纹等场景继续使用）。
func extractToolSummary(args map[string]interface{}) (summary string, clean map[string]interface{}) {
	if args == nil {
		return "", nil
	}
	v, ok := args[summaryParamKey]
	if !ok {
		return "", args
	}
	if s, ok := v.(string); ok {
		summary = strings.TrimSpace(s)
	}
	clean = make(map[string]interface{}, len(args)-1)
	for k, val := range args {
		if k != summaryParamKey {
			clean[k] = val
		}
	}
	return summary, clean
}

// recordUnknownTool 为「LLM 请求了不存在的工具」记一条失败 turn。
//
// 必须记录：这样下一轮 functional_call 重建历史时，LLM 会看到
// "该工具不存在，可用工具是 xxx" 的反馈，从而纠正自己的选择。
func recordUnknownTool(
	ctx context.Context, temp *model.TaskTemp, call llm.ToolCall, toolSet *ToolSet, summary string, index int,
) error {
	turn, err := toolCallRegistry.turnDAO.BeginTurn(ctx, dao.BeginTurnParams{
		TaskID:       temp.TaskID,
		ChatID:       temp.ChatID,
		ParentTaskID: temp.ParentTaskID,
		TurnType:     model.TurnTypeToolCall,
		Phase:        taskPhaseOf(temp),
		Stage:        model.TaskStage(temp.Stage),
		Round:        currentToolCallRoundForTool(ctx, temp.TaskID, taskPhaseOf(temp)),
		ToolName:     call.Name,
		Input:        buildToolTurnInput(call.Name, "", summary, call.Arguments, index),
	})
	if err != nil {
		return fmt.Errorf("begin turn for unknown tool: %w", err)
	}

	available := toolSet.Names()
	msg := fmt.Sprintf("工具 %q 不存在。当前可用工具: %s",
		call.Name, formatAvailableTools(available))

	if ferr := toolCallRegistry.turnDAO.FailTurn(ctx, turn.ID,
		fmt.Errorf("unknown tool: %s", call.Name),
		buildToolTurnOutput(msg, true, nil)); ferr != nil {
		log.Printf("[tool-call-actuator][%s] fail turn %d error: %v", temp.TaskID, turn.ID, ferr)
	}
	publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)
	log.Printf("[tool-call-actuator][%s] turn #%d unknown tool %s (available=%d)",
		temp.TaskID, turn.TurnSeq, call.Name, len(available))
	return nil
}

// formatAvailableTools 格式化可用工具列表，数量多时截断避免撑爆上下文
func formatAvailableTools(names []string) string {
	if len(names) == 0 {
		return "(无可用工具)"
	}
	const maxShow = 20
	if len(names) <= maxShow {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s ...(共 %d 个)", strings.Join(names[:maxShow], ", "), len(names))
}

// toolCallsFingerprint 计算一轮工具调用的指纹，用于死循环检测。
//
// 指纹 = 排序后的「工具名:参数规范化JSON」拼接。json.Marshal 对 map 的
// key 已按字典序输出，天然稳定；再按工具名排序拼接，保证同一组工具调用
// 无论返回顺序如何都得到相同指纹，避免「并发结果顺序抖动」造成误判。
//
// summary 元参数必须排除：它是模型每轮都会重写的说明文本，措辞天然抖动，
// 若计入指纹会让「同一工具重复调用」永远无法被识别成重复，死循环检测失效。
// toolCallFingerprint 单个工具调用的指纹：工具名 + 参数（剔除框架注入的
// summary 元参数后）的规范化 JSON。「同样的工具 + 同样的输入参数」即同一
// 指纹 —— 连续失败熔断按此粒度记账：同指纹反复失败说明在撞同一堵墙。
func toolCallFingerprint(c llm.ToolCall) string {
	_, clean := extractToolSummary(c.Arguments)
	args, err := json.Marshal(clean)
	if err != nil {
		args = []byte(fmt.Sprintf("%v", c.Arguments))
	}
	return c.Name + ":" + string(args)
}

func toolCallsFingerprint(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, toolCallFingerprint(c))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// currentToolCallRoundForTool 读取当前轮次用于标注 turn。
// 与 functional_call 侧保持一致：内存 hint 缺失时回落到 DB 统计（按 phase 隔离）。
func currentToolCallRoundForTool(
	ctx context.Context, taskID string, phase model.TaskPhase,
) int32 {
	if hint := container.GetTransitionStore().GetHint(taskID); hint != nil {
		return int32(hint.ToolCallRound)
	}
	round, err := toolCallRegistry.turnDAO.CountToolCallRoundsByPhase(ctx, taskID, phase)
	if err != nil {
		log.Printf("[tool-call-actuator][%s] count tool_call rounds (phase=%s) failed: %v",
			taskID, phase, err)
		return 0
	}
	return int32(round)
}

// publishTurnDone 通知 SSE 订阅者某个 turn 已落库结束。
// functional_call 与 tool_call 共用：一个 turn 从 running 变成 success/failed
// 都算「结束」，前端收到后回拉最新 turn 列表对齐。
func publishTurnDone(taskID string, turnID int64, turnSeq int32) {
	stream.Publish(stream.Event{
		Type:    stream.EventTypeTurnDone,
		TaskID:  taskID,
		TurnID:  turnID,
		TurnSeq: turnSeq,
	})
}

// publishToolCalling 通知 SSE 订阅者某个工具调用已开始执行（turn 已建、尚未完成）。
// 与 publishTurnDone 配对：前者是「开始」，后者是「结束」。前端以 turn_id 关联
// 两者，收到 turn_done 后移除对应的「正在调用」提示。
func publishToolCalling(taskID string, turnID int64, turnSeq int32, toolName string) {
	stream.Publish(stream.Event{
		Type:    stream.EventTypeToolCalling,
		TaskID:  taskID,
		TurnID:  turnID,
		TurnSeq: turnSeq,
		Data:    map[string]interface{}{"tool_name": toolName},
	})
}

// ============================================================
// 工厂
// ============================================================

// NewToolCallActuator 创建 tool_call 阶段执行器（同步）。
//
// 依赖：
//   - turnDAO       读取待执行的工具调用 + 记录每次调用的 turn
//   - convMCPDAO    查对话绑定的 MCP server
//   - mcpServerDAO  取 MCP server 配置
//   - registry      MCP 工具注册表（必须与 functional_call actuator 共享同一实例，
//     否则工具名反查会 miss）
//   - convSkillDAO  查对话绑定的 Skill（command 型作为工具执行）
//   - skillDAO      取 Skill 配置
//   - ws            工作区管理器（内置 native 工具执行时解析任务工作目录）
//   - defaultMCP    框架级 MCP server（fetch 精读，仅 deep_search 子任务注入；可空）
//   - taskDAO       任务 DAO（deep_search 创建/轮询搜索子任务用）
func NewToolCallActuator(
	turnDAO *dao.TaskTurnDAO,
	convMCPDAO *dao.ConversationMCPDAO,
	mcpServerDAO *dao.MCPServerDAO,
	registry *mcppkg.Registry,
	convSkillDAO *dao.ConversationSkillDAO,
	skillDAO *dao.SkillDAO,
	ws *workspacepkg.Manager,
	defaultMCP []*mcppkg.ServerConfig,
	llmResolver *llmreg.Resolver,
	convDAO *dao.ConversationDAO,
	modelConfigDAO *dao.ModelConfigDAO,
	taskDAO *dao.TaskDAO,
	searchConfigDAO *dao.SearchConfigDAO,
) framework.TaskActuator {
	toolCallOnce.Do(func() {
		toolCallRegistry = toolCallDeps{
			turnDAO:      turnDAO,
			toolProvider: newToolProvider(convMCPDAO, mcpServerDAO, registry, convSkillDAO, skillDAO, defaultMCP, convDAO, modelConfigDAO, searchConfigDAO, ws),
			ws:           ws,
			llmResolver:  llmResolver,
			convDAO:      convDAO,
			taskDAO:      taskDAO,
		}
	})
	actuator, err := fact.MakeFucntionActuator(toolCallImpl, nil)
	if err != nil {
		panic("create tool_call actuator: " + err.Error())
	}
	return actuator
}
