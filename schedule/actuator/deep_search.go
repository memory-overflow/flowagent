// Package actuator — deep_search 内置工具的搜索执行器
//
// deep_search 是「联网搜索」的框架级内置工具（见 pkg/workspace 的 deepSearchTool）。
// 它本身不抓网页，而是把一个检索目标交给一个专门的搜索子任务：
//
//	主对话任务（tool_call 阶段）
//	  └─ 调 deep_search(query, max_rounds)
//	      └─ 创建一个 TaskTypeWorkflowSubTask（ParentTaskID = 主对话任务）
//	          └─ 子任务走 functional_call ↔ tool_call 循环，用 fetch 抓网页
//	          └─ 子任务收口后，把 task_output 里的汇总结论读回来
//	      └─ 同步轮询子任务终态，把结论作为 deep_search 的工具结果返回
//
// 为什么用子任务而不是在主循环里直接 fetch：
//   - 搜索过程会反复抓取超长网页文本，直接放主上下文会迅速撑爆（历史问题 3）；
//   - 子任务的 turn 天然落库（task_turn），整个「模型 + fetch」循环每一步的
//     LLM 输入输出都能在调试页查看（用户要求的 debug 能力）；
//   - 子任务的检索轮次通过流转 hint 限制（默认 4 轮），及时收口避免空转；
//     最后一轮执行器会摘掉全部工具逼模型总结（见 runExecutingLoop），
//     否则模型在仍有工具可调用时往往只回 tool_call，落库的是「我还要去
//     核实 xx」这类过程描述而非结论，deep_search 就拿不到可用结果。
package actuator

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
	"github.com/memory-overflow/flowagent/schedule/container"
	"gorm.io/gorm"
)

// deepSearchPollInterval 轮询子任务终态的时间间隔。
const deepSearchPollInterval = 1 * time.Second

// deepSearchToolCallsPerRound 换算工具调用总次数预算时，每轮允许的调用次数。
//
// 取值 3 的依据：实测一轮里模型常并行发 4~6 个工具调用（一个 max_rounds=12
// 的子任务跑出过 49 次 web_search），而完全按 1 算又会掐死并行检索能力。
// 3 是折中 —— 12 轮单次预算从「不限」压到 36 次，同时一轮内仍可并行。
const deepSearchToolCallsPerRound = 3

// buildDeepSearchRunner 构造 deep_search 工具的搜索执行器闭包。
//
// 闭包捕获父任务 parent（当前正处于 tool_call 阶段的对话任务）与 taskDAO。
// 上层 tool_call actuator 在构造内置工具的 Context 时把它注入 DeepSearchRunner，
// 由 workspace 的 deepSearchTool handler 转发调用。parentTurnID 透传到子任务里，
// 让调试页能把子任务入口直接挂在对应的 tool_call 行上，而不是堆在顶部列表里。
func buildDeepSearchRunner(taskDAO *dao.TaskDAO, parent *model.TaskTemp, parentTurnID int64) workspacepkg.DeepSearchRunnerFunc {
	return func(ctx context.Context, query string, maxRounds int) (string, error) {
		if taskDAO == nil {
			return "", fmt.Errorf("deep_search: task dao not initialized")
		}
		if parent == nil {
			return "", fmt.Errorf("deep_search: parent task is nil")
		}
		return runDeepSearch(ctx, taskDAO, parent, parentTurnID, query, maxRounds)
	}
}

// runDeepSearch 执行一次深度搜索：创建/复用搜索子任务，限制检索轮次，
// 同步等待子任务终态，返回汇总结论。parentTurnID 写入新建子任务的
// TaskInput.Data.ParentTurnID；复用已有子任务时追加进 ParentTurnIDs，
// 便于调试页把每个 turn 都映射到子任务入口。
func runDeepSearch(
	ctx context.Context, taskDAO *dao.TaskDAO, parent *model.TaskTemp,
	parentTurnID int64,
	query string, maxRounds int,
) (string, error) {
	if maxRounds <= 0 {
		maxRounds = model.DefaultDeepSearchMaxRound
	}
	if maxRounds > model.DeepSearchMaxRoundLimit {
		maxRounds = model.DeepSearchMaxRoundLimit
	}
	if maxRounds < 1 {
		maxRounds = 1
	}
	// 比对与存储统一用 trim 后的 query，避免仅首尾空白差异造成误判为不同目标。
	query = strings.TrimSpace(query)

	// 幂等：同一父任务下可能已有未完成的搜索子任务（进程重启重放 / 上一轮
	// 等待被中断）。找到就复用，避免重复创建、重复搜索。
	//
	// 复用必须 query 完全一致：一批并发 tool_call 里常出现多个不同检索目标的
	// deep_search，若只看「有 pending 子任务」就复用，后一次会拿到前一次的结论，
	// 数据张冠李戴（比多跑一次严重得多）。
	sub, err := findPendingDeepSearchSubTask(ctx, taskDAO, parent.TaskID, query)
	if err != nil {
		return "", err
	}
	var subTaskID string
	if sub != nil {
		subTaskID = sub.TaskID
		// 复用意味着多个 turn 共享同一子任务：把本轮 turn id 也登记进
		// parent_turn_ids，调试页每个 turn 都能挂上入口。登记失败只丢调试入口，
		// 不阻断搜索（结论本身不受影响）。
		if changed, aErr := taskDAO.AppendSubTaskParentTurnIDs(ctx, subTaskID, parentTurnID); aErr != nil {
			log.Printf("[deep-search][%s] reuse pending subtask %s, but register parent turn %d failed: %v",
				parent.TaskID, subTaskID, parentTurnID, aErr)
		} else if changed {
			log.Printf("[deep-search][%s] reuse pending subtask %s (register parent turn %d)",
				parent.TaskID, subTaskID, parentTurnID)
		} else {
			log.Printf("[deep-search][%s] reuse pending subtask %s (parent turn %d already registered)",
				parent.TaskID, subTaskID, parentTurnID)
		}
	} else {
		subTaskID, err = createDeepSearchSubTask(ctx, taskDAO, parent, parentTurnID, query, maxRounds)
		if err != nil {
			return "", err
		}
		log.Printf("[deep-search][%s] created subtask %s (query_len=%d max_rounds=%d)",
			parent.TaskID, subTaskID, len(query), maxRounds)
	}

	// 限制子任务的检索规模：写入流转 hint，子任务首轮 functional_call 读到后
	// 按它收口（见 resolveLoopState / resolveToolCallBudget）。
	//
	// 两个预算是正交的，缺一不可：
	//   - MaxToolCallRound 数「轮」，但模型一轮会并行发多个调用，只限轮次
	//     等于没限总量（12 轮子任务实测跑出 49 次搜索），检索面随之扩张；
	//   - MaxToolCallCount 数「次」，是真正压住发散的那一道。
	//
	// 注意：hint 是进程内内存态（globalTransitionStore）。多副本部署下若子任务
	// 被另一副本接管，这里设的上限会丢失 —— 因此搜索 prompt 里同样写了
	// 「轮次 / 次数上限」的软约束兜底，两者叠加保证及时收口。
	container.GetTransitionStore().SetHint(subTaskID, &model.StageTransitionHint{
		MaxToolCallRound: maxRounds,
		MaxToolCallCount: maxRounds * deepSearchToolCallsPerRound,
	})

	return waitDeepSearchSubTask(ctx, taskDAO, subTaskID, parent.TaskID)
}

// findPendingDeepSearchSubTask 查找父任务下「检索目标相同且仍未完成」的搜索子任务（幂等复用）。
//
// 对话任务本身不创建 workflow 子任务，因此 parent 下的子任务只会是 deep_search
// 产生的搜索子任务，直接按 parent_task_id 查即可。
//
// 命中条件（三条全要满足）：
//  1. 子任务仍在 queued / running —— 已终态的子任务结论不再变动，复用无意义；
//  2. SubTaskKind 是 deep_search —— 避免误命中将来其他类型的子任务；
//  3. DeepSearchQuery 与本次 query 完全一致 —— 这才是「同一件事的重入」
//     （进程重启重放、同批并发对同一目标调了两次）。query 不同必须是两个
//     独立子任务，否则后一次会读到前一次的结论。
//
// 老数据（改动前创建的子任务）没写 DeepSearchQuery，条件 3 判空必然不成立，
// 退化为「新建子任务」—— 是安全的降级方向，最多多跑一次搜索。
func findPendingDeepSearchSubTask(
	ctx context.Context, taskDAO *dao.TaskDAO, parentTaskID string, query string,
) (*model.Task, error) {
	subs, err := taskDAO.ListSubTasksByParentLight(ctx, parentTaskID)
	if err != nil {
		return nil, fmt.Errorf("deep_search: list subtasks: %w", err)
	}
	for _, s := range subs {
		if s.Status != model.TaskStatusQueued && s.Status != model.TaskStatusRunning {
			continue
		}
		if s.TaskInput.Data == nil {
			continue
		}
		if s.TaskInput.Data.SubTaskKind != model.SubTaskKindDeepSearch {
			continue
		}
		if s.TaskInput.Data.DeepSearchQuery != query {
			continue
		}
		return s, nil
	}
	return nil, nil
}

// createDeepSearchSubTask 创建搜索子任务。
//
// 复用 workflow 子任务机制：TaskType=workflow_subtask、Stage=functional_call_wait、
// Phase=executing。functional_call 执行器的 workflowSubTaskExecImpl 分支会读取
// input_text（这里内联了搜索指令）作为 user 消息，跑「模型 + fetch」循环。
// parentTurnID 写入 TaskInput.Data.ParentTurnID，用于调试页 turn → sub_task 映射。
// query 原文一并写入 DeepSearchQuery，供并发/重放时判断能否复用。
func createDeepSearchSubTask(
	ctx context.Context, taskDAO *dao.TaskDAO, parent *model.TaskTemp,
	parentTurnID int64,
	query string, maxRounds int,
) (string, error) {
	subTaskID := uuid.New().String()
	now := time.Now()
	task := &model.Task{
		TaskID:       subTaskID,
		ParentTaskID: parent.TaskID,
		ChatID:       parent.ChatID,
		TaskType:     model.TaskTypeWorkflowSubTask,
		TaskInput: model.JsonField[*model.TaskInputData]{
			Data: &model.TaskInputData{
				InputText:    buildDeepSearchPrompt(query, maxRounds),
				SubTaskKind:  model.SubTaskKindDeepSearch,
				ParentTurnID: parentTurnID,
				// 首个派发 turn 同时进数组，保证 ParentTurnIDList() 在「有复用」和
				// 「无复用」两种情况下首元素一致，调试入口位置不跳。
				ParentTurnIDs:   []int64{parentTurnID},
				DeepSearchQuery: query,
				// 搜索子任务同样独占一个产出目录，挂在父任务目录下：
				// 它的长回复会被自动转存，若不隔离，转存文件会混进父任务的
				// 产物清单，而父任务（节点）自己也分不清那是谁写的。
				ArtifactDir: buildDeepSearchArtifactDir(
					resolveTaskArtifactDir(parent), subTaskID),
			},
		},
		Stage:     int(model.StageFunctionalCallWait),
		Phase:     int(model.PhaseExecuting),
		Status:    model.TaskStatusQueued,
		Progress:  0,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := dao.Transaction(func(tx *gorm.DB) error {
		return (&dao.TaskDAO{Tx: tx}).CreateTask(ctx, task)
	}); err != nil {
		return "", fmt.Errorf("create deep_search subtask: %w", err)
	}
	return subTaskID, nil
}

// buildDeepSearchPrompt 组装搜索子任务的检索指令（作为 input_text）。
//
// 预算与聚焦是这里最要紧的两段，都是被真实事故逼出来的：
//   - 预算：一次 Agent 平台选型调研里，单个 deep_search 子任务跑了 49 次搜索，
//     整条工作流累计 1011 次 web_search、445 次 fetch（12MB）。发散不是因为
//     重复搜同一个词（1001 个 query 各不相同，死循环检测拦不住），而是因为
//     每轮都在扩张检索面。所以除了轮次，必须给「总次数」一个硬上限。
//   - 聚焦：模型倾向于把检索目标里提到的每个对象、每个维度各搜一遍，
//     于是「对比 A 与 B」能铺成几十次搜索。指令里明确禁止这种展开。
func buildDeepSearchPrompt(query string, maxRounds int) string {
	maxCalls := maxRounds * deepSearchToolCallsPerRound
	return fmt.Sprintf(`你是一个专业的网络搜索与信息综合助手。请围绕下面的检索目标，进行多轮搜索与抓取，最终给出一份结构化的汇总结论。

【检索目标】
%s

【预算（硬约束）】
- 工具调用轮次不超过 %d 轮，且**工具调用总次数不超过 %d 次**（同一轮里的每个调用都各自计数）。
- 每一轮开始前先自问：已有信息是否足以回答检索目标？足够就立即收口输出结论，不要为了「更完整」继续搜。
- 预算耗尽时系统不会再给你工具，届时必须基于已有信息输出结论。

【聚焦要求（重要）】
1. 只检索【检索目标】明确涉及的对象与维度。不要扩展到目标没提到的对象（例如目标只问 A 与 B，就不要去查 C、D、E），也不要顺带覆盖目标没问的方面（部署、定价、社区生态等）。
2. 目标里列了多个对象时，用一次检索尽量覆盖多个对象（合并成一条关键词），不要为每个对象各发一次搜索 —— 那是检索面爆炸的首要原因。
3. 同一事实已有 2 个及以上独立来源确认即视为可信，不要再为它发起新的检索。
4. 某个方向抓不到，换一个来源或换一种表述最多重试 1 次；仍无结果就在结论里标注「未获取到」，不要反复换词。

【要求】
1. 先用联网搜索工具 web_search 搜索关键词、定位候选链接，再用网页抓取工具 fetch 抓取正文精读，交叉验证多个来源，不要只依赖单一结果。本子任务不提供 deep_search，请勿尝试调用它。
2. web_search 的返回值已经附带了前 3 条结果的正文（已截断），可直接引用；不要对这几条重复调用 fetch，把预算留给「补充检索」与「交叉验证」。只在其余条目确实需要精读时才用 fetch。
3. 抓取到的网页内容可能很长，请自行提炼要点，不要照搬原文。
4. 某个来源抓取失败或被拒绝（如 robots 限制、超时）时，换其他来源或搜索引擎继续，不要卡在同一个地址。
5. 最终直接输出汇总结论（含关键事实、要点、来源，以及仍存疑/不确定的部分），不要再调用工具。`, query, maxRounds, maxCalls)
}

// waitDeepSearchSubTask 同步轮询子任务直到终态，返回汇总结论。
//
// 两条退出路径缺一不可：
//  1. 上层 ctx 取消 —— deep_search 作为内置工具在 runToolEntry 里被 15 分钟
//     超时上下文包着，超时后这里随之退出，不再空等；
//  2. 父任务已进入终态（被用户停止 / 已失败）—— 此时搜索结果已经没人会用，
//     继续等只是白烧 token。这里主动把搜索子任务标 stopped 并返回，
//     作为 StopTask 级联的兜底：级联跑到「标记」与「本轮新建子任务」之间
//     的竞态窗口时，漏网的搜索子任务会被这条路径收掉。
//
// 顺序上先判子任务自身终态、再判父任务：父任务终态时子任务可能刚好也已
// 产出结论，这种情况下降级返回结论好过直接报错。
func waitDeepSearchSubTask(
	ctx context.Context, taskDAO *dao.TaskDAO, subTaskID string, parentTaskID string,
) (string, error) {
	ticker := time.NewTicker(deepSearchPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("deep_search: wait subtask %s cancelled: %w", subTaskID, ctx.Err())
		case <-ticker.C:
			sub, err := taskDAO.GetTaskByID(ctx, subTaskID)
			if err != nil {
				return "", fmt.Errorf("deep_search: get subtask %s: %w", subTaskID, err)
			}
			if sub == nil {
				return "", fmt.Errorf("deep_search: subtask %s not found", subTaskID)
			}

			switch sub.Status {
			case model.TaskStatusCompleted:
				return parseDeepSearchSummary(sub.TaskOutput), nil

			case model.TaskStatusFailed, model.TaskStatusStopped, model.TaskStatusPartialFailed:
				// 失败/停止：子任务若已产出部分结论则降级返回（好过直接报错），
				// 否则把原因作为错误回喂给主对话 LLM，让它决定重试或换方式。
				return degradeDeepSearchResult(subTaskID, sub)
			}

			// queued / running：父任务若已终态，别再等了
			if parentEnded(ctx, taskDAO, parentTaskID) {
				log.Printf("[deep-search] subtask %s aborted: parent task %s already ended",
					subTaskID, parentTaskID)
				if err := taskDAO.UpdateTaskStatus(ctx, subTaskID,
					model.TaskStatusStopped, nil, 0); err != nil {
					log.Printf("[deep-search] stop subtask %s failed: %v", subTaskID, err)
				}
				// 再读一次：可能刚好在判定间隙里写完了结论，有就用上
				if fresh, err := taskDAO.GetTaskByID(ctx, subTaskID); err == nil && fresh != nil {
					if summary := parseDeepSearchSummary(fresh.TaskOutput); strings.TrimSpace(summary) != "" {
						return summary, nil
					}
				}
				return "", fmt.Errorf("deep_search: parent task %s ended, subtask %s stopped",
					parentTaskID, subTaskID)
			}
		}
	}
}

// degradeDeepSearchResult 子任务异常收口时的降级处理：有部分结论就返回结论，
// 否则把原因作为错误回喂给主对话 LLM，让它决定重试或换方式。
//
// 这里不改变「调用是否失败」的语义 —— 有结论即视为可用，
// 因为对上游来说一份残缺的结论远好过一个错误串。
func degradeDeepSearchResult(subTaskID string, sub *model.Task) (string, error) {
	if summary := parseDeepSearchSummary(sub.TaskOutput); strings.TrimSpace(summary) != "" {
		log.Printf("[deep-search] subtask %s ended with status=%d, degrade to partial summary",
			subTaskID, sub.Status)
		return summary, nil
	}
	reason := ""
	if sub.FailedReason.Data != nil {
		reason = sub.FailedReason.Data.Message
	}
	if reason == "" {
		reason = fmt.Sprintf("status=%d", sub.Status)
	}
	return "", fmt.Errorf("deep_search: subtask %s ended with %s", subTaskID, reason)
}

// parentEnded 判断父任务是否已进入终态（含记录已不存在的情况）。
// 查询失败按「未结束」处理 —— 一次 DB 抖动不该中止还在正常跑的搜索。
func parentEnded(ctx context.Context, taskDAO *dao.TaskDAO, parentTaskID string) bool {
	if parentTaskID == "" {
		return false
	}
	p, err := taskDAO.GetTaskByID(ctx, parentTaskID)
	if err != nil {
		log.Printf("[deep-search] get parent task %s failed: %v", parentTaskID, err)
		return false
	}
	if p == nil {
		return true // 记录都没了：必然已经收口
	}
	return p.IsTerminal()
}

// parseDeepSearchSummary 从子任务 task_output 里取出汇总结论（ReplyText）。
func parseDeepSearchSummary(raw string) string {
	out := parseAgentOutput(raw)
	if out == nil {
		return ""
	}
	return strings.TrimSpace(out.ReplyText)
}
