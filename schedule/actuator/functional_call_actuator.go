// Package actuator — FunctionalCall 阶段执行器
//
// FuncCallActuator 是 agent loop 的核心：调 LLM，让它决定「继续调工具」还是「收口」。
//
// 一个 stage，两种语义阶段（由 task.phase 决定，见 model.TaskPhase）：
//
//	规划期 PhasePlanning  ：prompt 是规划器人格，工具 = 真实工具 + submit_plan 协议工具。
//	                        模型可以先调真实工具做调研，调研够了再调 submit_plan 提交规划；
//	                        执行器拦截 submit_plan → 落库 plan → 切 phase 到执行期。
//	执行期 PhaseExecuting ：prompt 是通用 agent 人格，工具 = 真实工具。
//	                        模型返回 tool_calls 则继续循环，返回纯文本即最终答案。
//
// 为什么规划不再是独立 stage：规划本身也需要调工具（查资料、看现状）才能拆得准。
// 做成独立 stage 就得为它再复制一套 functional_call ↔ tool_call 循环；
// 降级成 functional_call 阶段内的一个 phase，两者天然共享同一套循环。
//
// 单次执行流程（两个 phase 共用骨架）：
//  1. 从 task_turn 重建对话历史（含此前所有 LLM 输出与工具结果）
//  2. 加载对话绑定的 MCP / Skill 工具清单
//  3. 调 LLM
//  4. 记录 turn，按结果写状态机 hint：
//     · 规划期提交了规划   → hint.PlanSubmitted=true → 回到 functional_call（已切执行期）
//     · LLM 返回 tool_calls → hint.NeedToolCall=true  → 流转到 tool_call 阶段
//     · LLM 返回纯文本      → hint.NeedToolCall=false → 流转到 finished 阶段
//
// 同步执行器：LLM 调用本身是阻塞的 HTTP 请求，没有异步回调机制，
// 因此直接用 MakeFucntionActuator 包装，让框架在独立 goroutine 里跑。
// 这样也省掉了自己维护内存态 —— 状态完全落在 DB，天然支持多副本与重启恢复。
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
	skillpkg "github.com/memory-overflow/flowagent/pkg/skill"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
	"github.com/memory-overflow/flowagent/schedule/container"

	framework "github.com/memory-overflow/light-task-scheduler"
	fact "github.com/memory-overflow/light-task-scheduler/actuator"
)

// funcCallDeps functional_call actuator 的依赖
type funcCallDeps struct {
	llmResolver *llmreg.Resolver
	// taskDAO 写最终答案 / 规划快照 / phase 切换
	taskDAO *dao.TaskDAO
	// conversationDAO 规划期读写对话级累积 workflow
	conversationDAO *dao.ConversationDAO
	turnDAO         *dao.TaskTurnDAO
	toolProvider    *toolProvider
	// ws 工作区管理器：任务收口时扫描产物（output_files）与转存过长回复。
	// 与 tool_call actuator 共用同一个 root，保证「工具写入的目录」
	// 与「收口时扫描的目录」是同一个。
	ws *workspacepkg.Manager
}

// funcCallRegistry 包级依赖桥。
// fact.MakeFucntionActuator 只接受纯函数签名，无法直接携带依赖，用包级变量作桥。
var (
	funcCallRegistry funcCallDeps
	funcCallOnce     sync.Once
)

// ============================================================
// 提示词
// ============================================================

const funcCallSystemPrompt = `你是一个能够使用工具的智能助手。

工作方式：
1. 分析用户需求，判断是否需要调用工具获取信息或执行操作。
2. 需要工具时，直接发起工具调用；你会收到工具的执行结果。
3. 拿到工具结果后，可以继续调用其他工具，也可以基于已有信息给出最终回答。
4. 信息足够时，用清晰、准确的自然语言给出最终答案，不要再调用工具。

工具调用说明（重要）：
- 每个工具都有一个必填的 summary 参数：发起工具调用时，必须在 summary 里写一句话
  简要说明你接下来要做什么（例如「我先搜索一下相关资料」「我读取该文件的内容」）。
- summary 会展示给用户，让它理解你的行动意图；请写得具体、口语化，
  不要只重复工具名（尤其 run_command 这类，要说明执行的是什么命令、达到什么目的）。

能查就别问（重要）：
- 凡是可以用工具查到的客观信息（事实、数据、文件内容、网页资料、代码现状等），必须先调用工具查证，不要因为"信息不足"就反过来向用户提问。
- 用户输入可能有简称、错别字或模糊指代，先用工具（搜索、抓取、读文件等）确认它指什么，而不是让用户重说一遍。
- 只有工具无法获取的「主观意图与偏好」（如预算、风格取向、交付形式）才向用户确认。

联网获取信息（重要）：
- 需要联网查资料时，优先用 deep_search：它会在一个专门的搜索子任务里完成
  「搜索 → 抓取 → 交叉综合」，只把收口后的结论返回给你。这既比你自己逐条抓更全面，
  也不会让大量网页正文灌进主对话上下文。
- 只有一种情况才直接用 fetch：你已经拿到了明确的具体 URL，且需要的正是这个页面的
  原文细节（如读某份文档、核对某个接口的返回结构）。抓之前先问自己一句
  「这个信息是不是非看原文不可」—— 能靠 deep_search 的结论或已有知识回答的，就不要抓。
- fetch 有配额限制（每个任务次数有限），用完会被拒绝；此时不要再重试 fetch，
  改用 deep_search 或基于已有信息继续。
- 严禁用 run_command 执行 curl / wget / python requests 等命令去抓网页或调接口：
  抓取一律走 fetch / deep_search 这两条受控通道（有 Markdown 转换、长度上限与配额）。
  绕过它们既不受控，也拿不到结构化结果。

注意事项：
- 不要臆造工具返回的内容，只根据实际结果作答。
- 如果工具执行失败，向用户说明情况，不要假装成功。
- 如果没有可用工具，或工具都不适用，直接基于自身知识回答。
- 最终回答要完整、直接，不要输出"我将要做什么"这类过程性描述。

输出形态：优先文本，必要时才落盘（重要）：
- 默认直接把结论写在回复文本里。用户要的是答案本身，多一次「打开文件」的动作就多一层摩擦，
  而且纯文本能被下游步骤与后续对话直接引用。
- 只有在**正文确实长到不适合直接回复**时（预计超过 4000 字，如成篇的文章、报告、方案、
  完整代码文件、大数据表格），才先用 write_file 落盘。
- 不要为了「显得正式」而把本来几百字能说清的结论写成文件；也不要把一个完整交付物
  拆成一堆小文件让用户逐个点开。

落盘时的回复契约（重要）：
- 回复仍然要给出**中等详细度的结论**，而不是一句「已完成，见文件」。
  标准：用户只读你的回复，就能拿到关键结论、核心数据、主要建议与依据要点
  （通常 500~1500 字，含必要的结构化要点或小表）。
- 回复末尾再给出文件路径与全文规模，让用户知道去哪里看完整版。
- 反例：write_file 写完一万字报告后，只回「报告已生成，见 report.md」——
  用户什么结论都没拿到，必须再打开一个文件。
- 正例：回复里先给出「结论：推荐 A 方案，理由是……；三个候选的对比要点：……；
  主要风险：……；全文 8000 字见 report.md」。
- 文件命名要有意义（如 article-ai-trends.md、report-q3.md），并选好扩展名
  （文章/报告用 .md，代码用对应语言后缀，数据用 .csv/.json）。
- 需要多个文件时（如代码项目）分别落盘，回复里列出文件清单。

输出位置（重要）：
- 所有产出文件 —— 中间产物与最终交付物 —— 一律写到工作区目录，
  用相对路径传给 write_file / run_command，不要使用绝对路径。
- 区分「最终交付物」与「中间产物」的存放目录：
  · 最终交付物（用户真正要的文章、报告、清单、代码、数据表格等）写到工作区根目录
    或 outputs/ 目录，它们会进入交付清单。
  · 中间产物（探测/OCR/像素统计等调试脚本、裁剪/缩放/阈值化等调试图片、临时输出）
    一律写到 .scratch/ 子目录。.scratch/ 里的文件不会进入交付清单，仅供任务过程中
    自己或下游步骤临时使用。
- run_command 执行的命令/脚本，工作目录就是当前任务的工作区；不显式指定输出路径时，
  输出默认落在工作区，这正是期望行为。
- 严禁在命令或脚本里硬编码 /Users/xxx/Desktop、C:\Users\xxx 这类本机绝对路径，
  否则产物会散落到工作区之外，用户无法在产物列表里看到或下载。

文件下载链接（重要）：
- 回复里给用户文件下载链接时，只能用以下两种形式之一：
  · 工作区相对路径（如 outputs/xxx/report.md）—— 前端会自动转成可下载链接；
  · 完整下载地址 /api/v1/files/download?chat_id=<chat_id>&path=<URL 编码后的相对路径>。
- 严禁编造 sandbox:/、file://、/mnt/data、/tmp 这类假路径或本机绝对路径当下载链接 ——
  那些用户点不开、点了会跳错。相对路径直接用工作区里的真实路径，不要自己加任何协议前缀。

用户询问已生成产物的内容时，直接读取文件并完整呈现该产物本身，而不是只报告状态。`

// ============================================================
// 执行逻辑
// ============================================================

// funcCallImpl functional_call 阶段的实际逻辑（同步执行）。
//
// 按 task.phase 分派到规划期 / 执行期两套逻辑。两者共享历史重建、
// 工具加载、turn 记录、hint 写入等骨架，差异只在 prompt 与收口方式。
func funcCallImpl(ctx context.Context, task *framework.Task) (data interface{}, err error) {
	temp, ok := task.TaskItem.(*model.TaskTemp)
	if !ok {
		return nil, fmt.Errorf("func-call-actuator: task item is not *model.TaskTemp")
	}
	if funcCallRegistry.llmResolver == nil || funcCallRegistry.turnDAO == nil ||
		funcCallRegistry.taskDAO == nil || funcCallRegistry.toolProvider == nil {
		return nil, fmt.Errorf("func-call-actuator: dependencies not initialized")
	}

	// 清掉上一轮的「延后重排」标记。
	//
	// hint 常驻内存，若不清，本轮即便正常执行完，容器仍会按 Deferred 把它
	// 拨回 wait 重排 —— 任务从此原地打转。清理放在最前面而不是成功后：
	// 延后标记由本函数末尾写入（容器在 Start 返回后才读），先清后写顺序才对。
	container.GetTransitionStore().ClearDeferred(temp.TaskID)

	// 幂等：上一次 functional_call 是否已经执行完、但状态流转在崩溃时丢失。
	// 命中则跳过本轮 LLM 调用，补写缺失的收尾动作后直接返回，
	// 让框架的 resolveHint 从 task_turn 恢复意图并正常流转。
	if skipped, err := functionalCallIdempotentSkip(ctx, temp); err != nil {
		return nil, err
	} else if skipped {
		return nil, nil
	}

	// 单 Agent 子任务（工作流节点展开的执行单元）优先分派：它没有规划期，
	// 也不需要重建对话历史，prompt 已由 workflow 执行器在创建时组装好。
	if temp.TaskType == model.TaskTypeWorkflowSubTask {
		data, err = workflowSubTaskExecImpl(ctx, temp)
	} else if model.TaskPhase(temp.Phase) == model.PhasePlanning {
		// phase 缺省（历史数据 / DB 默认值异常）按执行期处理：
		// 宁可跳过规划直接干活，也不要卡在一个没有 submit_plan 语义的循环里。
		data, err = planningPhaseImpl(ctx, temp)
	} else {
		data, err = executingPhaseImpl(ctx, temp)
	}

	// 压缩要等前置任务收尾：本轮什么都没做（没调 LLM、没写 turn），
	// 把任务延后重排而不是推进或判失败。
	var waitErr *deferWaitError
	if errors.As(err, &waitErr) {
		if dErr := deferTaskForCompression(ctx, temp, waitErr); dErr != nil {
			return nil, dErr
		}
		return nil, nil
	}
	return data, err
}

// deferTaskForCompression 把「上下文压缩要等前置任务」翻译成一次延后重排。
//
// 三步，缺一不可：
//  1. 落库 delay_until / defer_count —— 调度器扫描会跳过未到期的任务，
//     到期后自动重新入队，无需任何后台唤醒逻辑；
//  2. 打上 hint.Deferred —— 容器据此把任务拨回**本阶段**的 wait，
//     而不是按正常成功路径推进到下一阶段或直接收口（本轮确实没执行）；
//  3. 作废本 task 的历史缓存 —— 前置任务还在写 turn，缓存的 before 段
//     在下次真正执行时已经过期，留着会把「等待期间新产生的交互」漏掉。
//
// 返回 nil 而不是 error：框架只有 success / failed 两个出口，走 failed 会
// 消耗重试次数并最终把任务判死，而等待既不是失败也不是完成。
func deferTaskForCompression(
	ctx context.Context, temp *model.TaskTemp, waitErr *deferWaitError,
) error {
	delay := nextDeferDelay(temp.DeferCount)
	until := time.Now().Add(delay)
	count := temp.DeferCount + 1

	if err := funcCallRegistry.taskDAO.SetTaskDelay(ctx, temp.TaskID, until, count); err != nil {
		return fmt.Errorf("func-call-actuator: defer task for compression: %w", err)
	}
	temp.DelayUntil = &until
	temp.DeferCount = count

	globalAgentHistoryCache.delete(temp.TaskID)
	container.GetTransitionStore().SetDeferred(temp.TaskID)

	log.Printf("[func-call-actuator][%s] deferred %v (round %d, until %s): %v",
		temp.TaskID, delay, count, until.Format(time.RFC3339), waitErr.TaskIDs)
	return nil
}

// functionalCallIdempotentSkip 判断并执行 functional_call 阶段的幂等跳过。
//
// 背景：turn 的写入（执行器内）与任务的状态流转（框架轮询）不在同一个事务里，
// 若进程在「LLM turn 已成功落库、但状态流转」之间崩溃，重启后任务仍停在本阶段，
// 重进执行器时会把已经调过的 LLM 再调一遍，产生重复的 functional_call turn。
// 这里利用 task_turn 作为持久化执行记录，识别「上一次已执行完」并跳过。
//
// 判定条件（同时满足才跳过）：
//  1. 最新一条 turn 是 LLM 调用（否则是正常的 tool_call → functional_call 循环回跳）
//  2. 该 turn 已 success（running/interrupted/failed 说明上次没成功，需重调）
//  3. 该 turn 的语义阶段 == 当前任务阶段（规划期→执行期的合法切换不跳过）
//
// 返回 true 表示已跳过，调用方直接 return nil；返回 false 表示需正常执行。
func functionalCallIdempotentSkip(ctx context.Context, temp *model.TaskTemp) (bool, error) {
	turnDAO := funcCallRegistry.turnDAO

	// 取「最新一条」turn（不论类型），而非 GetLatestLLMTurn：
	// 后者会忽略更晚的 tool_call turn。正常循环里 functional_call 是在 tool_call
	// 之后被重新调度的，此时最新 turn 是 tool_call，绝不能误判成「已执行过」。
	latest, err := turnDAO.GetLatestTurn(ctx, temp.TaskID)
	if err != nil {
		return false, err
	}
	if latest == nil || !latest.TurnType.IsLLMCall() {
		return false, nil
	}
	if latest.Status != model.TurnStatusSuccess {
		return false, nil
	}

	phase := model.TaskPhase(temp.Phase)
	if phase == 0 {
		phase = model.PhaseExecuting
	}
	if latest.TurnPhase() != phase {
		// 规划期提交规划后切执行期：最新 LLM turn 是规划期的，当前阶段是执行期，
		// 这是合法切换，不是「重复执行」，需正常调执行期 LLM。
		return false, nil
	}

	// 解析该 turn 的输出，据此决定重放策略。
	var out model.TurnLLMOutput
	if latest.Output != "" {
		if err := json.Unmarshal([]byte(latest.Output), &out); err != nil {
			// 输出损坏无法重放：保守重调 LLM。重调不会产生副作用，
			// 只是多一次计费，但比「状态丢失/误流转」更安全。
			return false, nil
		}
	}

	hasSubmitPlan, hasRealTool := false, false
	for _, tc := range out.ToolCalls {
		if tc.Name == model.SubmitPlanToolName {
			hasSubmitPlan = true
		} else {
			hasRealTool = true
		}
	}

	if phase == model.PhasePlanning {
		if hasSubmitPlan {
			// 规划期 submit_plan 的正常路径会在 finishPlanning 里把 phase 切成执行期；
			// 这里 phase 仍是规划期，只可能是「输出节点校验不过 → 带反馈重规划」的
			// 中间态。重规划必须带反馈重调 LLM，无法从已落库的 turn 重放，故不跳过。
			return false, nil
		}
		if hasRealTool {
			// 规划期调研 turn：重放「需要工具」的意图。hint 交给框架 resolveHint
			// 从 llmTurnIntent 恢复，这里只需跳过 LLM 调用。
			log.Printf("[func-call-actuator][%s] idempotent skip: planning research turn #%d already done, "+
				"flow to tool_call", temp.TaskID, latest.TurnSeq)
			return true, nil
		}
		// 无工具调用：正常路径已在降级分支切执行期，理论到不了这，保守不跳过。
		return false, nil
	}

	// 执行期。
	if hasRealTool {
		log.Printf("[func-call-actuator][%s] idempotent skip: executing turn #%d already requested tools, "+
			"flow to tool_call", temp.TaskID, latest.TurnSeq)
		return true, nil
	}

	// 最终答案（无工具调用）：上次可能崩溃在「FinishTurn 之后、persistAgentOutput 之前」，
	// 导致 task_output 缺失。这里补写最终答案，再跳过 LLM 调用。
	if err := persistAgentOutput(ctx, funcCallRegistry.taskDAO, funcCallRegistry.turnDAO,
		temp, out.Text, ""); err != nil {
		// 补写失败不阻断：状态机仍会流转到 finished，只是输出可能缺失；记日志便于排查。
		log.Printf("[func-call-actuator][%s] idempotent skip: persist final output on replay error: %v",
			temp.TaskID, err)
	}
	log.Printf("[func-call-actuator][%s] idempotent skip: executing turn #%d already produced final answer (%d chars)",
		temp.TaskID, latest.TurnSeq, len(out.Text))
	return true, nil
}

// executingPhaseImpl 执行期：正常 agent 循环，产出最终答案。
func executingPhaseImpl(ctx context.Context, temp *model.TaskTemp) (data interface{}, err error) {
	log.Printf("[func-call-actuator][%s] start phase=executing (chat=%s turn_count=%d)",
		temp.TaskID, temp.ChatID, temp.TurnCount)

	// 客户端与工具在此一次性备好，再往下传给消息组装与执行循环。
	//
	// 为什么提前到这里：上下文压缩要计量 tools 的 token 占用（它是五块
	// 输入之一，几十个 MCP 工具能占上万 token），还要用 LLM 生成摘要。
	// 而 toolProvider.Load 会实际去 MCP server 拉工具列表，不可为了
	// 压缩再调一遍。
	rt, err := prepareExecutingRuntime(ctx, temp)
	if err != nil {
		return nil, err
	}

	// 组装对话历史（含历史 task）的执行消息；超限时先压缩历史
	messages, ctxBlocks, err := assembleExecutingMessages(ctx, temp, rt)
	if err != nil {
		return nil, err
	}
	return runExecutingLoop(ctx, temp, messages, rt, ctxBlocks)
}

// executingRuntime 执行期一轮 turn 复用的重资源。
//
// 把「解析客户端」与「加载工具」的结果收成一个值传递，避免同一轮 turn
// 内重复解析/重复拉取 MCP 工具列表。
type executingRuntime struct {
	Client  llm.Client
	ToolSet *ToolSet
	Tools   []llm.Tool
}

// prepareExecutingRuntime 解析 LLM 客户端并加载工具集。
func prepareExecutingRuntime(ctx context.Context, temp *model.TaskTemp) (*executingRuntime, error) {
	llmClient, err := resolveLLMClient(ctx, temp)
	if err != nil {
		return nil, err
	}

	toolSet, err := funcCallRegistry.toolProvider.Load(ctx, temp.ChatID)
	if err != nil {
		// 工具加载失败不阻断 —— 降级为无工具模式，让 LLM 用自身知识回答
		log.Printf("[func-call-actuator][%s] load tools failed, fallback to no-tool mode: %v",
			temp.TaskID, err)
		toolSet = nil
	}
	// deep_search 子任务：隐藏 deep_search（防递归嵌套）+ 注入 web_search
	// （联网搜索，绕开 robots 拦截）+ 注入 fetch（抓正文精读），形成
	// 「web_search 定位 → fetch 精读」两段式检索；主 agent 不暴露 fetch。
	if isDeepSearchSubTask(temp) {
		toolSet = funcCallRegistry.toolProvider.applyDeepSearchSubTaskTools(ctx, toolSet)
	}

	return &executingRuntime{
		Client:  llmClient,
		ToolSet: toolSet,
		Tools:   toolSet.LLMTools(),
	}, nil
}

// assembleExecutingMessages 组装对话任务执行期的消息序列：
// system + 已压缩历史摘要 + 未压缩历史交互 + 本轮用户输入 + 本 task 内已发生的交互。
//
// 走 assembleTurnMessages 这个规划期与执行期共用的入口，而不是自己调
// assembleWithCompaction：压缩、分块、底线判死三条规则必须两期一致，
// 分开写迟早分叉（见该函数的注释）。执行期没有尾部追加消息，Tail 留空。
//
// 同时返回最终消息序列的分块 token 明细（含模型上限），供 turn 快照落库 ——
// 那是本次 LLM 调用真实送出的输入构成。
func assembleExecutingMessages(
	ctx context.Context, temp *model.TaskTemp, rt *executingRuntime,
) ([]llm.Message, *model.TurnContextBlocks, error) {
	// 本轮用户输入。带上工作区目录，让小体积文本附件内联预览，
	// 省掉一整轮 read_file 往返。
	userText := renderUserInput(temp.TaskInput.Data, resolveWorkspaceDirForTask(temp))
	if strings.TrimSpace(userText) == "" {
		return nil, nil, fmt.Errorf("func-call-actuator: task %s has empty input_text", temp.TaskID)
	}

	systemPrompt := executingSystemPrompt(ctx, temp.ChatID)

	// 注入规划期已确定的执行计划。
	//
	// submit_plan 那条 turn 不进对话历史（执行期没有该工具，回放会被 provider 判 400），
	// 所以规划结论必须走 system prompt 这条旁路，否则执行期完全不知道刚才规划了什么。
	if planCtx := renderPlanContext(temp.Plan); planCtx != "" {
		systemPrompt = systemPrompt + "\n\n" + planCtx
	}

	asm, err := assembleTurnMessages(ctx, turnAssembleInput{
		Temp:         temp,
		SystemPrompt: systemPrompt,
		UserText:     userText,
		Tools:        rt.Tools,
		Client:       rt.Client,
	})
	if err != nil {
		return nil, nil, err
	}
	return asm.Messages, asm.Blocks, nil
}

// workflowSubTaskExecImpl 单 Agent 子任务（工作流节点）的执行逻辑。
//
// 与对话任务执行期的差别只在 message 组装：子任务不带「对话历史」
// （本 task 之前的历史 task 交互），也没有规划上下文 —— 它的 input_text
// 已由 workflow 执行器在创建时内联了「工作流总体目标 + 节点说明/输入 +
// 前置步骤输出 + 用户附件」。
//
// 但子任务同样会经历 functional_call ↔ tool_call 多轮循环：第二轮起
// 必须带上本子任务内已发生的 assistant tool_calls 与 tool 结果，
// 否则模型会丢失工具调用上下文，重复劳动甚至死循环。因此这里带上
// 「本 task 内的历史交互」（current 段），而不是只发一条 user。
//
// current 段同样是会撑爆窗口的：节点跑几十轮工具循环（抓网页、读文件）
// 攒下的交互量不比主对话小。此前子任务没有压缩兜底、超限直接判死，
// 现在它与对话任务走同一个组装入口（Isolated=true），超限先压自己的
// 早期交互 —— 水位按 task 存，并发节点互不干扰。
func workflowSubTaskExecImpl(ctx context.Context, temp *model.TaskTemp) (data interface{}, err error) {
	log.Printf("[func-call-actuator][%s] start workflow subtask (parent=%s)",
		temp.TaskID, temp.ParentTaskID)

	userText := renderUserInput(temp.TaskInput.Data, resolveWorkspaceDirForTask(temp))
	if strings.TrimSpace(userText) == "" {
		return nil, fmt.Errorf("func-call-actuator: subtask %s has empty input_text", temp.TaskID)
	}

	systemPrompt := executingSystemPrompt(ctx, temp.ChatID)
	rt, err := prepareExecutingRuntime(ctx, temp)
	if err != nil {
		return nil, err
	}

	asm, err := assembleTurnMessages(ctx, turnAssembleInput{
		Temp:         temp,
		SystemPrompt: systemPrompt,
		UserText:     userText,
		Tools:        rt.Tools,
		Client:       rt.Client,
		Isolated:     true,
	})
	if err != nil {
		return nil, err
	}
	return runExecutingLoop(ctx, temp, asm.Messages, rt, asm.Blocks)
}

// executingSystemPrompt 执行期 system prompt：通用 agent 人格 + 图片理解策略 + 对话已启用 skill。
func executingSystemPrompt(ctx context.Context, chatID string) string {
	systemPrompt := funcCallSystemPrompt + "\n\n" + imagePolicySection(ctx, chatID, nil)
	if skillConfigs, sErr := funcCallRegistry.toolProvider.LoadSkills(ctx, chatID); sErr != nil {
		log.Printf("[func-call-actuator] load skills failed (ignore): %v", sErr)
	} else if section := skillpkg.BuildPromptSection(skillConfigs); section != "" {
		systemPrompt = systemPrompt + "\n\n" + section
	}
	return systemPrompt
}

// imagePolicySection 返回「图片理解策略」系统提示片段，按当前对话所选模型是否支持
// 多模态、以及工具集里是否存在图像类工具，给出三种不同指引：
//
//   - 支持多模态：引导模型优先调用 analyze_image 工具理解图片，禁止 read_file 读图
//     或 run_command 跑 OCR。
//   - 不支持多模态、但工具集里存在图像类工具（用户手动接入的 OCR / 识图 MCP 或
//     command skill）：允许模型用这些工具尝试提取图中文字，提取不了再拒绝用户。
//   - 不支持多模态、且无图像类工具：明确告知模型无法查看图像，直接向用户说明并请其
//     用文字描述。不再刻意强调「禁止用 read_file / run_command 跑 OCR 等代码化方案」。
//
// toolSet 为调用方已加载的工具集；传 nil 时内部自行加载（执行期 assemble 阶段
// 尚未来得及加载工具集，这里兜底加载一次，registry 缓存命中、成本可控）。
//
// 与 renderInputFileManifest 的图片指引互补：那里是「附件清单里的处理方式」，
// 这里是全局策略；两者措辞一致，避免模型无所适从。
func imagePolicySection(ctx context.Context, chatID string, toolSet *ToolSet) string {
	tp := funcCallRegistry.toolProvider
	supportsImage := tp != nil && tp.resolveModelSupportsImage(ctx, chatID)
	if supportsImage {
		return "图片理解：\n" +
			"- 本对话已内置 analyze_image 工具。当用户提供图片、或需要理解图中内容时，优先调用 analyze_image（传入图片相对路径），拿到结构化描述后再据此回答。\n" +
			"- 不要用 read_file 读取图片（二进制会乱码），也不要 run_command 跑 OCR（效果差）。"
	}

	// 非多模态：看工具集里有没有图像类工具，决定是「引导 OCR」还是「直接拒绝」。
	if toolSet == nil && tp != nil {
		if ts, err := tp.Load(ctx, chatID); err == nil {
			toolSet = ts
		}
	}
	if hasImageRelatedTool(toolSet) {
		return "图片理解（重要）：\n" +
			"- 你当前使用的模型不支持直接理解图像，无法真正「看」图，但对话中已接入图像处理类工具。\n" +
			"- 当用户提供图片时，可以尝试用这些工具提取图中的文字信息（OCR），再基于提取到的文本回答用户。\n" +
			"- 如果工具无法从图片中提取出有效文字或内容（例如纯图表、照片、手绘图、无文字图片），不要臆测图片内容，明确告知用户你无法处理这张图片，并请其改用文字描述（或改用支持多模态的模型）。\n" +
			"- 不要用 read_file 直接读取图片（会得到乱码）。"
	}
	return "图片理解（重要）：\n" +
		"- 你当前使用的模型不支持图像理解，无法查看图片内容。\n" +
		"- 当用户提供图片或要求理解图片时，明确告知用户你无法处理图片理解，并请其改用文字描述图片内容（或改用支持多模态的模型）。"
}

// imageToolKeywords 图像类工具识别关键词（匹配工具名 + 描述，小写比较）。
//
// 覆盖常见的 OCR / 识图 / 图片理解类工具命名。analyze_image 是框架内置的
// 多模态理解工具，只在 supports_image 时注入，走到 hasImageRelatedTool 时
// 工具集里不会含它，故无需特别排除。
var imageToolKeywords = []string{
	"image", "图片", "图像", "ocr", "vision", "识图", "文字识别", "提取文字",
	"截图", "screenshot", "image_to_text", "extract_text",
}

// hasImageRelatedTool 判断工具集里是否存在图像类工具（按工具名 + 描述关键词匹配）。
func hasImageRelatedTool(ts *ToolSet) bool {
	if ts == nil {
		return false
	}
	for _, e := range ts.tools {
		haystack := strings.ToLower(e.ExposedName + " " + e.Description)
		for _, kw := range imageToolKeywords {
			if strings.Contains(haystack, kw) {
				return true
			}
		}
	}
	return false
}

// resolveLLMClient 解析本对话使用的 LLM 客户端。
//
// 按 task 所属对话的 conversation.model_id 动态选模型；model_id 为空（未显式
// 指定 / 历史数据）时回落到默认模型，再回落到 config.LLM 兜底客户端。
// 对话任务与单 Agent 子任务共用：子任务的 temp.ChatID 与对话一致，因此天然
// 继承对话选定的模型。
func resolveLLMClient(ctx context.Context, temp *model.TaskTemp) (llm.Client, error) {
	return resolveLLMClientWith(ctx, temp.ChatID, funcCallRegistry.llmResolver, funcCallRegistry.conversationDAO)
}

// resolveLLMClientWith 按 chatID 解析对话所用 LLM 客户端（通用实现）。
//
// 与 resolveLLMClient 的差异在于不依赖 funcCallRegistry 包级变量：tool_call
// actuator 侧注入 llm.Client 做图片理解（analyze_image）时也复用本函数。
func resolveLLMClientWith(
	ctx context.Context, chatID string, resolver *llmreg.Resolver, convDAO *dao.ConversationDAO,
) (llm.Client, error) {
	if resolver == nil {
		return nil, fmt.Errorf("llm resolver not initialized")
	}
	modelID := ""
	if convDAO != nil {
		conv, err := convDAO.GetConversationByID(ctx, chatID)
		if err != nil {
			return nil, fmt.Errorf("resolve llm client: get conversation: %w", err)
		}
		if conv != nil {
			modelID = conv.ModelID
		}
	}
	return resolver.Resolve(ctx, modelID)
}

// runExecutingLoop 执行期公共循环骨架：加载工具 → 开 turn → 调 LLM →
// 轮次/熔断检查 → 落库输出 → 写状态机 hint。
//
// 对话任务与单 Agent 子任务共用；差异只在 message 组装（见调用方）。
//
// rt 为调用方已备好的客户端与工具集（见 prepareExecutingRuntime）——
// 上下文压缩需要在组装阶段就拿到工具列表来计量 token，所以这两个重资源
// 上移到调用方加载一次，此处不再重复解析/重复拉 MCP 工具列表。
//
// ctxBlocks 为本次输入的分块 token 明细（含模型上限），随 turn 快照落库；
// nil 表示该路径尚未接入分块计量（历史行为，不报错）。
func runExecutingLoop(
	ctx context.Context, temp *model.TaskTemp, messages []llm.Message, rt *executingRuntime,
	ctxBlocks *model.TurnContextBlocks,
) (data interface{}, err error) {
	if rt == nil {
		return nil, fmt.Errorf("func-call-actuator: executing runtime is nil")
	}
	llmClient := rt.Client
	toolSet := rt.ToolSet
	tools := rt.Tools

	// 轮次预算耗尽后的最后一轮：调用 LLM 之前就把工具摘掉，逼模型直接写总结。
	//
	// 旧实现是「调完再收口」——模型在工具可用的上下文里通常只回 tool_call、
	// 不带文本（实测 case：deep_search 子任务 baffb17c 第 8 轮的 text 只有
	// 「最后一轮：核实 Wan2.5 官方发布细节…」这句过程描述），执行器把
	// needToolCall 置 false 后落库的就是这句话，任务看似成功、结论却是废的。
	// 改成调用前摘工具后，模型没有工具可调，只能输出真正的总结。
	round, maxRound, _, _ := resolveLoopState(ctx, temp.TaskID, model.PhaseExecuting)
	if round >= maxRound {
		log.Printf("[func-call-actuator][%s] tool call budget exhausted (%d/%d), "+
			"final round without tools: force summarization",
			temp.TaskID, round, maxRound)
		tools = nil
		toolSet = nil
		messages = appendFinalRoundInstruction(messages, maxRound)
	}

	log.Printf("[func-call-actuator][%s] %d tools available", temp.TaskID, len(tools))

	// 3. 开 turn
	opts := llm.DefaultOptions()
	choice := llm.ToolChoiceAuto
	if len(tools) == 0 {
		choice = llm.ToolChoiceNone
	}

	inputSnap := buildLLMTurnInput(llmClient.Name(), messages, tools, choice, opts)
	// 分块明细随快照落库：这是本次调用真实送出的输入构成，
	// 调试页据此渲染「各模块占比」，对话页据此显示上下文水位。
	//
	// 注：ctxBlocks 由调用方按 rt.Tools 预先算好，最后一轮摘掉工具后
	// 「工具」这一块会偏大。它只用于展示（压缩决策在调用方已算完），
	// 不值得为最后一轮把整个组装流程重排一遍。
	inputSnap.ContextBlocks = ctxBlocks

	turn, err := funcCallRegistry.turnDAO.BeginTurn(ctx, dao.BeginTurnParams{
		TaskID:       temp.TaskID,
		ChatID:       temp.ChatID,
		ParentTaskID: temp.ParentTaskID,
		TurnType:     model.TurnTypeFunctionalCall,
		Phase:        model.PhaseExecuting,
		Stage:        model.TaskStage(temp.Stage),
		Round:        currentToolCallRound(ctx, temp.TaskID, model.PhaseExecuting),
		Model:        llmClient.Name(),
		Input:        inputSnap,
	})
	if err != nil {
		return nil, fmt.Errorf("func-call-actuator: begin turn failed: %w", err)
	}
	log.Printf("[func-call-actuator][%s] turn #%d started (id=%d)", temp.TaskID, turn.TurnSeq, turn.ID)

	// 失败也把 LLM 已拿到的输出落库，便于调试
	var llmResult *llm.ChatResult
	failTurn := func(cause error) error {
		var outSnap interface{}
		if llmResult != nil {
			outSnap = buildLLMTurnOutput(llmResult)
		}
		if ferr := funcCallRegistry.turnDAO.FailTurn(ctx, turn.ID, cause, outSnap); ferr != nil {
			log.Printf("[func-call-actuator][%s] fail turn %d error: %v", temp.TaskID, turn.ID, ferr)
		}
		// 失败 turn 也算结束 —— 前端收到 turn_done 才会停 spinner 并刷新
		publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)
		return cause
	}

	// 4. 调 LLM
	if reqBytes, mErr := json.Marshal(map[string]interface{}{
		"messages": messages,
		"tools":    toolNames(tools),
		"choice":   choice,
	}); mErr == nil {
		log.Printf("[func-call-actuator][%s] llm req=%s", temp.TaskID, truncate(string(reqBytes), 4000))
	}

	result, err := chatWithToolsStreaming(ctx, llmClient, temp.TaskID, turn.ID, turn.TurnSeq, messages, tools, choice, opts)
	if err != nil {
		return nil, failTurn(fmt.Errorf("func-call-actuator: llm chat failed: %w", err))
	}
	llmResult = result

	if rspBytes, mErr := json.Marshal(result); mErr == nil {
		log.Printf("[func-call-actuator][%s] llm rsp=%s", temp.TaskID, truncate(string(rspBytes), 2000))
	}

	// 5. 检查 LLM 请求的工具是否都存在。
	//
	// 这里只做告警不做过滤：未知工具照样交给 tool_call 阶段，
	// 由它记录一条「工具不存在，可用工具是 xxx」的失败 turn 回喂 LLM。
	// 若在此静默丢弃，模型收不到任何反馈，下一轮会重复请求同一个不存在的工具。
	_, invalidNames := splitValidToolCalls(result.ToolCalls, toolSet)
	if len(invalidNames) > 0 {
		log.Printf("[func-call-actuator][%s] llm requested unknown tools (will feed back): %v",
			temp.TaskID, invalidNames)
	}

	// LLM 既没给文本也没给工具调用 —— 无法推进，判失败。
	//
	// 推理模型（gpt-5.6 / deepseek-reasoner 等）可能只吐思考链不吐正文：
	// 此时把思考链摘要带进错误信息，调试页才能看到「模型到底想了什么」，
	// 而不是一句无信息的「neither text nor tool call」。
	if len(result.ToolCalls) == 0 && strings.TrimSpace(result.Text) == "" {
		reason := fmt.Errorf("func-call-actuator: llm returned neither text nor tool call")
		if s := strings.TrimSpace(result.Reasoning); s != "" {
			reason = fmt.Errorf(
				"func-call-actuator: llm 只返回了思考链、未产出正文或工具调用（思考链 %d 字符，摘录：%s）",
				len(s), truncateRunes(s, 300))
		}
		return nil, failTurn(reason)
	}

	needToolCall := len(result.ToolCalls) > 0

	// 6. 轮次耗尽 + 连续失败熔断检查。
	//
	// 状态机在 ToolCallRound 达到上限时会直接把任务判为 completed。
	// 若此处仍按「要去 tool_call」处理而不写 task_output，
	// 任务会以「成功但无输出」的状态结束。因此提前收口：
	// 把已有文本（可能为空）作为最终答案落库，并强制走 finished 分支。
	round, maxRound, consecutiveFails, repeatedCalls := resolveLoopState(ctx, temp.TaskID, model.PhaseExecuting)
	if needToolCall && round >= maxRound {
		log.Printf("[func-call-actuator][%s] tool call round exhausted (%d/%d), force finish",
			temp.TaskID, round, maxRound)
		needToolCall = false
		if strings.TrimSpace(result.Text) == "" {
			result.Text = fmt.Sprintf(
				"抱歉，本次任务已达到工具调用次数上限（%d 次），未能得出完整结论。", maxRound)
		}
	}

	// 工具调用总次数预算：与轮次上限是两个正交的预算。
	//
	// 只限轮次是不够的 —— 模型一轮会并行发多个工具调用（实测一轮 4~6 个），
	// max_rounds=12 的子任务能跑出 49 次搜索。轮次没超，总量早已失控，
	// 检索面随每轮并行调用不断扩张（一次 Agent 选型调研里 1011 次搜索用了
	// 1001 个**不同** query，死循环检测抓不到这种「每次都不同」的发散）。
	if needToolCall {
		if used, budget := resolveToolCallBudget(ctx, temp.TaskID); budget > 0 && used >= budget {
			log.Printf("[func-call-actuator][%s] tool call budget exhausted (%d/%d calls), force finish",
				temp.TaskID, used, budget)
			needToolCall = false
			if strings.TrimSpace(result.Text) == "" {
				result.Text = fmt.Sprintf(
					"抱歉，本次检索已达到工具调用次数上限（%d 次），未能覆盖全部方向。以上结论基于已获取的信息。",
					budget)
			} else {
				// 模型已给出部分结论：提示它这是被截断的收口，避免下游
				// 把「未覆盖完全」的结论当成完整结论使用。
				result.Text += fmt.Sprintf(
					"\n\n（注：已达到工具调用次数上限 %d 次而收口，以上结论可能未覆盖全部方向。）", budget)
			}
		}
	}

	// 连续失败熔断：同一「工具 + 参数」的调用反复失败（参数错/工具不存在/
	// 服务不可用），继续重试只会空耗 token 与时间。换工具或换参数是新的
	// 指纹、不背旧账 —— 那是模型在换方法尝试；只有完全相同的调用连续失败
	// 达到阈值才强制收口，不再进入 tool_call。
	if needToolCall && consecutiveFails >= model.DefaultMaxConsecutiveFailures {
		log.Printf("[func-call-actuator][%s] same tool call failed %d times in a row (%d/%d), force finish",
			temp.TaskID, consecutiveFails, consecutiveFails, model.DefaultMaxConsecutiveFailures)
		needToolCall = false
		if strings.TrimSpace(result.Text) == "" {
			result.Text = fmt.Sprintf(
				"抱歉，同一个工具调用（相同的工具与参数）已连续失败 %d 次，为避免无限重试已停止。请检查工具配置或换一种方式提问。",
				consecutiveFails)
		}
	}

	// 连续无效熔断：检索类工具「调用成功却一直拿不到东西」（返回为空或过短）。
	//
	// 与上面两条熔断互补：连续失败数的是报错、死循环数的是「完全相同的调用」，
	// 都覆盖不到「每次换个词、每次都空手而归」这种发散型空转 —— 实测一次调研
	// 里 25.7% 的搜索返回 <100B，模型靠换词绕开了前两道检测。
	// 只有「连续 N 次都没拿到有效内容」能识别这种状态。
	if needToolCall {
		if streak := resolveLowValueStreak(ctx, temp.TaskID); streak >= model.DefaultMaxLowValueToolCalls {
			log.Printf("[func-call-actuator][%s] consecutive low-value tool calls (%d/%d), force finish",
				temp.TaskID, streak, model.DefaultMaxLowValueToolCalls)
			needToolCall = false
			if strings.TrimSpace(result.Text) == "" {
				result.Text = fmt.Sprintf(
					"抱歉，连续 %d 次检索都没有拿到有效内容（返回为空或过短），已停止继续检索。"+
						"建议换更具体的关键词、换信息来源，或把取证交给 deep_search。", streak)
			} else {
				result.Text += fmt.Sprintf(
					"\n\n（注：连续 %d 次检索未取得有效内容而收口，以上结论可能缺少外部取证支撑。）", streak)
			}
		}
	}

	// 死循环检测：模型连续多轮请求完全相同的工具调用（工具名 + 参数一致），
	// 说明它卡在「重复同一动作」上，无论调用成功与否都不会再推进。
	// 达到阈值后强制收口，不再进入 tool_call。这是把轮次上限放宽到 100 后
	// 的第二道防线 —— 光靠连续失败熔断拦不住「每次都成功却空转」的循环。
	if needToolCall && repeatedCalls >= model.DefaultMaxRepeatedToolCalls {
		log.Printf("[func-call-actuator][%s] repeated identical tool calls (%d/%d), force finish (dead loop)",
			temp.TaskID, repeatedCalls, model.DefaultMaxRepeatedToolCalls)
		needToolCall = false
		if strings.TrimSpace(result.Text) == "" {
			result.Text = fmt.Sprintf(
				"抱歉，检测到连续 %d 轮重复请求了相同的工具调用，判定为死循环，已停止。请换一种方式提问，或检查工具执行结果是否有变化。",
				repeatedCalls)
		}
	}

	// 7. 先收尾 turn：把本轮 LLM 的结果写库（status → success、output、耗时）。
	//
	// 必须先于 persistAgentOutput —— 后者要读全量 turn 列表生成思考轨迹，
	// 若当前 turn 仍是 running，会被 buildThinkSteps 当作「未完成」跳过，
	// 导致最终答案这一轮（执行器层）永远不出现在 think_layers / think_process 里。
	if err := funcCallRegistry.turnDAO.FinishTurn(ctx, turn.ID, dao.FinishTurnParams{
		Output: buildLLMTurnOutput(result),
		// 冗余到 task_turn 的 token 三列，便于按 task/chat 聚合统计。
		// prompt 单独落列是上下文管理的前提 —— 水位看的是输入侧。
		TotalTokens:      result.Usage.TotalTokens,
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
	}); err != nil {
		log.Printf("[func-call-actuator][%s] finish turn %d error: %v", temp.TaskID, turn.ID, err)
	}
	// 通知 SSE 订阅者：func-call 这一步落库结束，前端应回拉最新 turn 列表。
	// 注意：不论 FinishTurn 本身成功与否都要广播 —— turn 已跑完，订阅方需要刷新。
	publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)

	// 8. 不再调用工具即最终答案，写 task_output
	if !needToolCall {
		// 任务收尾，清理 before 段历史缓存（该 task 不会再进 functional_call）
		globalAgentHistoryCache.delete(temp.TaskID)
		if err := persistAgentOutput(ctx, funcCallRegistry.taskDAO, funcCallRegistry.turnDAO,
			temp, result.Text, ""); err != nil {
			return nil, failTurn(fmt.Errorf("func-call-actuator: persist output failed: %w", err))
		}
	}

	// 9. 写状态机 hint —— 决定流转方向。
	// hint 可能因进程重启而缺失，此时用 DB 统计的轮次重建，避免循环上限失效。
	store := container.GetTransitionStore()
	hint := store.GetHint(temp.TaskID)
	if hint == nil {
		hint = &model.StageTransitionHint{
			MaxToolCallRound: maxRound,
			ToolCallRound:    round,
		}
	}
	if hint.MaxToolCallRound <= 0 {
		hint.MaxToolCallRound = model.DefaultMaxToolCallRound
	}
	hint.NeedToolCall = needToolCall
	// 必须显式清零：规划期提交规划时置过 true，若沿用到执行期，
	// 状态机会一直判「刚提交规划」而把任务反复拉回 functional_call_wait，永不收口。
	hint.PlanSubmitted = false
	store.SetHint(temp.TaskID, hint)

	if needToolCall {
		log.Printf("[func-call-actuator][%s] turn #%d done → tool_call (round %d/%d, %d calls: %v)",
			temp.TaskID, turn.TurnSeq, hint.ToolCallRound, hint.MaxToolCallRound,
			len(result.ToolCalls), callNames(result.ToolCalls))
	} else {
		log.Printf("[func-call-actuator][%s] turn #%d done → finished (answer %d chars)",
			temp.TaskID, turn.TurnSeq, len(result.Text))
	}
	return nil, nil
}

// appendFinalRoundInstruction 轮次预算耗尽时，在本轮消息末尾追加一条收口
// 指令，逼模型输出总结而不是继续要工具。
//
// 为什么不留在「调完 LLM 再收口」：模型在工具可用的上下文里通常只回
// tool_call，附带的那句 text 是「我还要去核实 xx」这类过程描述而非结论，
// 直接收口会把这句话当成最终答案落库 —— 任务状态成功、内容却是废的
// （实测 case：deep_search 子任务 baffb17c 的第 8 轮）。
//
// 工具摘除由调用方负责（tools/toolSet 置 nil），本函数只管把「必须收口」
// 这件事告诉模型 —— 否则它不知道自己被摘了工具，可能因「信息还不够」
// 而输出一段解释而非结论。
//
// 与规划期 researchExhausted 的处理同构（那里只保留 submit_plan 这一个
// 协议工具），区别只是执行期没有任何必须调用的工具，所以直接清空。
//
// 纯函数（不碰 registry / 不落库），便于单测。
func appendFinalRoundInstruction(messages []llm.Message, maxRound int) []llm.Message {
	// 显式告诉模型「现在必须收口」：否则它不知道自己被摘了工具，
	// 可能因为「信息还不够」而输出一段解释而非结论。
	return append(messages, llm.Message{
		Role: llm.RoleUser,
		Content: fmt.Sprintf(
			"（工具调用轮次已达上限 %d 轮，本轮不再提供任何工具）"+
				"请立即基于上面已获得的信息输出最终结论：含关键事实、要点与来源，"+
				"以及仍存疑或未覆盖到的部分。不要再尝试调用工具。", maxRound),
	})
}

// resolveLoopState 取当前工具调用轮次、上限、连续失败计数与连续重复调用计数。
//
// 内存 hint 缺失时（进程重启 / 多副本）轮次回落到 DB 统计，且按 phase 隔离 ——
// 规划期调研消耗的轮次不该占用执行期干活的预算。切 phase 时会把内存轮次归零
// （见 setPlanSubmittedHint），DB 回落路径同样要按 phase 只数本阶段的 tool_call，
// 否则「切换后进程重启」会让执行期一进来就背着规划期的轮次，提前收口。
//
// 连续失败计数与连续重复调用计数只存内存（重启后从 0 重计，由轮次上限兜底）。
func resolveLoopState(
	ctx context.Context, taskID string, phase model.TaskPhase,
) (round, maxRound, consecutiveFails, repeatedCalls int) {
	maxRound = model.DefaultMaxToolCallRound
	if hint := container.GetTransitionStore().GetHint(taskID); hint != nil {
		round = hint.ToolCallRound
		if hint.MaxToolCallRound > 0 {
			maxRound = hint.MaxToolCallRound
		}
		return round, maxRound, hint.ConsecutiveFailures, hint.RepeatedToolCallCount
	}
	return int(countToolCallRoundInPhase(ctx, taskID, phase)), maxRound, 0, 0
}

// resolveToolCallBudget 取工具调用总次数的用量与预算。
//
// 返回 (used, max)；max <= 0 表示本任务不设总次数预算（普通对话任务），
// 调用方应跳过这条收口判定。
//
// 用量一律以 DB 统计为准，不用内存的 hint.ToolCallCount：预算失效的后果是
// 检索发散（一次调研能跑出一千次搜索），而内存态在进程重启 / 多副本接管时
// 会丢 —— 这种关键约束不能建立在会丢的状态上。
func resolveToolCallBudget(ctx context.Context, taskID string) (used, max int) {
	if hint := container.GetTransitionStore().GetHint(taskID); hint == nil || hint.MaxToolCallCount <= 0 {
		return 0, 0
	} else {
		max = hint.MaxToolCallCount
	}
	count, err := funcCallRegistry.turnDAO.CountToolCallTurns(ctx, taskID)
	if err != nil {
		log.Printf("[func-call-actuator][%s] count tool calls failed: %v", taskID, err)
		return 0, max
	}
	return int(count), max
}

// countToolCallRoundInPhase 按 phase 统计已完成的 tool_call 轮数。
func countToolCallRoundInPhase(
	ctx context.Context, taskID string, phase model.TaskPhase,
) int32 {
	round, err := funcCallRegistry.turnDAO.CountToolCallRoundsByPhase(ctx, taskID, phase)
	if err != nil {
		log.Printf("[func-call-actuator][%s] count tool_call rounds (phase=%s) failed: %v",
			taskID, phase, err)
		return 0
	}
	return int32(round)
}

// ============================================================
// 工具调用校验
// ============================================================

// splitValidToolCalls 把 LLM 请求的工具调用分成「工具集里存在的」与「未知的」
func splitValidToolCalls(
	calls []llm.ToolCall, toolSet *ToolSet,
) (valid []llm.ToolCall, invalidNames []string) {
	if len(calls) == 0 {
		return nil, nil
	}
	for _, c := range calls {
		if _, ok := toolSet.Lookup(c.Name); ok {
			valid = append(valid, c)
		} else {
			invalidNames = append(invalidNames, c.Name)
		}
	}
	return valid, invalidNames
}

// currentToolCallRound 读取当前的工具调用轮次，用于标注 turn 的 round。
// 内存 hint 缺失时（进程重启 / 多副本）回落到 DB 统计（按 phase 隔离）。
func currentToolCallRound(ctx context.Context, taskID string, phase model.TaskPhase) int32 {
	if hint := container.GetTransitionStore().GetHint(taskID); hint != nil {
		return int32(hint.ToolCallRound)
	}
	return countToolCallRoundInPhase(ctx, taskID, phase)
}

// ============================================================
// 输出持久化
// ============================================================

// persistAgentOutput 把 agent 的最终答案写入 task.task_output。
//
// 除了答案本身，还会补齐三类信息：
//   - OutputFiles  ：本次任务在工作区内产出的文件（按 mtime > start_time 扫描）
//   - ToolCalls    ：工具调用明细（工具名、入参、返回、耗时）
//   - ThinkProcess ：planning / functional_call / tool_call 全过程的可读描述
//
// 长回复保护：reply_text 超过 maxInlineReplyRunes 时自动转存为工作区文件，
// task_output 只留摘要 + 文件引用，避免撑爆 DB 与后续 LLM 上下文。
//
// 入参用 *model.TaskTemp 而非裸 taskID —— 产物扫描需要 ChatID（定位工作区）
// 与 StartTime（判定哪些文件属于本次任务）。
//
// 产物扫描基准用 StartTime（任务首次开始执行时刻，恒定不变）：start_time
// 已与看门狗计时解耦（看门狗改用 stage_start_time），不再每次流转重设，
// 因此能正确涵盖任务全生命周期（含前几轮 stage）产出的文件。
// persistAgentOutput 把任务（对话任务 / 工作流节点子任务）的最终回复落库为
// AgentOutputData。workflowTaskID 非空时随输出透出（仅规划移交工作流的对话
// 任务使用），前端据此渲染「查看工作流」跳转按钮。
func persistAgentOutput(
	ctx context.Context,
	taskDAO *dao.TaskDAO, turnDAO *dao.TaskTurnDAO,
	temp *model.TaskTemp, replyText string, workflowTaskID string,
) error {
	if temp == nil {
		return fmt.Errorf("persistAgentOutput: nil task")
	}
	taskID := temp.TaskID
	wsDir := resolveWorkspaceDirForTask(temp)
	// 子任务各自持有独立产出目录：转存与产物扫描都按它走，
	// 避免把并行节点 / 同窗口其它子任务的文件算成本任务的产物。
	artifactDir := resolveTaskArtifactDir(temp)

	// 长回复先转存，再收集产物 —— 顺序不能反：
	// 转存产生的文件本身也要出现在 OutputFiles 里，
	// 而它的 mtime 必然晚于 start_time，会被随后的扫描自然收进来。
	finalReply, dumped := dumpLongReply(wsDir, artifactDir, taskID, replyText)

	out := model.AgentOutputData{
		ReplyText:      finalReply,
		WorkflowTaskID: workflowTaskID,
	}

	// 产物清单：扫描工作区内本次任务写过的文件。
	// 扫描失败/为空都不影响主流程，产物清单是增值信息而非答案本身。
	out.OutputFiles = collectTaskArtifacts(wsDir, temp.StartTime, artifactDir)
	if dumped != nil {
		// 兜底：扫描理应已经覆盖转存文件，但若 start_time 缺失导致扫描返回空，
		// 至少要把转存文件挂上去，否则用户拿不到完整正文。
		out.OutputFiles = mergeOutputFiles(out.OutputFiles, []model.FileInfo{*dumped})
	}
	// 补齐下载链接（需要 chat_id，故不在 pkg/workspace 里做）
	fillDownloadURLs(temp.ChatID, out.OutputFiles)

	// 汇总执行轨迹（失败不阻断主流程，答案更重要）
	turns, err := turnDAO.ListTurnsByTaskID(ctx, taskID)
	if err != nil {
		log.Printf("[func-call-actuator][%s] list turns for output failed: %v", taskID, err)
	} else {
		for _, t := range turns {
			if t.TurnType == model.TurnTypeToolCall {
				out.ToolCalls = append(out.ToolCalls, buildToolCallRecord(t))
			}
		}
		out.FillThinkProcess(turns)
	}

	outJSON, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal agent output: %w", err)
	}
	return taskDAO.UpdateTaskOutput(ctx, taskID, string(outJSON))
}

// buildToolCallRecord 把一条 tool_call turn 转成对外的工具调用记录
func buildToolCallRecord(t *model.TaskTurn) model.ToolCallRecord {
	rec := model.ToolCallRecord{
		ToolName:   t.ToolName,
		ServerName: t.ServerName,
		Status:     int(t.Status),
		TimeCost:   fmt.Sprintf("%dms", t.TimeCostMs),
	}
	if t.Input != "" {
		var in model.TurnToolInput
		if err := json.Unmarshal([]byte(t.Input), &in); err == nil {
			if argBytes, mErr := json.Marshal(in.Arguments); mErr == nil {
				rec.Input = string(argBytes)
			}
			if rec.ToolName == "" {
				rec.ToolName = in.ToolName
			}
			rec.Summary = strings.TrimSpace(in.Summary)
		}
	}
	if t.Output != "" {
		var o model.TurnToolOutput
		if err := json.Unmarshal([]byte(t.Output), &o); err == nil {
			rec.Output = o.Content
		}
	}
	if t.ErrorMsg != "" && rec.Output == "" {
		rec.Output = t.ErrorMsg
	}
	return rec
}

// ============================================================
// 日志辅助
// ============================================================

func toolNames(tools []llm.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func callNames(calls []llm.ToolCall) []string {
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return names
}

// ============================================================
// 工厂
// ============================================================

// NewFuncCallActuator 创建 functional_call 阶段执行器（同步）。
//
// 规划期与执行期共享本执行器，由 task.phase 分派（见 funcCallImpl）。
//
// 依赖：
//   - llmResolver  按对话 model_id 解析大模型客户端
//   - taskDAO      写最终答案 / 规划快照 / 切换 phase
//   - convDAO      规划期读写对话级累积 workflow（也用于取对话的 model_id）
//   - turnDAO      记录 turn 明细 + 重建对话历史
//   - convMCPDAO   查对话绑定的 MCP server
//   - mcpServerDAO 取 MCP server 配置
//   - registry     MCP 工具注册表（须与 tool_call actuator 共享同一实例，
//     保证暴露给 LLM 的工具名在 tool_call 侧能反查到）
//   - convSkillDAO 查对话绑定的 Skill（prompt 型注入 system prompt，command 型作为工具）
//   - skillDAO     取 Skill 配置
//   - defaultMCP   框架级 MCP server（fetch 精读，仅 deep_search 子任务注入；可空）
//
// 因 fact.MakeFucntionActuator 只接受纯函数签名，实际依赖通过包级
// funcCallRegistry 注入，故本工厂在进程内应只调用一次（用 sync.Once 兜底）。
func NewFuncCallActuator(
	llmResolver *llmreg.Resolver,
	taskDAO *dao.TaskDAO,
	convDAO *dao.ConversationDAO,
	turnDAO *dao.TaskTurnDAO,
	convMCPDAO *dao.ConversationMCPDAO,
	mcpServerDAO *dao.MCPServerDAO,
	registry *mcppkg.Registry,
	convSkillDAO *dao.ConversationSkillDAO,
	skillDAO *dao.SkillDAO,
	ws *workspacepkg.Manager,
	defaultMCP []*mcppkg.ServerConfig,
	modelConfigDAO *dao.ModelConfigDAO,
	searchConfigDAO *dao.SearchConfigDAO,
) framework.TaskActuator {
	funcCallOnce.Do(func() {
		funcCallRegistry = funcCallDeps{
			llmResolver:     llmResolver,
			taskDAO:         taskDAO,
			conversationDAO: convDAO,
			turnDAO:         turnDAO,
			toolProvider:    newToolProvider(convMCPDAO, mcpServerDAO, registry, convSkillDAO, skillDAO, defaultMCP, convDAO, modelConfigDAO, searchConfigDAO, ws),
			ws:              ws,
		}
	})
	actuator, err := fact.MakeFucntionActuator(funcCallImpl, nil)
	if err != nil {
		panic("create func_call actuator: " + err.Error())
	}
	return actuator
}
