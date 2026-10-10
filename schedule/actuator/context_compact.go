// Package actuator — 上下文压缩
//
// 模型输入被拆成五块，按「可否压缩」分成两类：
//
//	不可压：
//	  1. system_prompt  —— agent 人格 / 工具指引 / skill 声明 / 规划上下文
//	  2. tools          —— 工具 JSON Schema（每轮全量重发）
//	  3. 工作流摘要      —— renderWorkflowSummary 的产物，本身已是压缩结果
//	可压：
//	  4. 已压缩历史摘要  —— context_summary 表，滚动更新
//	  5. 未压缩历史详情  —— 尚未纳入摘要的历史 task 交互
//
// 流程：拼装后用 tiktoken 计总量 → 超过上限 80% 则进入压缩循环 →
// 每轮把「块4 + 块5 的前 80%（按 token 累计、对齐 task 边界）」交给 LLM
// 重写成一份新摘要（≤ 上限 10%）→ 重算水位 → 直到降到上限 30% 以下。
//
// 三条硬约束（错一条就会出线上问题）：
//   - task 原子性：一个 task 的所有 turn 要么全压、要么全不压。半压会
//     切断 assistant tool_calls 与 tool 结果的配对，provider 直接判 400。
//   - 当前 task 的**近况**永不压缩：它还在推进中，最近几轮的交互是模型
//     下一步决策的直接依据。只有在「历史压干净了仍超底线」时才动它的
//     早期部分，且尾部始终保留若干条原文（见 pickCurrentCut）。
//   - 必须能退出：不可压部分本身就可能超过 30%，此时目标永远达不成，
//     需降级到「压到 80% 以下即通过」并在无法推进时立即停止。
package actuator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
	"github.com/memory-overflow/flowagent/pkg/stream"
	"github.com/memory-overflow/flowagent/schedule/container"
)

// ============================================================
// 五块输入的 token 明细
// ============================================================

// contextBlocks 一次请求各块的 token 占用，用于水位判定与可观测性。
type contextBlocks struct {
	SystemPrompt    int // 块1，不可压
	Tools           int // 块2，不可压
	WorkflowSummary int // 块3，不可压
	Compacted       int // 块4，已压缩历史摘要
	Detail          int // 块5，未压缩历史详情
	// UserAndCurrent 本轮 user 输入 + 当前 task 已发生的交互。
	// 按决策「当前 task 永不压缩」，它与前三块一样属于不可压部分。
	UserAndCurrent int
}

// Total 本次请求的输入 token 总量。
func (b contextBlocks) Total() int {
	return b.SystemPrompt + b.Tools + b.WorkflowSummary +
		b.Compacted + b.Detail + b.UserAndCurrent
}

// Incompressible 不可压部分的总量。
//
// 它决定了压缩的理论下界：即使把块4、块5 压到 0，总量也不会低于此值。
// 判定「是否还有必要继续压」时必须参考它，否则会在压不动的情况下空转。
func (b contextBlocks) Incompressible() int {
	return b.SystemPrompt + b.Tools + b.WorkflowSummary + b.UserAndCurrent
}

func (b contextBlocks) String() string {
	return fmt.Sprintf("sys=%d tools=%d wf=%d compacted=%d detail=%d cur=%d total=%d",
		b.SystemPrompt, b.Tools, b.WorkflowSummary,
		b.Compacted, b.Detail, b.UserAndCurrent, b.Total())
}

// toSnapshot 转成 model 包的持久化结构，随 turn input 快照落库。
// limit 为该次调用所用模型的最大输入 token 上限（0 表示未知，照原样存）。
func (b contextBlocks) toSnapshot(limit int) *model.TurnContextBlocks {
	return &model.TurnContextBlocks{
		SystemPrompt:    b.SystemPrompt,
		Tools:           b.Tools,
		WorkflowSummary: b.WorkflowSummary,
		Compacted:       b.Compacted,
		Detail:          b.Detail,
		UserAndCurrent:  b.UserAndCurrent,
		Total:           b.Total(),
		ContextLimit:    limit,
	}
}

// tailTurnBlocks 在压缩给出的基础分块上，叠加「尾部追加消息」的增量。
//
// base 是压缩路径给出的基础分块（历史 / 摘要 / 工作流摘要的归属已定），
// tail 是组装完成后追加到序列尾部的消息（目前只有规划期重规划轮的失败
// 反馈）。它们与当前 task 交互同属「本轮正在发生的交互」，因此只增加
// UserAndCurrent 这一块。
//
// 不为几 KB 的追加文本重跑一次压缩 —— 但它确实可能把总量顶到底线之上
// （压缩时它还不存在），所以调用方仍要用 checkWithinCeiling 兜一道。
func tailTurnBlocks(base contextBlocks, tail []llm.Message, limit int) *model.TurnContextBlocks {
	snap := base.toSnapshot(limit)
	if len(tail) == 0 {
		return snap
	}
	extra := contextmgr.GetCounter().CountMessages(tail)
	snap.UserAndCurrent += extra
	snap.Total += extra
	return snap
}

// checkWithinCeiling 校验组装结果是否在上限的 80%（底线）内。
//
// 组装路径（含压缩）已保证输出落在底线内，这里判的是「压缩之后又叠上来
// 的部分」：目前只有规划期的重规划反馈（见 tailTurnBlocks）—— 它不在压缩
// 的计量里，追加后可能把总量顶穿底线。
//
// 原先靠裁剪工具结果兜底，裁剪移除后必须显式判死 —— 否则超限请求会一路
// 送到 provider，换回一个只有错误码、看不出是哪块撑爆的 400。
func checkWithinCeiling(taskID string, blocks *model.TurnContextBlocks) error {
	if blocks == nil {
		return nil
	}
	th := contextmgr.NewThresholds(blocks.ContextLimit)
	if blocks.Total <= th.Ceiling {
		return nil
	}
	return fmt.Errorf(
		"ctx-compact[%s]: input over ceiling: sys=%d tools=%d wf=%d compacted=%d "+
			"detail=%d cur=%d total=%d limit=%d ceiling=%d",
		taskID, blocks.SystemPrompt, blocks.Tools, blocks.WorkflowSummary,
		blocks.Compacted, blocks.Detail, blocks.UserAndCurrent,
		blocks.Total, th.Limit, th.Ceiling)
}

// ============================================================
// 压缩入口
// ============================================================

// maxCompactRounds 压缩循环的硬上限。
//
// 正常情况下 1-2 轮即可达标（每轮吃掉未压缩详情的 80%，收敛很快）。
// 设上限是防「每轮都压不动却判定未达标」导致的无限循环 —— 那会把
// 一次用户请求变成无数次 LLM 调用。达到上限即按现状放行。
const maxCompactRounds = 5

// maxCurrentCompactRounds 第二级压缩（当前 task 早期交互）的硬上限。
//
// 比第一级更该设上限：它压的是模型正在用的即时上下文，压过头会明显影响
// 回答质量。5 轮配合每轮「按缺口 2 倍冗余」的选取，正常一次就能落入底线；
// 连续多轮说明缺口大得离谱（比如窗口极小而单次工具结果就有上万 token），
// 那种情况下继续压也不会有好结果，交给下面的超底线报错更诚实。
const maxCurrentCompactRounds = 5

// ============================================================
// 等待前置任务 —— 压缩的前置条件
// ============================================================
//
// 一个对话允许用户连发多个问题（每个问题一个对话任务，并行推进）。
// 压缩会把「更早的历史 task」的详情换成一份摘要，并把那些 task 标记为
// 已覆盖 —— 之后它们的 turn 不再回放。若某个更早的 task 还在跑，
// 此时压缩等于把它后半段的交互永久丢掉：用户前一个问题刚跑到一半，
// 结论就被后面的问题挤出了上下文。
//
// 因此压缩触发时要求「更早的对话任务都已收尾」，没收尾就把本轮延后
// （见 deferWaitError），由调度器按指数退避重新排队，而不是原地阻塞
// —— 阻塞会一直占着并发槽，还有被看门狗判超时的风险。

const (
	// deferBaseDelay 第一次延后的等待时长。
	//
	// 取 2s 而不是毫秒级：前置任务即便只是「一个 LLM 调用 + 一次工具调用」，
	// 也要跨 functional_call ↔ tool_call 两个调度周期（各 1s 轮询），
	// 秒级以下的退避只会换来一轮完整的「重建历史 + 计量 token」空转。
	deferBaseDelay = 2 * time.Second

	// deferMaxDelay 单次延后的上限。指数退避不能无限增长 ——
	// 前置任务若真的卡住，等待间隔越大，任务从「延后」恢复的延迟越不可控，
	// 用户感知就是「我发的消息一直不回答」。30s 是「不空转」与「能及时恢复」
	// 之间的折中。
	deferMaxDelay = 30 * time.Second

	// maxDeferCount 单个任务最多延后多少次（超过后不再等待，见下方处理）。
	//
	// 按 base=2s、cap=30s 计算，前 10 次累计约 3.5 分钟。这个预算足够覆盖
	// 「前一个问题正常跑完」，又不至于让新问题被无限期挂起：前置任务可能
	// 本身就是一个长任务，也可能已经卡死，无条件等待等于把新问题陪葬。
	maxDeferCount = 10
)

// nextDeferDelay 按已延后次数计算本次等待时长（指数退避 + 封顶）。
//
// count=0 → 2s，1 → 4s，2 → 8s，3 → 16s，4 次及以后 → 30s（封顶）。
func nextDeferDelay(deferCount int) time.Duration {
	if deferCount < 0 {
		deferCount = 0
	}
	delay := deferBaseDelay
	for i := 0; i < deferCount && delay < deferMaxDelay; i++ {
		delay *= 2
	}
	if delay > deferMaxDelay {
		delay = deferMaxDelay
	}
	return delay
}

// deferWaitError 压缩要等前置任务收尾：本轮不执行，请求延后重排。
//
// 它不是「压缩失败」—— 素材本身没问题，只是时机未到。执行器捕获它后
// 不判失败、不写 turn，而是把任务拨回本阶段 wait 并设上 delay_until。
type deferWaitError struct {
	// TaskIDs 仍在跑的、更早的对话任务
	TaskIDs []string
}

func (e *deferWaitError) Error() string {
	return fmt.Sprintf("等待前置任务收尾后再压缩上下文（未完成：%s）",
		strings.Join(e.TaskIDs, ", "))
}

// compactInput 上下文压缩所需的全部输入。
type compactInput struct {
	Temp         *model.TaskTemp
	SystemPrompt string
	UserText     string
	Tools        []llm.Tool
	// Client 用于生成摘要的 LLM 客户端。按决策复用当前对话所选模型，
	// 由调用方传入已解析好的客户端，避免这里重复解析。
	Client llm.Client
	// CompactBudget 单次压缩调用可占用的输入 token 预算。
	//
	// 压缩本身也是一次 LLM 调用，同样受模型上下文约束：从大窗口模型
	// 切到小窗口模型后，待压缩素材可能远超新模型的上限，整包一次调用
	// 必然被 provider 拒绝。素材超过此预算时按此值分块，走多层压缩。
	// 由 assembleWithCompaction 按 resolveChatContextLimit 的结果算出。
	CompactBudget int

	// CompactExtraBody 压缩调用要额外注入的请求体字段。
	//
	// 目前只有一处用途：给混元下发 chat_template_kwargs 关掉深度思考
	// （见 resolveCompactClient）。nil 表示不注入任何额外字段。
	CompactExtraBody map[string]interface{}

	// Isolated 子任务视角：不带对话历史，压缩水位独立于对话。
	//
	// 工作流节点子任务走这条路径。它与对话任务的区别**只有两处**：
	//   1. 历史段为空 —— 它刻意不看主对话历史，上下文只由「工作流目标 +
	//      本步骤说明 + 上游输出 + 用户附件 + 本节点自己的交互」构成；
	//   2. 压缩水位存在 task_context_summary 而不是 context_summary ——
	//      同一对话下的多个节点是并发跑的，共享一行会互相覆盖。
	//
	// 除此之外（触发水位、选取策略、分块归属、底线判死）与对话任务完全
	// 同一套规则 —— 子任务同样会经历几十轮工具循环，同样会把窗口撑爆。
	Isolated bool
}

// assembleWithCompaction 组装消息序列，必要时先压缩历史。
//
// 返回最终可直接送给 provider 的消息序列，以及最终消息序列的分块计量
// （blocks / limit），供调用方随 turn 快照落库 —— 它就是本次 LLM 调用
// 真实送出的输入构成，调试页与「上下文用量」弹窗的数据源。
//
// 压缩失败一律返回 error，让任务直接失败：
// 压缩是唯一的超限兜底，它失败后输入必然超限，继续送 provider 只会换来
// 一个更难定位的 400。明确报错能让用户在前端立刻看到「上下文压缩超时」，
// 而不是等 provider 拒绝后去猜。
func assembleWithCompaction(ctx context.Context, in compactInput) ([]llm.Message, contextBlocks, int, error) {
	temp := in.Temp
	counter := contextmgr.GetCounter()

	// 上限：优先取模型配置，漏配时按模型名查内置表兜底
	limit := resolveChatContextLimit(ctx, temp.ChatID)
	th := contextmgr.NewThresholds(limit)

	// 固定不变的部分先算一次，压缩循环里复用
	fixed := contextBlocks{
		SystemPrompt: counter.CountText(in.SystemPrompt),
		Tools:        counter.CountTools(in.Tools),
	}

	// 当前 task 压缩水位的落库动作。两种作用域写不同的表，但压缩循环
	// 只认这一个回调 —— 规则不因作用域分叉。
	saveCurrent := func(ctx context.Context, s *model.ContextSummary) error {
		return saveContextSummary(ctx, s)
	}

	var summary *model.ContextSummary
	if in.Isolated {
		// 子任务：对话级摘要与它无关（历史段为空），但**自己的**水位要读。
		// 载体复用 ContextSummary 的 current_* 四个字段，落库时转成
		// task_context_summary 行 —— 压缩循环不必知道存到了哪张表。
		summary = &model.ContextSummary{ChatID: temp.ChatID}
		taskSummary, err := loadTaskContextSummary(ctx, temp.TaskID)
		if err != nil {
			// 读不出来只影响压缩起点：降级成「没压过」，本轮会重新压一遍，
			// 不会丢数据（原始交互仍在 task_turn 里）。
			log.Printf("[ctx-compact][%s] load task compaction watermark failed (ignore): %v",
				temp.TaskID, err)
		} else if taskSummary != nil {
			summary.CurrentTaskID = temp.TaskID
			summary.CurrentCompactedMsgs = taskSummary.CompactedMsgs
			summary.CurrentSummary = taskSummary.Summary
			summary.CurrentSummaryTokens = taskSummary.SummaryTokens
			summary.CurrentCompactedTurnIDs = taskSummary.CompactedTurnIDs
			summary.CompactCount = taskSummary.CompactCount
			summary.OriginalTokens = taskSummary.OriginalTokens
		}
		saveCurrent = func(ctx context.Context, s *model.ContextSummary) error {
			return saveTaskContextSummary(ctx, &model.TaskContextSummary{
				TaskID:           temp.TaskID,
				CompactedMsgs:    s.CurrentCompactedMsgs,
				Summary:          s.CurrentSummary,
				SummaryTokens:    s.CurrentSummaryTokens,
				CompactedTurnIDs: s.CurrentCompactedTurnIDs,
				CompactCount:     s.CompactCount,
				OriginalTokens:   s.OriginalTokens,
			})
		}
	} else {
		loaded, loadErr := loadContextSummary(ctx, temp.ChatID)
		if loadErr != nil {
			// 摘要读不出来只影响压缩，不影响历史重建：降级成「无摘要」
			log.Printf("[ctx-compact][%s] load summary failed (ignore): %v", temp.TaskID, loadErr)
		}
		summary = loaded

		// 换 task 时把上一轮遗留的当前上下文水位归位。
		//
		// 用户每发一条新消息都会开一个新 task；旧 task 已结束，它那份
		// 「早期交互摘要」从此就是普通历史 —— 并入历史摘要，并把旧 task
		// 标记为已覆盖，避免它的剩余交互又原样回放一遍（那会和摘要重复）。
		// 不归位的话，水位里的 task ID 永远对不上当前 task，第二级压缩
		// 会一直认为「当前 task 没压过」，每次都从头压起。
		if summary.HasCurrentCompaction() && summary.CurrentTaskID != temp.TaskID {
			log.Printf("[ctx-compact][%s] roll over current compaction from finished task %s",
				temp.TaskID, summary.CurrentTaskID)
			summary.RollOverCurrent()
			if err := saveContextSummary(ctx, summary); err != nil {
				return nil, contextBlocks{}, 0, fmt.Errorf(
					"ctx-compact[%s]: roll over current compaction: %w", temp.TaskID, err)
			}
			globalAgentHistoryCache.delete(temp.TaskID)
		}
	}

	// 首次组装：先看压缩能不能解决。
	res, err := buildAssembled(ctx, in, fixed, summary, counter)
	if err != nil {
		return nil, contextBlocks{}, 0, err
	}
	blocks := res.blocks

	if blocks.Total() <= th.Trigger {
		log.Printf("[ctx-compact][%s] within budget: %s limit=%d trigger=%d",
			temp.TaskID, blocks, th.Limit, th.Trigger)
		return res.msgs, blocks, limit, nil
	}

	log.Printf("[ctx-compact][%s] over trigger, start compaction: %s limit=%d trigger=%d target=%d",
		temp.TaskID, blocks, th.Limit, th.Trigger, th.Target)

	// 压缩的前置条件：更早的对话任务必须都已收尾。
	//
	// 子任务（Isolated）没有历史段，不受影响 —— 它压的是自己早前的交互，
	// 不存在「别人的结果还没出来」这回事。
	if !in.Isolated {
		pending, perr := funcCallRegistry.taskDAO.ListUnfinishedPredecessors(ctx, temp.ChatID, temp.CreatedAt)
		if perr != nil {
			// 查不出来不能因此判死任务：退化为「当作没有前置任务」，
			// 最坏情况是压掉一个仍在跑的历史 task 的后半段（回到旧行为），
			// 好过因为一次查询失败就把用户的问题失败掉。
			log.Printf("[ctx-compact][%s] list unfinished predecessors failed (compact anyway): %v",
				temp.TaskID, perr)
		} else if len(pending) > 0 {
			ids := make([]string, 0, len(pending))
			for _, t := range pending {
				ids = append(ids, t.TaskID)
			}
			if temp.DeferCount < maxDeferCount {
				return nil, contextBlocks{}, 0, &deferWaitError{TaskIDs: ids}
			}
			// 退避预算用尽：不再等待，但**绝不**把这些未完成的任务压进摘要。
			//
			// 压进去的代价是不可逆的 —— 它们后续产生的 turn 会被 covered
			// 过滤掉，前一个问题就等于白跑。宁可少压一点（详情仍留在上下文里、
			// 可能因此触到超底线报错），也不能静默丢掉用户已经等到的结果。
			log.Printf("[ctx-compact][%s] defer budget exhausted (%d/%d), "+
				"compact without unfinished predecessors: %v",
				temp.TaskID, temp.DeferCount, maxDeferCount, ids)
			res.segs = excludeSegments(res.segs, ids)
		}
	}

	// 压缩会调 LLM、耗时可观（几秒到几十秒），必须让前端知道在干什么，
	// 否则用户看到的是「卡住不动」
	publishCompacting(temp.TaskID, "start", blocks.Total(), th.Limit, "")
	defer func() {
		publishCompacting(temp.TaskID, "done", blocks.Total(), th.Limit, "")
	}()

	// 压缩调用换用专用客户端：更长超时 + 关闭深度思考。
	//
	// 复用主对话客户端时压缩几乎必然失败 —— 混元默认开深度思考，思考链
	// 会先把 60s 的 http 超时吃满（实测 context_summary 表长期 0 行）：
	// 压缩是忠实转述，不需要推理，关掉思考后耗时降一个数量级。
	compactClient, extraBody, err := resolveCompactClient(ctx, temp.ChatID)
	if err != nil {
		return nil, contextBlocks{}, 0, err
	}
	in.Client = compactClient
	in.CompactExtraBody = extraBody

	// 素材放不下时按此预算分块。取「上限的 60%」：输入 0.6 + 输出 0.1
	// = 0.7，留 30% 吸收 tiktoken 的计数误差与 provider 额外开销。
	in.CompactBudget = th.CompactCall

	// 第一级：压历史 task。
	//
	// 子任务走这里等于空转一圈（没有历史段，选不出东西，第一轮就 break），
	// 随后落到下面的第二级。保留同一条通路而不是为它开分支：触发水位、
	// 重建与判死都是同一套，多一次选段调用的代价可以忽略。
	for round := 1; round <= maxCompactRounds; round++ {
		// 选出本轮要压缩的 task（未压缩详情的前 80%，对齐 task 边界）
		picked, pickedTokens := pickSegmentsToCompact(res.segs, blocks.Detail)
		if len(picked) == 0 {
			log.Printf("[ctx-compact][%s] round %d: nothing left to compact, stop at %d",
				temp.TaskID, round, blocks.Total())
			break
		}

		newSummary, err := compactOnce(ctx, in, summary, picked, th.Summary)
		if err != nil {
			// 压缩失败即放弃本轮请求：没有别的兜底手段了，带着超限输入
			// 去调 provider 只会换来一个更难定位的 400。
			return nil, contextBlocks{}, 0, fmt.Errorf(
				"ctx-compact[%s]: round %d compaction failed: %w", temp.TaskID, round, err)
		}

		// 累加覆盖集合：新摘要覆盖「原有覆盖的 task」+「本轮压缩的 task」
		covered := summary.CoveredTasks()
		ids := make([]string, 0, len(covered)+len(picked))
		for id := range covered {
			ids = append(ids, id)
		}
		// 索引项必须在压掉之前取标签：一旦进了摘要，原消息就看不到了。
		indexAdd := make([]model.TaskIndexEntry, 0, len(picked))
		for _, s := range picked {
			indexAdd = append(indexAdd, model.TaskIndexEntry{
				TaskID: s.TaskID,
				Label:  segmentLabel(s.Messages),
			})
		}
		originalTokens := 0
		if summary != nil {
			originalTokens = summary.OriginalTokens
		}
		compactCount := 0
		if summary != nil {
			compactCount = summary.CompactCount
		}
		for _, s := range picked {
			ids = append(ids, s.TaskID)
		}

		next := &model.ContextSummary{
			ChatID:         temp.ChatID,
			Summary:        newSummary,
			SummaryTokens:  counter.CountText(newSummary),
			CompactCount:   compactCount + 1,
			OriginalTokens: originalTokens + pickedTokens,
		}
		// 索引是代码产物：先继承旧索引，再追加本轮被压掉的任务。
		// （summary 可能是 nil —— 首次压缩或读取失败时都是如此，
		//  此时没有旧索引可继承。）
		if summary != nil {
			next.CoveredTaskIndex = summary.CoveredTaskIndex
		}
		next.AppendCoveredTaskIndex(indexAdd...)
		if err := next.SetCoveredTasks(ids); err != nil {
			return nil, contextBlocks{}, 0, fmt.Errorf(
				"ctx-compact[%s]: round %d marshal covered tasks: %w", temp.TaskID, round, err)
		}
		if err := saveContextSummary(ctx, next); err != nil {
			// 存不下去就别继续：下一轮重建历史仍会读到旧摘要，
			// 会把同一段历史反复压缩。同理直接失败，不静默放行。
			return nil, contextBlocks{}, 0, fmt.Errorf(
				"ctx-compact[%s]: round %d save summary: %w", temp.TaskID, round, err)
		}
		summary = next

		// before 段缓存里存的是压缩前的历史，必须失效，
		// 否则下一轮 turn 会继续复用未压缩的旧历史，压缩等于白做
		globalAgentHistoryCache.delete(temp.TaskID)

		// 重建并重算水位
		res, err = buildAssembled(ctx, in, fixed, summary, counter)
		if err != nil {
			return nil, contextBlocks{}, 0, err
		}
		blocks = res.blocks
		log.Printf("[ctx-compact][%s] round %d done: compacted %d tasks (%d tokens) -> summary %d tokens; now %s",
			temp.TaskID, round, len(picked), pickedTokens, next.SummaryTokens, blocks)

		if blocks.Total() <= th.Target {
			log.Printf("[ctx-compact][%s] reached target after %d round(s)", temp.TaskID, round)
			return res.msgs, blocks, limit, nil
		}

		// 降级判定：不可压部分已经顶到目标线之上时，Target 永远达不成。
		// 只要落到 Ceiling 以下就算通过，避免无意义的空转压缩。
		if blocks.Incompressible() >= th.Target && blocks.Total() <= th.Ceiling {
			log.Printf("[ctx-compact][%s] incompressible=%d exceeds target=%d; "+
				"accept degraded result total=%d (<= ceiling %d)",
				temp.TaskID, blocks.Incompressible(), th.Target, blocks.Total(), th.Ceiling)
			return res.msgs, blocks, limit, nil
		}
	}

	// ============================================================
	// 第二级：压当前 task 的早期交互
	// ============================================================
	//
	// 走到这里说明「只压历史 task」已经无解 —— 不可压部分（含当前 task 的
	// 全量交互）本身就顶穿了底线。典型形态是一个跑了几十轮的任务：
	// 历史早就压干净了，超限的全是它自己累积的工具结果。
	//
	// 工作流节点子任务**只走这一级**：它没有历史段，第一级空转一圈后必然
	// 落到这里。节点同样会跑几十轮工具循环（抓网页、读文件、跑命令），
	// 攒起来的上下文一样能把窗口撑爆 —— 此前它超限就判死，是最容易撞上
	// 的一类任务（system + tools 固定开销之外，只剩自己的交互在涨）。
	//
	// 只在第一级压完仍超底线时才触发：正常情况下当前 task 的完整交互
	// 是模型决策最关键的依据，不该无谓地降级成摘要。
	if blocks.Total() > th.Ceiling {
		if summary == nil {
			summary = &model.ContextSummary{ChatID: temp.ChatID}
		}
		res, err = compactCurrentRounds(ctx, compactionRun{
			in:          in,
			th:          th,
			fixed:       fixed,
			saveCurrent: saveCurrent,
		}, res, summary)
		if err != nil {
			return nil, contextBlocks{}, 0, err
		}
		blocks = res.blocks
	}

	if blocks.Total() > th.Ceiling {
		// 压到极限仍超底线：直接失败。
		//
		// 此前这里会把工具结果裁进底线（截断 + 落盘 read_file 指引），
		// 已按用户决策移除 —— 模型 read_file 会把原文原封不动读回来，
		// 裁剪只是把超限风险推后一轮，同时让上下文变得不可信。
		// 与其送出必然被拒的请求，不如把「压不动」如实报出来。
		return nil, contextBlocks{}, 0, fmt.Errorf(
			"ctx-compact[%s]: context still over ceiling after compaction: %s limit=%d ceiling=%d",
			temp.TaskID, blocks, th.Limit, th.Ceiling)
	}
	return res.msgs, blocks, limit, nil
}

// ============================================================
// 第二级压缩 —— 压当前 task 的早期交互
// ============================================================

// compactionRun 一次压缩过程的不变量。
//
// 抽成结构体是因为第二级压缩要服务两种作用域：对话任务把水位写进
// context_summary，工作流节点子任务写进 task_context_summary（并发节点
// 不能共用一行）。除「存到哪张表」外规则完全一致，因此循环只写一份，
// 差异收敛到 saveCurrent 这一个回调里。
type compactionRun struct {
	in    compactInput
	th    contextmgr.Thresholds
	fixed contextBlocks
	// saveCurrent 落库当前 task 的压缩水位。
	saveCurrent func(context.Context, *model.ContextSummary) error
}

// compactCurrentRounds 反复压缩当前 task 的早期交互，直到落入底线或压不动。
//
// 返回最后一次重建的组装结果（含更新后的 curCut 与分块）。summary 是水位
// 载体，会被就地推进；调用方持有的指针同步生效。
func compactCurrentRounds(
	ctx context.Context, run compactionRun, res *assembledResult,
	summary *model.ContextSummary,
) (*assembledResult, error) {
	temp := run.in.Temp
	counter := contextmgr.GetCounter()

	for round := 1; round <= maxCurrentCompactRounds; round++ {
		if res.blocks.Total() <= run.th.Ceiling {
			break
		}
		from := res.curCut
		cut := pickCurrentCut(res.current, from, res.blocks.Total()-run.th.Ceiling, counter)
		if cut <= from {
			log.Printf("[ctx-compact][%s] current compaction round %d: "+
				"no more current messages to compact, stop at %d",
				temp.TaskID, round, res.blocks.Total())
			break
		}

		// 复用历史压缩的同一条通路：把「旧的当前摘要 + 新纳入的交互」
		// 合成一份新摘要。传一个只带摘要文本的临时对象即可，
		// compactOnce 只关心 summary.Summary / SummaryTokens。
		prev := &model.ContextSummary{
			ChatID:        temp.ChatID,
			Summary:       summary.CurrentSummary,
			SummaryTokens: summary.CurrentSummaryTokens,
		}
		part := res.current[from:cut]
		picked := []historySegment{{
			TaskID:   temp.TaskID,
			Kind:     segKindConversation,
			Messages: part,
			Tokens:   counter.CountMessages(part),
		}}

		newSummary, err := compactOnce(ctx, run.in, prev, picked, run.th.Summary)
		if err != nil {
			return nil, fmt.Errorf(
				"ctx-compact[%s]: current compaction round %d failed: %w",
				temp.TaskID, round, err)
		}

		next := *summary
		next.CurrentTaskID = temp.TaskID
		next.CurrentCompactedMsgs = cut
		next.CurrentSummary = newSummary
		next.CurrentSummaryTokens = counter.CountText(newSummary)
		// 记下这批消息来自哪些 turn：摘要不可逆，但按 turn_id 还能取回原文。
		next.SetCurrentCompactedTurns(
			appendTurnIDs(summary.CurrentCompactedTurns(), collectCompactedTurns(part)))
		next.CompactCount = summary.CompactCount + 1
		next.OriginalTokens = summary.OriginalTokens + picked[0].Tokens
		if err := run.saveCurrent(ctx, &next); err != nil {
			return nil, fmt.Errorf(
				"ctx-compact[%s]: current compaction round %d save summary: %w",
				temp.TaskID, round, err)
		}
		*summary = next
		globalAgentHistoryCache.delete(temp.TaskID)

		res, err = buildAssembled(ctx, run.in, run.fixed, summary, counter)
		if err != nil {
			return nil, err
		}
		log.Printf("[ctx-compact][%s] current compaction round %d: "+
			"compacted msgs [%d,%d) (%d tokens) -> summary %d tokens; now %s",
			temp.TaskID, round, from, cut, picked[0].Tokens, next.CurrentSummaryTokens, res.blocks)
	}
	return res, nil
}

// ============================================================
// turn 组装入口 —— 规划期 / 执行期 / 工作流节点子任务共用
// ============================================================
//
// 三条路径的上下文规则本就该是同一套：同样的历史压缩、同样的分块
// 归属、同样的底线判死。此前规划期在压缩之外还自己管了一摊（记基础
// 长度、按增量补算反馈、自己判死），执行期则由 assembleExecutingMessages
// 管，子任务更是自己拼装 + 自己计量 —— 三处规则一旦有一处先改，
// 另外两处就悄悄不一致了。
//
// 因此把「压缩组装 → 追加尾部消息 → 折叠 → 计量 → 判死」整条链路收到
// 这一个入口，三者都只调它，规则不可能再分叉。
//
// 子任务与其他两者的差别只有「带不带对话历史、水位存哪张表」，由
// turnAssembleInput.Isolated 一个开关表达，压缩规则本身不分支。

// turnAssembleInput 一次 LLM turn 的组装输入。
type turnAssembleInput struct {
	Temp         *model.TaskTemp
	SystemPrompt string
	UserText     string
	Tools        []llm.Tool
	Client       llm.Client

	// Tail 组装完成后追加到序列尾部的消息。
	//
	// 目前只有规划期的重规划轮会用到：模型提交的 submit_plan 被拒后，
	// 要把「它提交的那次调用 + 校验反馈」追加到尾部再重调一次。
	// 这部分在压缩时还不存在，故不参与压缩计量；但它是本轮真实输入，
	// 必须计入总量并参与底线判定，否则会带着超限请求去调 provider。
	//
	// 调用方按轮次累积后整体传入，而不是「先取回 messages 再自己 append」：
	// 追加动作必须伴随重新折叠（见 assembleTurnMessages），交给调用方做
	// 就又变回两处各自维护了。
	Tail []llm.Message

	// Isolated 子任务视角：组装时**不带对话历史**，压缩水位独立存
	// （见 compactInput.Isolated）。工作流节点子任务置 true，其余留 false。
	Isolated bool
}

// turnAssembleResult 一次 turn 组装的产物。
type turnAssembleResult struct {
	// Messages 可直接送 provider 的消息序列（已折叠相邻同角色）。
	Messages []llm.Message
	// Blocks 本次输入的分块计量，已含 Tail 的增量，可直接随 turn 快照落库。
	Blocks *model.TurnContextBlocks
}

// assembleTurnMessages 组装一次 LLM turn 的消息序列，必要时先压缩。
//
// 规划期 / 执行期 / 工作流节点子任务共用：三者都只准备自己的 system
// prompt / 工具 / 用户输入，剩下的（压不压、怎么分块、超不超限）全部由
// 这里按同一套规则决定。
func assembleTurnMessages(
	ctx context.Context, in turnAssembleInput,
) (*turnAssembleResult, error) {
	msgs, blocks, limit, err := assembleWithCompaction(ctx, compactInput{
		Temp:         in.Temp,
		SystemPrompt: in.SystemPrompt,
		UserText:     in.UserText,
		Tools:        in.Tools,
		Client:       in.Client,
		Isolated:     in.Isolated,
	})
	if err != nil {
		return nil, err
	}
	return applyTail(in.Temp.TaskID, msgs, blocks, in.Tail, limit)
}

// applyTail 把尾部消息追加到已组装的序列上：重新折叠、叠加计量、判死。
//
// 抽成纯函数是为了可测 —— 它不碰 DB、不调 LLM，可以完全用构造数据验证；
// assembleTurnMessages 只是「压缩组装 + 本函数」的组合。
func applyTail(
	taskID string, msgs []llm.Message, base contextBlocks,
	tail []llm.Message, limit int,
) (*turnAssembleResult, error) {
	if len(tail) == 0 {
		return &turnAssembleResult{Messages: msgs, Blocks: base.toSnapshot(limit)}, nil
	}

	// 尾部消息只增加「当前交互」一块，并可能把总量顶到底线之上
	// （压缩时它还不存在），因此仍要判死 —— 与其送 provider 吃一个
	// 看不出原因的 400，不如在这里把「哪块撑爆了」如实报出来。
	snap := tailTurnBlocks(base, tail, limit)
	if err := checkWithinCeiling(taskID, snap); err != nil {
		return nil, err
	}

	// 追加后必须重新折叠：Tail 以 assistant（模型刚提交的 submit_plan）
	// 开头，而序列末尾可能正好也是 assistant（当前 task 摘要），
	// 不折叠会留下连续 assistant —— Gemini 协议直接判 400。
	//
	// 复制一份再 append：msgs 的底层数组容量不保证，就地追加可能
	// 污染压缩路径内部仍持有的切片。
	out := make([]llm.Message, 0, len(msgs)+len(tail))
	out = append(out, msgs...)
	out = append(out, tail...)
	return &turnAssembleResult{
		Messages: collapseAdjacentSameRole(out),
		Blocks:   snap,
	}, nil
}

// pickCurrentCut 选出当前 task 要压缩到哪一条消息（返回切点下标）。
//
// needTokens 是「至少要腾出多少原文 token」。按 token 累计选而非按条数：
// 一条 fetch 结果能占几千 token，一句 assistant 短回复只有几十，
// 按条数切会出现「压了 20 条还是没腾出空间」或「压 1 条直接过头」。
//
// 取 2 倍冗余：压掉 X 原文后摘要本身还要占一部分（实测压缩比约 0.6），
// 净省只有 40%~50%，按 1:1 选必然不够、要多跑几轮 LLM。宁可一轮多压一点。
func pickCurrentCut(current []llm.Message, from, needTokens int, counter *contextmgr.Counter) int {
	if needTokens <= 0 || from >= len(current) {
		return from
	}
	target := needTokens * 2
	acc := 0
	n := from
	for n < len(current) && acc < target {
		acc += counter.CountMessages(current[n : n+1])
		n++
	}
	// 尾部保留下限：默认留 2 条原文，让模型能看到「刚刚发生了什么」。
	//
	// 但若已经压到只剩 2 条还落不进底线（典型：倒数第二条是几千 token 的
	// fetch 结果），让步到只留 1 条 —— 不让步就永远差最后一口气，
	// 明明再压一条就能进底线，却卡在报错上。全压光是不允许的：
	// alignCurrentSplitPoint 只会把切点推过孤儿 tool，不会凭空清空。
	keepTail := 2
	if from >= len(current)-keepTail {
		keepTail = 1
	}
	if n > len(current)-keepTail {
		n = len(current) - keepTail
	}
	return alignCurrentSplitPoint(current, n)
}

// buildAssembled 重建历史、拼装消息序列并计量各块。
//
// 返回的 segs 只含「可压缩的对话段」（已排除工作流摘要段），
// 供压缩选取直接使用。
// assembledResult 一次历史重建的完整产物。
//
// 除了消息序列本身，还要带上「当前 task 交互序列」与「它的压缩切点」：
// 第二级压缩要在这条序列上选切点、算能腾出多少 token，
// 光有拼好的 msgs 反推不回来（中间已注入了摘要与 user 消息）。
type assembledResult struct {
	msgs    []llm.Message
	blocks  contextBlocks
	segs    []historySegment // 可压缩的历史段（已排除工作流摘要段）
	current []llm.Message    // 当前 task 的完整交互序列
	curCut  int              // current 中已被摘要代表的消息条数
}

func buildAssembled(
	ctx context.Context, in compactInput, fixed contextBlocks,
	summary *model.ContextSummary, counter *contextmgr.Counter,
) (*assembledResult, error) {
	temp := in.Temp
	covered := summary.CoveredTasks()

	var segs []historySegment
	var current []llm.Message
	var err error
	if in.Isolated {
		// 子任务视角：只有本 task 自己已发生的交互，不带对话历史。
		//
		// 不能复用 buildSegmentedHistory —— 它按 chat 拉全量 turn，会把主
		// 对话的历史 task 全带进来；而且子任务自己的 turn 带 parent_task_id，
		// 在那条路径里被当成「别人的子任务」跳过了（还会顺手注入工作流摘要）。
		current, err = buildCurrentHistory(ctx, funcCallRegistry.turnDAO, temp.TaskID)
	} else {
		segs, current, err = buildSegmentedHistory(ctx,
			funcCallRegistry.turnDAO, funcCallRegistry.taskDAO,
			temp.ChatID, temp.TaskID, covered)
	}
	if err != nil {
		return nil, fmt.Errorf("func-call-actuator: %w", err)
	}

	userMsg := llm.Message{Role: llm.RoleUser, Content: in.UserText}

	blocks, compressible := classifyHistorySegments(segs, counter, fixed)

	// 当前 task 的压缩水位（第二级压缩的产物）：
	// 前 curCut 条交互已被 CurrentSummary 代表，不再逐条回放。
	//
	// 水位只在「还是同一个 task」时有效 —— 换了 task 说明旧 task 已结束，
	// 它的那份摘要会在 rollover 时并入历史摘要（见 rollOverCurrentCompaction）。
	curCut := 0
	if summary.HasCurrentCompaction() && summary.CurrentTaskID == temp.TaskID {
		curCut = alignCurrentSplitPoint(current, summary.CurrentCompactedMsgs)
	}

	blocks.UserAndCurrent = counter.CountMessages(append([]llm.Message{userMsg}, current[curCut:]...))

	// 组装顺序：system → 已压缩历史摘要 → 未压缩历史详情（含工作流摘要段）
	//          → 本轮 user → 当前 task 已压摘要 → 当前 task 未压交互
	//
	// 历史摘要置于历史最前：它代表的是被它吃掉的那批最早的 task，
	// 放在最前才与剩余历史保持时序一致。
	// 当前 task 摘要紧贴在它代表的那些交互原本的位置上（即本轮 user 之后、
	// 剩余交互之前），两者时序都不能乱。
	out := make([]llm.Message, 0, len(segs)+8)
	out = append(out, llm.Message{Role: llm.RoleSystem, Content: in.SystemPrompt})

	if summary != nil && strings.TrimSpace(summary.Summary) != "" {
		blocks.Compacted = summary.SummaryTokens
		if blocks.Compacted == 0 {
			blocks.Compacted = counter.CountText(summary.Summary)
		}
		text := summary.Summary
		// 索引由代码拼在摘要外层：LLM 只压正文，不碰 ID。
		if idx := renderTaskIndex(coveredTaskIndex(summary, covered)); idx != "" {
			text += "\n\n" + idx
			blocks.Compacted += counter.CountText(idx)
		}
		out = append(out, llm.Message{
			Role:    llm.RoleAssistant,
			Content: text,
		})
	}

	out = append(out, flattenSegments(segs)...)
	out = append(out, userMsg)

	if curCut > 0 {
		// 与历史摘要同属「已压缩」块：对前端而言它们没有区别，
		// 六块模型保持不变，各块之和仍等于 Total。
		blocks.Compacted += summary.CurrentSummaryTokens
		text := summary.CurrentSummary
		if idx := renderTurnIndex(summary.CurrentCompactedTurns()); idx != "" {
			text += "\n" + idx
			blocks.Compacted += counter.CountText(idx)
		}
		out = append(out, llm.Message{
			Role:    llm.RoleAssistant,
			Content: text,
		})
	}

	out = append(out, current[curCut:]...)

	// 折叠相邻同角色消息：摘要注入会在序列里制造连续的 assistant。
	// 计量在折叠之前完成，因此 blocks 是「折叠前」的偏大估计 ——
	// 偏保守（少算可用空间）比偏乐观安全。
	out = collapseAdjacentSameRole(out)

	return &assembledResult{
		msgs:    out,
		blocks:  blocks,
		segs:    compressible,
		current: current,
		curCut:  curCut,
	}, nil
}

// ============================================================
// 压缩产物的「取回索引」
// ============================================================
//
// 压缩是有损的：被压掉的消息从此只剩一段摘要。索引的作用是让它变成
// 「可逆」—— 摘要旁边附上句柄（task_id / turn_id），后续想回看时有据可依。
//
// 两条硬规则：
//  1. 索引只能由代码拼装。压缩走的是多层 map-reduce，中间 LLM 会丢 ID
//     甚至编造 ID，把 ID 写进摘要正文等于把句柄交给幻觉。
//  2. 注入要有上限。ID 列表会随对话变长，长任务压一次能攒上百个 turn，
//     全量注入反而在挤占本就紧张的窗口（小窗口模型尤其）。

// maxInjectedIndexEntries 索引注入上下文的最大条数（超出只保留最近的）。
const maxInjectedIndexEntries = 20

// coveredTaskIndex 取覆盖任务的索引。
//
// 存量兜底：索引列是后加的，它上线前压缩过的对话只有 covered_task_ids
// 没有索引列，标签已经无从还原（原消息早被压掉了）。此时退化成「只给 ID、
// 没有标签」也比什么都不给强 —— 句柄才是关键，标签只是让它更好认。
// 这样省掉一次全量回填，老对话下次压缩时会自动补上真正的标签。
func coveredTaskIndex(
	summary *model.ContextSummary, covered map[string]bool,
) []model.TaskIndexEntry {
	entries := summary.CoveredTaskIndexEntries()
	if len(entries) > 0 || len(covered) == 0 {
		return entries
	}
	for id := range covered {
		entries = append(entries, model.TaskIndexEntry{TaskID: id})
	}
	// map 遍历顺序随机，排序保证同一对话每轮注入的文本稳定
	sort.Slice(entries, func(i, j int) bool { return entries[i].TaskID < entries[j].TaskID })
	return entries
}

// renderTaskIndex 渲染「已被压缩的历史任务」索引。
//
// entries 按时间序，超出上限时保留最近的：刚压掉的更可能被回看，
// 且它们对应的是最近的话题。
func renderTaskIndex(entries []model.TaskIndexEntry) string {
	if len(entries) == 0 {
		return ""
	}
	start := 0
	if len(entries) > maxInjectedIndexEntries {
		start = len(entries) - maxInjectedIndexEntries
	}
	var b strings.Builder
	b.WriteString("【已压缩历史索引】以上摘要覆盖以下任务。摘要有损，若需要其中某轮的原始细节" +
		"（原始报错、命令输出、代码片段、具体数值等），用 " + model.RecallContextToolName +
		"(task_id=\"…\", query=\"要查什么\") 取回：\n")
	for _, e := range entries[start:] {
		if e.Label != "" {
			fmt.Fprintf(&b, "- task_id=%s ｜ %s\n", e.TaskID, e.Label)
		} else {
			fmt.Fprintf(&b, "- task_id=%s\n", e.TaskID)
		}
	}
	if start > 0 {
		fmt.Fprintf(&b, "（更早还有 %d 个已被压缩的任务，未逐一列出）\n", start)
	}
	return b.String()
}

// renderTurnIndex 渲染「当前 task 已被压缩的 turn」索引。
func renderTurnIndex(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	start := 0
	if len(ids) > maxInjectedIndexEntries {
		start = len(ids) - maxInjectedIndexEntries
	}
	parts := make([]string, 0, len(ids)-start)
	for _, id := range ids[start:] {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	out := "（以上摘要覆盖的 turn_id：" + strings.Join(parts, ", ") + "）"
	if start > 0 {
		out += fmt.Sprintf("（更早还有 %d 个，未逐一列出）", start)
	}
	// 必须说明用途：只给一串数字，模型不知道能拿它做什么，这段索引就是纯占位。
	// 工具名从 model 常量取，保证与真实注册名一致 —— 写死字符串一旦改名
	// 就会诱导模型调用不存在的工具。
	out += "\n需要其中某轮的原始内容时用 " + model.RecallContextToolName +
		"(turn_id=…) 取回原文。"
	return out
}

// segmentLabel 取一段消息的首条 user 提问作为任务标签。
//
// 只取 user：任务标签要说清「用户当时问了什么」，assistant/tool 的内容
// 是过程。只取首行：user 消息常带附件清单等多行内容，全塞进索引太吵。
func segmentLabel(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		s := strings.TrimSpace(m.Content)
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
		runes := []rune(s)
		if len(runes) > 40 {
			s = string(runes[:40]) + "…"
		}
		return s
	}
	return ""
}

// collectCompactedTurns 汇总本次被压掉的消息所属的 turn ID，去掉重复与零值。
//
// 多条消息可能来自同一 turn（一次 LLM 调用既产出文本又产出工具调用），
// 因此必须去重；未标来源的消息（零值）直接跳过 —— 它们没有可回取的原始记录。
func collectCompactedTurns(msgs []llm.Message) []int64 {
	seen := make(map[int64]bool, len(msgs))
	out := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		id := m.SourceTurnID
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// appendTurnIDs 把新增的 turn ID 追加进已有列表并保持去重。
func appendTurnIDs(existing, added []int64) []int64 {
	seen := make(map[int64]bool, len(existing)+len(added))
	for _, id := range existing {
		seen[id] = true
	}
	out := append([]int64{}, existing...)
	for _, id := range added {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// classifyHistorySegments 逐段计量并归类，返回累加后的分块与可压段集合。
//
// 工作流摘要段单独计入 WorkflowSummary 而不是 Detail：
// 「上下文用量」弹窗与调试页按块展示，若把摘要混进 Detail，有工作流的对话
// 会显示工作流占比 0 —— 摘要明明在输入里，只是被记错了块（实测 chat
// 498d0d1e 规划期：输入含工作流摘要 118 token，workflow_summary 却计成 0）。
//
// 段级 token 缓存进 seg.Tokens，压缩选取时不必重新编码。
func classifyHistorySegments(
	segs []historySegment, counter *contextmgr.Counter, base contextBlocks,
) (contextBlocks, []historySegment) {
	blocks := base
	var compressible []historySegment
	for i := range segs {
		segs[i].Tokens = counter.CountMessages(segs[i].Messages)
		switch segs[i].Kind {
		case segKindWorkflowSummary:
			blocks.WorkflowSummary += segs[i].Tokens
		default:
			blocks.Detail += segs[i].Tokens
			compressible = append(compressible, segs[i])
		}
	}
	return blocks, compressible
}

// excludeSegments 从可压缩段里剔除指定 task 的段。
//
// 用于「前置任务还没跑完、又不想再等」的降级路径：它们的详情仍留在上下文
// 里（Detail 不变，不会被丢），只是本次不参与压缩。
func excludeSegments(segs []historySegment, taskIDs []string) []historySegment {
	if len(taskIDs) == 0 {
		return segs
	}
	skip := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		skip[id] = true
	}
	kept := make([]historySegment, 0, len(segs))
	for _, s := range segs {
		if skip[s.TaskID] {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// alignCurrentSplitPoint 把当前 task 的压缩切点对齐到安全的消息边界。
//
// 为什么必须对齐：一次工具调用在消息序列里是「assistant(带 tool_calls) +
// 随后若干条 tool 结果」，切断点落在中间会留下孤儿 tool 结果，
// 被 dropOrphanToolResults 静默删掉 —— 那不是压缩，是无声丢信息。
//
// 对齐只需一个方向：把切点往后推过紧随其后的 tool 消息。这一个动作同时
// 覆盖了两种越界形态：
//   - 切点后紧跟的是孤儿 tool 结果（它的 assistant 在切点之前）→ 一起纳入压缩；
//   - 切点前是一个带 tool_calls 的 assistant（它的结果还在切点之后）→ 结果一并纳入，
//     保证这次工具调用是完整地被压缩，而不是「提问进了摘要、答案被丢掉」。
func alignCurrentSplitPoint(msgs []llm.Message, n int) int {
	if n < 0 {
		n = 0
	}
	if n > len(msgs) {
		n = len(msgs)
	}
	for n < len(msgs) && msgs[n].Role == llm.RoleTool {
		n++
	}
	return n
}

// ============================================================
// 选取：未压缩详情的前 80%，对齐 task 边界
// ============================================================

// pickSegmentsToCompact 从可压缩段里选出「前 80% token」对应的整数个 task。
//
// 按 token 累计而非按 task 个数取 80%：task 的体量差异极大（一次
// deep_search 能占掉十万 token，一句「谢谢」只占几个），按个数切会
// 出现「压了 8 个小 task 却没动那个撑爆窗口的大 task」。
//
// 对齐 task 边界靠「整段纳入」实现：段本身就是 task 粒度，
// 只要不拆段，原子性天然成立。
//
// 返回选中的段与它们的 token 总量。
func pickSegmentsToCompact(segs []historySegment, detailTokens int) ([]historySegment, int) {
	if len(segs) == 0 {
		return nil, 0
	}

	budget := int(float64(detailTokens) * 0.8)

	picked := make([]historySegment, 0, len(segs))
	acc := 0
	for i := range segs {
		// 纳入本段后仍不超预算 —— 继续吃
		if acc+segs[i].Tokens <= budget {
			picked = append(picked, segs[i])
			acc += segs[i].Tokens
			continue
		}
		break
	}

	// 一个都没选中的两种情形，都必须强制吃掉第一段来保证推进：
	//   - 只有一个 task，且它就是全部详情（80% 预算装不下 100% 的它）
	//   - 第一个 task 单独就超过 80% 预算（典型：一次巨型 deep_search）
	// 不强制推进会让压缩循环空转，最终带着超限输入去调 provider。
	if len(picked) == 0 {
		return segs[:1], segs[0].Tokens
	}

	// 全选中时留一手：至少保留最后一个 task 的原始详情。
	//
	// 最近一轮的交互细节对模型继续作业最关键（用户刚问了什么、
	// 上一个工具返回了什么），全压成摘要会明显降低回答质量。
	// 仅当段数 > 1 时生效；只剩一段时上面的强制推进分支已处理。
	if len(picked) == len(segs) && len(segs) > 1 {
		picked = picked[:len(picked)-1]
		acc -= segs[len(segs)-1].Tokens
	}

	return picked, acc
}

// ============================================================
// 单轮压缩：调 LLM 把「旧摘要 + 选中详情」重写成一份新摘要
// ============================================================

// compactSystemPrompt 压缩用的 system prompt。
//
// 措辞要点（每一条都对应一种实际失效模式）：
//   - 明确「这是给 AI 自己看的工作记录」，否则模型会写成面向用户的
//     总结报告，丢掉文件路径、参数、报错原文这类续作业必需的信息。
//   - 显式要求保留文件路径 / 命令 / 关键参数 / 未完成事项 —— 这些是
//     压缩后最容易丢、丢了最致命的部分。
//   - 禁止调用工具、禁止输出 JSON：压缩是一次纯文本改写，
//     模型若返回 tool_calls，回放时会因「工具未声明」被 provider 判 400。
const compactSystemPrompt = `你是一个对话历史压缩器。你的输出会作为 AI 助手的「历史工作记录」注入后续对话的上下文，供它继续完成任务。

请把给定的历史对话压缩成一份紧凑的工作记录，严格遵守：

【必须保留】
- 用户提出过的需求、目标与明确约束（尤其是后来修正过的口径）
- 已完成的工作及其结论
- 产出与修改过的文件路径（原样保留，不要改写或省略路径）
- 关键的命令、参数、配置值、标识符（如 ID、URL、端口）
- 遇到的错误与最终的解决方式
- 尚未完成、被搁置或用户明确要求后续处理的事项
- 已经做出的技术决策及其理由

【必须丢弃】
- 工具返回的原始大段内容（网页正文、文件全文、长日志）——只留结论
- 重复的试错过程——只留最终有效的做法
- 礼貌寒暄与过程性表述

【格式要求】
- 用简洁的中文分点陈述，按主题而非时间组织
- 不要调用任何工具，只输出纯文本
- 不要输出 JSON、不要加代码块包裹整份记录
- 不要写「以下是摘要」之类的开场白，直接开始正文`

// compactOnce 执行一次压缩，返回新摘要文本。
//
// 素材放得进单次调用时直接一次压完；放不下时自动走「分块 map-reduce」：
//   - map：把素材按 CompactBudget 切成 N 块，每块独立压成一份中间摘要；
//   - reduce：把中间摘要再按预算分批合并，逐层收敛到 1 份。
//
// 这样无论历史多大、模型窗口多小，每一次 LLM 调用都保证不超限。
// 典型触发场景正是「从大窗口模型切到小窗口模型」——历史是在大模型
// 时代攒下的，窗口缩水后它自己就装不进一次压缩调用了。
func compactOnce(
	ctx context.Context, in compactInput,
	prev *model.ContextSummary, picked []historySegment, budget int,
) (string, error) {
	if in.Client == nil {
		return "", fmt.Errorf("compact: llm client is nil")
	}

	counter := contextmgr.GetCounter()

	// 1. 素材切分。旧摘要也参与 —— 它同样可能超限（在大窗口模型下
	//    按 10% 生成的摘要，换到小窗口模型后可能已超过新预算）。
	units := make([]compactUnit, 0, len(picked)+1)
	if prev != nil && strings.TrimSpace(prev.Summary) != "" {
		units = append(units, splitTextUnits(
			"【此前已压缩的历史工作记录】\n"+prev.Summary, in.CompactBudget, counter)...)
	}
	for i := range picked {
		units = append(units, splitSegmentUnits(picked[i], in.CompactBudget, counter)...)
	}
	if len(units) == 0 {
		return "", fmt.Errorf("compact: nothing to compact")
	}

	// 2. 打包成若干「单次调用放得下」的批次
	batches := groupUnits(units, in.CompactBudget)
	if len(batches) > 1 {
		log.Printf("[ctx-compact][%s] payload does not fit one call: %d units -> %d batches (budget=%d)",
			in.Temp.TaskID, len(units), len(batches), in.CompactBudget)
	}

	r := &compactRunner{in: in, budget: budget, counter: counter}

	// 3. map：每批压成一份中间摘要
	level, err := r.summarizeBatches(ctx, batches, "map")
	if err != nil {
		return "", err
	}

	// 4. reduce：逐层合并到 1 份。每层都是「按预算分批 + 每批压一次」，
	//    层数是对数级的（扇出 = CompactBudget / budget ≈ 6）。
	for depth := 1; len(level) > 1; depth++ {
		if r.calls >= maxCompactLLMCalls {
			log.Printf("[ctx-compact][%s] WARNING reached %d llm calls at reduce depth %d "+
				"with %d partials left — keep them concatenated",
				in.Temp.TaskID, r.calls, depth, len(level))
			break
		}
		parts := make([]compactUnit, len(level))
		for i, s := range level {
			parts[i] = compactUnit{text: s, tokens: counter.CountText(s)}
		}
		next, err := r.summarizeBatches(ctx, groupUnits(parts, in.CompactBudget), "merge")
		if err != nil {
			return "", err
		}
		// 无法推进（每批都只剩 1 份，或分批没减少数量）时立即停，
		// 否则会退化成无限循环
		if len(next) >= len(level) {
			break
		}
		level = next
	}

	// 5. 兜底：调用次数用尽时可能残留多份，拼接后硬截断到预算。
	//
	// 截断保留头部（最早的历史）。这是有意的取舍：残留下来的第一份通常
	// 是「旧摘要 + 最早的批次」，信息密度最高；而超预算本身说明素材量已达
	// 上限的十几倍，无论保留哪一段都是严重损失，此时可预测比取巧更重要。
	final := strings.Join(level, "\n\n")
	if n := counter.CountText(final); n > budget {
		log.Printf("[ctx-compact][%s] WARNING residual summary %d tokens exceeds budget %d, "+
			"truncating (compaction quality degraded)", in.Temp.TaskID, n, budget)
	}
	final = counter.TrimToTokens(final, budget)
	if strings.TrimSpace(final) == "" {
		return "", fmt.Errorf("compact: produced empty summary")
	}
	return "【此前对话的历史工作记录（已压缩）】\n" + final, nil
}

// maxCompactLLMCalls 单轮压缩允许的 LLM 调用次数上限。
//
// 分块压缩的调用次数是 O(素材量 / 预算)，极端长历史 + 极小窗口下可能
// 达到几十次。设上限是防「一次用户请求变成上百次压缩调用」—— 超出后
// 不再继续合并，把已有的中间摘要拼接起来交付（质量下降但可控）。
const maxCompactLLMCalls = 24

// minCompactChunk 分块预算的下限（token）。
//
// 上限极小的模型（如被误配成几千）会把块预算压到负数或几十 token，
// 那样的块压不出任何有意义的内容。低于此值时不再分块，改为单次调用 +
// 硬截断素材，宁可丢信息也不做无效切分。
const minCompactChunk = 500

// trimSuffixReserve 给「截断提示」预留的 token。
//
// TrimToTokens 截完后会追加一行「…（摘要超长，已截断）」，这段后缀同样
// 计入请求体。不预留的话每次调用都会略微顶出预算。
const trimSuffixReserve = 24

// compactUnit 喂给压缩 LLM 的最小素材单元，携带预计算的 token 数。
//
// 预计算是必要的：分块要为每段素材反复试算「加进去会不会超预算」，
// 每次都重新编码整段文本会让分块变成 O(n²)。
type compactUnit struct {
	text   string
	tokens int
}

// compactRunner 串起一次压缩里的多次 LLM 调用，负责计数与进度上报。
type compactRunner struct {
	in      compactInput
	budget  int // 每份摘要的目标体量
	counter *contextmgr.Counter
	calls   int
}

// summarizeBatches 把若干批素材各压成一份摘要，返回中间摘要列表。
//
// 任一批失败即整体失败：部分成功会导致「摘要丢了中间一段历史」，
// 那比不压缩更糟 —— 模型会以为某事从未发生过。
func (r *compactRunner) summarizeBatches(
	ctx context.Context, batches [][]compactUnit, stage string,
) ([]string, error) {
	out := make([]string, 0, len(batches))
	for i, batch := range batches {
		parts := make([]string, len(batch))
		for j, u := range batch {
			parts[j] = u.text
		}
		if len(batches) > 1 {
			publishCompacting(r.in.Temp.TaskID, "progress", 0, 0,
				fmt.Sprintf("第 %d/%d 段", i+1, len(batches)))
		}
		s, err := r.summarize(ctx, parts)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// summarize 执行一次压缩 LLM 调用。
func (r *compactRunner) summarize(ctx context.Context, parts []string) (string, error) {
	if r.calls >= maxCompactLLMCalls {
		// 次数用尽：不再调 LLM，直接把素材硬截断当作结果。
		// 丢了细节但保住了「不超限」这条硬约束。
		return r.counter.TrimToTokens(strings.Join(parts, "\n\n"), r.budget), nil
	}
	r.calls++

	instruction := fmt.Sprintf(
		"\n\n请把以上全部内容合并压缩成一份新的工作记录，控制在 %d 个 token 以内（约 %d 个汉字）。",
		r.budget, r.budget*3/2)

	// 素材侧的硬约束：扣掉 system prompt 与指令后的可用额度。
	// 分块已经保证不超，这里是双保险 —— 万一某单元自身就超过预算
	// （一条巨型 tool 结果），不截断就会把调用打爆。
	//
	// 额外扣掉 trimSuffixReserve：TrimToTokens 截断后会追加「已截断」提示，
	// 不预留的话总输入会略微顶出预算（实测超出约 10 token）。
	avail := r.in.CompactBudget - r.counter.CountText(compactSystemPrompt) -
		r.counter.CountText(instruction) - trimSuffixReserve
	if avail < minCompactChunk {
		avail = minCompactChunk
	}
	body := strings.Join(parts, "\n\n")
	if r.counter.CountText(body) > avail {
		body = r.counter.TrimToTokens(body, avail)
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: compactSystemPrompt},
		{Role: llm.RoleUser, Content: body + instruction},
	}

	opts := llm.DefaultOptions()
	// 压缩要的是忠实转述而非创造性改写，温度压到接近确定性
	opts.Temperature = 0.2
	// 关掉深度思考（混元）：压缩不需要推理，思考链只会把调用拖到超时
	opts.ExtraBody = r.in.CompactExtraBody

	// 留痕：压缩也是一次真实 LLM 调用（烧 token + 引入延迟），
	// 落一条 turn 才能在调试页看见它。纯旁路 —— 落库失败绝不阻断压缩。
	turn := beginCompactTurn(ctx, r.in, r.calls, messages, opts)

	// 不走 chatWithToolsStreaming：压缩是内部动作，不该产生
	// delta 事件污染前端的回答流（前端会把 delta 当成助手正文渲染）。
	// 也刻意不传任何工具，从协议层杜绝模型返回 tool_calls。
	result, err := r.in.Client.ChatWithTools(ctx, messages, nil, llm.ToolChoiceNone, opts)
	if err != nil {
		failCompactTurn(ctx, turn, err)
		return "", fmt.Errorf("compact: llm call failed: %w", err)
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		failCompactTurn(ctx, turn, errors.New("llm returned empty summary"))
		return "", fmt.Errorf("compact: llm returned empty summary")
	}

	// 硬性兜底：模型常不遵守 token 预算。超了就截断，
	// 否则摘要自身会成为下一轮的膨胀源。
	text = r.counter.TrimToTokens(text, r.budget)
	finishCompactTurn(ctx, turn, text, result.Usage)
	return text, nil
}

// ============================================================
// 压缩调用的调试留痕
// ============================================================
//
// 压缩此前是完全隐形的：它只在日志里出现，调试页看不到「这一轮为什么
// 多花了 3 秒、多烧了几千 token」。它又是唯一可能把任务判死的环节
// （压缩失败即任务失败），看不见就等于没法排查。
//
// 因此每次压缩 LLM 调用都落一条 turn_type=context_compact 的 turn，
// 与 functional_call 并列出现在调试页的时间线里。

// compactTurnSnapshot 压缩调用的输入快照。
//
// 结构对齐 model.TurnLLMInput：system 单列、素材作为一条 user 消息，
// 这样调试页的 INPUT 区块能直接展示「模型到底被喂了什么」。
// seq 是本次压缩流程内的第几次 LLM 调用（1 起），与轮次无关，
// 记进 Options 便于区分同一次压缩里的多次调用。
func compactTurnSnapshot(in compactInput, seq int, messages []llm.Message, opts llm.Options) *model.TurnLLMInput {
	snap := &model.TurnLLMInput{
		Model:        in.Client.Name(),
		Options:      map[string]interface{}{"temperature": opts.Temperature},
		SystemPrompt: "",
	}
	snap.Options["compact_call_seq"] = seq
	if len(in.CompactExtraBody) > 0 {
		snap.Options["extra_body"] = in.CompactExtraBody
	}
	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			if snap.SystemPrompt == "" {
				snap.SystemPrompt = m.Content
				continue
			}
		}
		snap.Messages = append(snap.Messages, model.TurnMessage{
			Role:    string(m.Role),
			Content: m.Content,
		})
	}
	return snap
}

// compactLoopRound 取压缩发生时所处的 agent 循环轮次。
//
// 与同阶段的 functional_call / tool_call turn 的 Round 同源取值：
// 优先 hint 的 ToolCallRound（执行期 = 即将进行的循环轮次，规划期 =
// 研究轮次），无 hint 时按 phase 数已完成的 tool_call 轮数兜底 ——
// 与 currentToolCallRound 语义一致，但用独立 DAO 取数，不依赖
// funcCallRegistry 的装配（单测里是 nil）。
func compactLoopRound(ctx context.Context, taskID string, phase model.TaskPhase) int32 {
	if hint := container.GetTransitionStore().GetHint(taskID); hint != nil {
		return int32(hint.ToolCallRound)
	}
	round, err := (&dao.TaskTurnDAO{}).CountToolCallRoundsByPhase(ctx, taskID, phase)
	if err != nil {
		log.Printf("[ctx-compact][%s] count tool_call rounds (phase=%s) failed: %v",
			taskID, phase, err)
		return 0
	}
	return int32(round)
}

// beginCompactTurn 开启一条压缩 turn。任何失败都只记日志并返回 nil ——
// 留痕失败不该让压缩本身失败。
func beginCompactTurn(
	ctx context.Context, in compactInput, seq int,
	messages []llm.Message, opts llm.Options,
) *model.TaskTurn {
	if !dao.Ready() || in.Temp == nil {
		return nil
	}
	phase := model.TaskPhase(in.Temp.Phase)
	if phase == 0 {
		phase = model.PhaseExecuting
	}
	// 用独立 DAO 而非 funcCallRegistry.turnDAO：后者只在生产装配时注入，
	// 单测里是 nil。旁路留痕不该依赖那条装配链是否就绪。
	turnDAO := &dao.TaskTurnDAO{}
	turn, err := turnDAO.BeginTurn(ctx, dao.BeginTurnParams{
		TaskID: in.Temp.TaskID,
		ChatID: in.Temp.ChatID,
		// 父任务必须带上：子任务（工作流节点 / deep_search）压缩时同样留痕，
		// 缺了它这条 turn 会被 buildSegmentedHistory 当成主对话自己的 turn
		// 回放进历史 —— 压缩动作本身不该出现在对话里。带上之后它与子任务的
		// 其他 turn 走同一条「跳过原始回放」分支。
		ParentTaskID: in.Temp.ParentTaskID,
		TurnType:     model.TurnTypeContextCompact,
		Phase:        phase,
		Stage:        model.TaskStage(in.Temp.Stage),
		// Round 记「压缩发生在 agent 循环的第几轮」，与同阶段的
		// functional_call / tool_call turn 同源取值（hint 的 ToolCallRound，
		// 无 hint 时按 phase 数已完成的轮数）。此前这里记的是压缩流程
		// 内部的调用序号（1/2/3…），调试页时间线上轮次会从 6 跳回 1
		// 再跳回 7，看起来像断链。
		Round: compactLoopRound(ctx, in.Temp.TaskID, phase),
		Model: in.Client.Name(),
		Input: compactTurnSnapshot(in, seq, messages, opts),
	})
	if err != nil {
		log.Printf("[ctx-compact][%s] begin compact turn failed (ignore): %v",
			in.Temp.TaskID, err)
		return nil
	}
	return turn
}

// finishCompactTurn 回填压缩 turn 的输出与 token。
func finishCompactTurn(ctx context.Context, turn *model.TaskTurn, text string, usage llm.Usage) {
	if turn == nil {
		return
	}
	if err := (&dao.TaskTurnDAO{}).FinishTurn(ctx, turn.ID, dao.FinishTurnParams{
		Output:           &model.TurnLLMOutput{Text: text},
		TotalTokens:      usage.TotalTokens,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
	}); err != nil {
		log.Printf("[ctx-compact][%s] finish compact turn %d failed (ignore): %v",
			turn.TaskID, turn.ID, err)
		return
	}
	publishTurnDone(turn.TaskID, turn.ID, turn.TurnSeq)
}

// failCompactTurn 压缩调用失败时把 turn 标成 failed。
func failCompactTurn(ctx context.Context, turn *model.TaskTurn, reason error) {
	if turn == nil {
		return
	}
	if err := (&dao.TaskTurnDAO{}).FailTurn(ctx, turn.ID,
		fmt.Errorf("context compact failed: %w", reason), nil); err != nil {
		log.Printf("[ctx-compact][%s] fail compact turn %d failed (ignore): %v",
			turn.TaskID, turn.ID, err)
		return
	}
	publishTurnDone(turn.TaskID, turn.ID, turn.TurnSeq)
}

// splitSegmentUnits 把一个 task 段切成若干素材单元。
//
// 放得下就整段一个单元；放不下则退到消息粒度切分。
// 这里切段不违反「task 原子性」—— 那条约束针对的是主对话历史里
// assistant tool_calls 与 tool 结果的配对（断配会被 provider 判 400），
// 而本函数的产物是喂给压缩 LLM 的纯文本素材，不参与消息配对校验。
func splitSegmentUnits(seg historySegment, budget int, counter *contextmgr.Counter) []compactUnit {
	whole := renderSegmentsForCompaction([]historySegment{seg})
	tokens := counter.CountText(whole)
	if tokens <= budget || len(seg.Messages) <= 1 {
		return []compactUnit{{text: whole, tokens: tokens}}
	}

	units := make([]compactUnit, 0, 4)
	var b strings.Builder
	acc := 0
	flush := func() {
		if b.Len() == 0 {
			return
		}
		units = append(units, compactUnit{text: b.String(), tokens: acc})
		b.Reset()
		acc = 0
	}
	for i := range seg.Messages {
		piece := renderMessageForCompaction(seg.Messages[i])
		n := counter.CountText(piece)
		if n > budget {
			// 单条消息就超过整块预算：先落盘已有内容，再把它硬截断成
			// 独立单元（截断优于丢弃开头或结尾）
			flush()
			units = append(units, compactUnit{
				text:   counter.TrimToTokens(piece, budget),
				tokens: budget,
			})
			continue
		}
		if acc+n > budget {
			flush()
		}
		b.WriteString(piece)
		acc += n
	}
	flush()
	return units
}

// splitTextUnits 把一段纯文本按行切成素材单元。
//
// 用于旧摘要的超限处理：它是无结构的摘要文本，只能按行边界切。
func splitTextUnits(text string, budget int, counter *contextmgr.Counter) []compactUnit {
	if counter.CountText(text) <= budget {
		return []compactUnit{{text: text, tokens: counter.CountText(text)}}
	}

	units := make([]compactUnit, 0, 4)
	var b strings.Builder
	acc := 0
	flush := func() {
		if b.Len() == 0 {
			return
		}
		units = append(units, compactUnit{text: b.String(), tokens: acc})
		b.Reset()
		acc = 0
	}
	// 只在非末段补回换行：strings.Split 会在末尾留一个空串，
	// 无条件补 "\n" 会给原文多加一个字节，破坏「切分不丢内容」的可校验性。
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i < len(lines)-1 {
			line += "\n"
		}
		n := counter.CountText(line)
		if n > budget {
			flush()
			units = append(units, compactUnit{
				text:   counter.TrimToTokens(line, budget),
				tokens: budget,
			})
			continue
		}
		if acc+n > budget {
			flush()
		}
		b.WriteString(line)
		acc += n
	}
	flush()
	return units
}

// groupUnits 把素材单元贪心打包成若干批次，保证每批 token 不超预算。
//
// 强制每个批次至少含一个单元：即使某单元自身就超预算（已在上游截断过，
// 理论上不会），也不允许产出空批 —— 空批会让外层把它当成「压完了」。
func groupUnits(units []compactUnit, budget int) [][]compactUnit {
	if len(units) == 0 {
		return nil
	}

	batches := make([][]compactUnit, 0, 4)
	cur := make([]compactUnit, 0, 4)
	acc := 0
	for i := range units {
		if len(cur) > 0 && acc+units[i].tokens > budget {
			batches = append(batches, cur)
			cur = make([]compactUnit, 0, 4)
			acc = 0
		}
		cur = append(cur, units[i])
		acc += units[i].tokens
		if acc >= budget {
			batches = append(batches, cur)
			cur = make([]compactUnit, 0, 4)
			acc = 0
		}
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

// renderSegmentsForCompaction 把待压缩的段渲染成可读文本喂给 LLM。
//
// 不直接把 []llm.Message 原样传过去，原因有两个：
//   - 历史里的 assistant 消息带 tool_calls 与 Signature，作为「待压缩
//     素材」传入会让模型误以为那是它自己的上一轮输出，从而续写而非压缩。
//   - 工具结果需要明确标注「这是工具返回」，模型才知道可以只留结论。
func renderSegmentsForCompaction(segs []historySegment) string {
	var b strings.Builder
	for i := range segs {
		for _, m := range segs[i].Messages {
			b.WriteString(renderMessageForCompaction(m))
		}
	}
	return b.String()
}

// renderMessageForCompaction 把单条消息渲染成一行压缩素材。
//
// 单独拆出来是为了让 splitSegmentUnits 能做到消息粒度的切分 ——
// 段放不下时需要逐条累加并计量，不能只在整段层面渲染。
func renderMessageForCompaction(m llm.Message) string {
	var b strings.Builder
	switch m.Role {
	case llm.RoleUser:
		b.WriteString("用户：")
		b.WriteString(m.Content)
		b.WriteString("\n")
	case llm.RoleAssistant:
		if s := strings.TrimSpace(m.Content); s != "" {
			b.WriteString("助手：")
			b.WriteString(s)
			b.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "助手调用工具：%s\n", tc.Name)
		}
	case llm.RoleTool:
		fmt.Fprintf(&b, "工具 %s 返回：%s\n", m.ToolName, m.Content)
	}
	return b.String()
}

// ============================================================
// 依赖访问的小封装
// ============================================================

// resolveChatContextLimit 解析该对话所用模型的上下文上限。
//
// 任何一步失败都回落到保守默认值而非报错 —— 拿不到上限时宁可按
// 128K 早压，也不要因为「查不到配置」就让整轮请求失败。
func resolveChatContextLimit(ctx context.Context, chatID string) int {
	resolver := funcCallRegistry.llmResolver
	convDAO := funcCallRegistry.conversationDAO
	if resolver == nil {
		return contextmgr.DefaultContextLimit
	}

	modelID := ""
	if convDAO != nil {
		if conv, err := convDAO.GetConversationByID(ctx, chatID); err == nil && conv != nil {
			modelID = conv.ModelID
		}
	}

	var spec *llmreg.ModelSpec
	if s, err := resolver.ResolveSpec(ctx, modelID); err == nil {
		spec = s
	}
	if spec == nil {
		return contextmgr.DefaultContextLimit
	}
	return contextmgr.ResolveContextLimit(spec.ContextLimit, spec.Model)
}

// compactTimeout 压缩调用的单次超时。
//
// 主对话客户端的超时（混元 60s）是为「带深度思考的对话生成」设计的，
// 压缩走的是另一条路径：思考已关闭，输出又短（≤ 上限 10%），60s 本该
// 绰绰有余。留到 180s 是为了吸收分块压缩时偶发的网关排队 —— 但也仅此
// 而已，超时必须失败（见 assembleWithCompaction），不能静默放行。
const compactTimeout = 180 * time.Second

// resolveCompactClient 为压缩单独构建 LLM 客户端。
//
// 为什么不复用主对话客户端：
//   - 超时太短：混元网关的 60s 会被深度思考链吃满，压缩从未成功过
//     （context_summary 表长期 0 行就是证据）；
//   - 思考链无意义：压缩是忠实转述，不开思考质量不降反稳，
//     实测同一批任务 439s → 59s。
//
// 返回的 extraBody 是配套的请求体补丁（关闭深度思考），由调用方塞进
// compactInput.CompactExtraBody，最终由 provider 层合并进请求。
func resolveCompactClient(
	ctx context.Context, chatID string,
) (llm.Client, map[string]interface{}, error) {
	resolver := funcCallRegistry.llmResolver
	if resolver == nil {
		return nil, nil, fmt.Errorf("ctx-compact: llm resolver not initialized")
	}

	modelID := ""
	if convDAO := funcCallRegistry.conversationDAO; convDAO != nil {
		if conv, err := convDAO.GetConversationByID(ctx, chatID); err == nil && conv != nil {
			modelID = conv.ModelID
		}
	}
	spec, err := resolver.ResolveSpec(ctx, modelID)
	if err != nil {
		return nil, nil, fmt.Errorf("ctx-compact: resolve model spec: %w", err)
	}
	if spec == nil {
		return nil, nil, fmt.Errorf("ctx-compact: model spec not found (model=%q)", modelID)
	}

	// 只改超时，端点 / key / 模型全部沿用对话所用配置
	c := *spec
	c.Timeout = compactTimeout
	return llmreg.BuildClient(c), compactNoThinkBody(spec), nil
}

// compactNoThinkBody 生成「关闭深度思考」的请求体补丁。
//
// 为什么是顶层 reasoning_effort=none —— 这是实测筛出来的唯一有效写法
// （hy4-preview @ api.lkeap.cloud.tencent.com，同一份压缩素材）：
//
//	写法                                       耗时      思考字符
//	（不传）                                    48s       16648
//	enable_thinking=false                       2m10s     52586
//	chat_template_kwargs.enable_thinking=false  1m15s     26775
//	chat_template_kwargs.reasoning_effort=...   1m18s     30571
//	reasoning_effort=none            ✅          1.7s         0
//
// 也就是说：混元在这条网关上只认 OpenAI 口径的顶层 reasoning_effort，
// 而 chat_template_kwargs 那套（另一条 tokenhub 网关上有效）在这里**会让
// 思考更长**。写错写法比不写更糟。
//
// 只在混元（openai 协议 + 模型名以 hy 开头）下发：reasoning_effort 对
// 非推理模型是未知字段，严格校验的网关可能 400。宁可让压缩慢一点，
// 也不能让压缩请求被拒 —— 那会直接把任务判死。
func compactNoThinkBody(spec *llmreg.ModelSpec) map[string]interface{} {
	if spec == nil || spec.Protocol != llmreg.ProtocolOpenAI {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(spec.Model), "hy") {
		return nil
	}
	return map[string]interface{}{"reasoning_effort": "none"}
}

func loadContextSummary(ctx context.Context, chatID string) (*model.ContextSummary, error) {
	d := &dao.ContextSummaryDAO{}
	return d.GetByChatID(ctx, chatID)
}

func saveContextSummary(ctx context.Context, s *model.ContextSummary) error {
	d := &dao.ContextSummaryDAO{}
	return d.Upsert(ctx, s)
}

// loadTaskContextSummary 读取单任务的压缩水位（工作流节点子任务专用）。
func loadTaskContextSummary(ctx context.Context, taskID string) (*model.TaskContextSummary, error) {
	d := &dao.TaskContextSummaryDAO{}
	return d.GetByTaskID(ctx, taskID)
}

// saveTaskContextSummary 写入单任务的压缩水位。
func saveTaskContextSummary(ctx context.Context, s *model.TaskContextSummary) error {
	d := &dao.TaskContextSummaryDAO{}
	return d.Upsert(ctx, s)
}

// publishCompacting 广播「正在压缩上下文」事件。
//
// phase 取 start / done / progress。start 与 done 携带水位，用于显示
// 「正在压缩上下文… (12.4k / 1.0M)」；progress 用于分块压缩时报进度
// 「第 2/7 段」—— 分块后耗时成倍增加，没有进度提示用户会以为卡死。
//
// 事件是尽力而为的（见 pkg/stream 包注释），丢了也只是前端少一个提示，
// 不影响压缩本身，因此不返回错误。
func publishCompacting(taskID, phase string, tokens, limit int, stage string) {
	data := map[string]interface{}{
		"phase":  phase,
		"tokens": tokens,
		"limit":  limit,
	}
	if stage != "" {
		data["stage"] = stage
	}
	stream.Publish(stream.Event{
		Type:   stream.EventTypeCompacting,
		TaskID: taskID,
		Data:   data,
	})
}
