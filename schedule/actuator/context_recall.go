// Package actuator — recall_context 内置工具的取回执行器
//
// # 为什么需要它
//
// 上下文压缩是**有损且不可逆**的：一段历史被摘要化之后，原始的工具输出、
// 报错栈、代码片段就永久离开了上下文。模型后续若发现「摘要里那句结论不够，
// 我需要看当时的原始响应体」，没有任何办法拿回来 —— 只能凭摘要继续推理，
// 或者重新跑一遍同样的取证动作（浪费且结果可能不同）。
//
// P0 已经在压缩产物旁挂了句柄（工作流摘要带 task_id、历史摘要带
// covered_task_index、当前 task 摘要带被压掉的 turn_id）。本文件提供
// 「拿句柄换原文」的通道，两者必须成对存在 —— 只有句柄没有通道，
// 那些 ID 对模型就是纯装饰，甚至会诱导它幻觉一个不存在的工具。
//
// # 为什么不做成独立子任务（与 deep_search 的区别）
//
//	deep_search：检索目标未知 → 要多轮「搜索 → 抓取 → 再搜」→ 必须 agent
//	             循环 → 必须独立子任务承载
//	recall     ：取回目标已确定（就是该 task 的全部 turn）→ 只需「读取 +
//	             按 query 提取」→ 一次或 map-reduce 若干次 LLM → 不需要
//	             任何工具循环
//
// 因此本实现直接复用压缩链路的分块与 map-reduce（splitTextUnits /
// groupUnits），省掉子任务创建、轮询等待、query 幂等复用四块逻辑，并从
// 根上消除两类风险：没有 agent 循环 → 不可能递归取回；不是独立任务 →
// 没有自己的上下文 → 无需「禁止取回时触发压缩」的特例。
//
// 可观测性不丢：每次取回的 LLM 调用落一条 turn_type=context_recall，
// 与压缩的 turn_type=context_compact 并列出现在调试页时间线里。
//
// # 两种模式
//
//	turn_id 模式：直接查 task_turn 返回该轮原文，零 LLM 调用、当轮返回。
//	              适用于「摘要说第 265 轮命令失败了，我要看原始报错」。
//	task_id 模式：加载该 task 全量交互。放得下就原样返回（仍然零 LLM ——
//	              原文比 LLM 转述更忠实）；放不下才按 query 做 map-reduce
//	              提取。适用于「上一轮对话到底查出了什么」。
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// ============================================================
// 配额与预算
// ============================================================

// recallQuotaPerTask 单个任务允许的取回次数。
//
// 取回的目的是「补齐摘要里缺的那一块」，正常一轮推理里 1~3 次足够。
// 给到 6 是留出「取回后发现还要看上游那一段」的余量。设上限的理由与
// fetch 配额一致：取回的产物会进入上下文，无限取回等于把压缩掉的历史
// 又原样搬回来，压缩就白做了。
const recallQuotaPerTask = 6

// recallMaxOutputTokens 单次取回返回给模型的 token 上限（固定回退值）。
//
// 这是本工具最关键的一个数：取回结果要进主对话上下文，而主对话之所以
// 触发压缩就是因为空间不够。给太大会让「取回一次 → 立刻又超限 → 触发
// 压缩 → 把刚取回的内容又压掉」形成反复空转。
//
// 但它不该是固定值 —— 固定 8000 对 128K 窗口模型太小（白白浪费可用空间），
// 对小窗口模型又可能太大（取回一次就顶到压缩线）。真正的上限由
// resolveRecallOutputBudget 按「主对话剩余空间」动态计算；本常量只是
// 拿不到占用水位时的回退值。
const recallMaxOutputTokens = 8000

// recallMinOutputTokens 动态预算的保底下限。
//
// 小窗口模型（如 8K）下剩余空间算出来可能很小，但取回至少要能装下
// 一条有意义的历史（几 KB 的工具输出），低于这个值取回就失去意义。
const recallMinOutputTokens = 4000

// recallMaxOutputCeiling 动态预算的绝对上限。
//
// 大窗口模型（128K/1M）下剩余空间很大，但一次取回塞几十万 token 既拖慢
// 下一轮又逼近压缩线。64K 已足够装下任意单条历史记录，再大没有意义。
const recallMaxOutputCeiling = 64 * 1024

// recallVerbatimFloor 「小到不值得提取」的门槛（token）。
//
// 低于它一律给全文，即使模型给了 query：提取要多一次 LLM 往返（延迟 +
// token），而一两千 token 的记录模型自己读毫无压力，让 LLM 转述反而多一道
// 丢细节的风险 —— 而模型来取回，图的就是细节。
//
// 1500 ≈ 2~3k 字符，是「一眼能读完」的量级。
const recallVerbatimFloor = 1500

// recallExtractBudget map-reduce 每层中间产物的 token 预算。
//
// 比 recallMaxOutputTokens 小：中间产物还要再合并一层，留出合并余量。
const recallExtractBudget = 3000

// maxRecallLLMCalls 单次取回允许的 LLM 调用次数上限。
//
// 与压缩的 maxCompactLLMCalls 同理：超长 task（几十轮工具循环，素材
// 十几万 token）在小窗口下分块数会很大。超出后不再合并，把已有的中间
// 产物拼接交付（质量下降但可控），绝不无限调用。
const maxRecallLLMCalls = 12

// resolveRecallOutputBudget 动态计算单次取回能返回的 token 上限。
//
// 为什么不能固定：取回结果要进下一轮主对话上下文，而主对话在
// Trigger(0.8×limit) 以上就会触发压缩。固定 8000 对 128K 窗口模型太小
// （白白浪费空间），对小窗口模型又可能太大（取回一次就顶到压缩线）。
//
// 算法：headroom = Trigger - 当前已用（最近一次 functional_call 的
// context_blocks.Total），budget = headroom × recallOutputHeadroomRatio，
// 再夹到 [recallMinOutputTokens, recallMaxOutputCeiling]。
//
// 拿不到占用水位（首次调用 / 快照缺失）时回退到固定默认值 recallMaxOutputTokens。
func resolveRecallOutputBudget(
	ctx context.Context, deps recallDeps, temp *model.TaskTemp,
) int {
	limit := resolveChatContextLimit(ctx, temp.ChatID)
	trigger := contextmgr.NewThresholds(limit).Trigger

	used := 0
	if deps.turnDAO != nil {
		// 用 ListTurnsByTaskID（走 getDB()，测试可注入 Tx）而非 GetLatestLLMTurn
		// （内部 turnTypeCond 用全局 DB()，测试环境会 nil panic）。
		if turns, err := deps.turnDAO.ListTurnsByTaskID(ctx, temp.TaskID); err == nil {
			for i := len(turns) - 1; i >= 0; i-- {
				if !turns[i].TurnType.IsLLMCall() {
					continue
				}
				var in model.TurnLLMInput
				if json.Unmarshal([]byte(turns[i].Input), &in) == nil && in.ContextBlocks != nil {
					used = in.ContextBlocks.Total
				}
				break
			}
		}
	}

	budget := recallMaxOutputTokens
	if used > 0 && trigger > used {
		// 取剩余空间的 7/10（留 30% 缓冲：计量有 ±20% 误差，且本轮 tool_call
		// 交互记录下一轮组装时也要进上下文）。整数运算避免浮点截断误差。
		budget = (trigger - used) * 7 / 10
	}
	if budget < recallMinOutputTokens {
		budget = recallMinOutputTokens
	}
	if budget > recallMaxOutputCeiling {
		budget = recallMaxOutputCeiling
	}
	return budget
}

// ============================================================
// 配额记账
// ============================================================

// recallBudget 一个任务的取回用量。
//
// 必须加锁：一次 LLM 响应里的多个 tool_call 是并行执行的，
// 并发拿到同一个 budget 后 used++ 会 data race。
type recallBudget struct {
	mu       sync.Mutex
	used     int
	lastSeen time.Time
}

// recallBudgets 任务维度的取回用量表（惰性回收，见 maybeSweepRecallBudgets）。
var recallBudgets sync.Map // taskID -> *recallBudget

// recallBudgetTTL 用量条目的过期时间：超过它没被访问即视为任务已结束。
const recallBudgetTTL = 6 * time.Hour

// recallBudgetSweepEvery 每累计这么多次配额申请做一次惰性回收。
const recallBudgetSweepEvery = 128

// recallSweepCounter 配额申请计数器（原子自增，只用于触发回收）。
var recallSweepCounter atomic.Int64

// takeRecallQuota 申请一次取回配额，返回 (本次是第几次, 是否放行)。
//
// 「先扣后取」：拿到 true 就一定会发起取回，否则并发的批量 tool_call
// 能钻空子突破配额。
func takeRecallQuota(taskID string) (int, bool) {
	if taskID == "" {
		// 拿不到任务维度就退化为「不计量但放行」：配额是防滥用的护栏，
		// 不是功能开关，不该因为缺个 ID 就废掉取回能力。
		return 0, true
	}
	now := time.Now()
	v, _ := recallBudgets.LoadOrStore(taskID, &recallBudget{lastSeen: now})
	b := v.(*recallBudget)

	b.mu.Lock()
	b.used++
	b.lastSeen = now
	used := b.used
	b.mu.Unlock()

	maybeSweepRecallBudgets(now)
	return used, used <= recallQuotaPerTask
}

// maybeSweepRecallBudgets 惰性回收过期条目。
func maybeSweepRecallBudgets(now time.Time) {
	if recallSweepCounter.Add(1)%recallBudgetSweepEvery != 0 {
		return
	}
	recallBudgets.Range(func(k, v interface{}) bool {
		b, ok := v.(*recallBudget)
		if !ok {
			recallBudgets.Delete(k)
			return true
		}
		b.mu.Lock()
		stale := now.Sub(b.lastSeen) > recallBudgetTTL
		b.mu.Unlock()
		if stale {
			recallBudgets.Delete(k)
		}
		return true
	})
}

// resetRecallBudgets 清空全部用量（仅测试使用）。
func resetRecallBudgets() {
	recallBudgets.Range(func(k, _ interface{}) bool {
		recallBudgets.Delete(k)
		return true
	})
}

// recallQuotaExceededMessage 配额用尽时回喂给模型的话术。
//
// 必须给出明确的下一步，否则模型只会原地重试同一个工具。
func recallQuotaExceededMessage() string {
	return fmt.Sprintf(
		"本任务的历史取回配额已用尽（每任务最多 %d 次）。请不要再调用 recall_context，"+
			"改为基于已取回的内容与现有摘要继续推进；确实缺少关键信息时，"+
			"直接重新执行对应的取证动作（如再读一次文件、再跑一次命令）。",
		recallQuotaPerTask)
}

// ============================================================
// 取回执行器
// ============================================================

// recallDeps 取回要用到的数据访问依赖。
//
// 显式传入而非在函数里 new：取回要读「目标 task / 目标 turn」，这是本功能的
// 核心输入，必须能在单测里替换成测试库。留痕用的 DAO 则仍在内部 new
// （与压缩一致），因为那是旁路动作、失败即忽略。
type recallDeps struct {
	taskDAO *dao.TaskDAO
	turnDAO *dao.TaskTurnDAO
}

// resolve 补齐缺失的 DAO。
//
// 生产路径由 tool_call actuator 注入；万一装配遗漏（或从别处直接调用），
// 回落到独立 DAO 走全局连接，不因为少传一个依赖就废掉取回能力。
func (d recallDeps) resolve() recallDeps {
	if d.taskDAO == nil {
		d.taskDAO = &dao.TaskDAO{}
	}
	if d.turnDAO == nil {
		d.turnDAO = &dao.TaskTurnDAO{}
	}
	return d
}

// buildContextRecaller 构造 recall_context 工具的取回执行器闭包。
//
// 闭包捕获父任务 temp（当前正处于 tool_call 阶段的任务）与 parentTurnID
// （本次 tool_call 的 turn，用于把取回的 LLM 调用挂在同一轮上）。
func buildContextRecaller(
	deps recallDeps, temp *model.TaskTemp, parentTurnID int64,
) workspacepkg.ContextRecallerFunc {
	return func(ctx context.Context, req workspacepkg.ContextRecallRequest) (string, error) {
		if temp == nil {
			return "", fmt.Errorf("recall_context: 当前任务上下文缺失")
		}
		return runContextRecall(ctx, deps.resolve(), temp, parentTurnID, req)
	}
}

// runContextRecall 执行一次取回。
//
// 配额检查放在最前：两种模式都要受限（turn_id 模式虽不调 LLM，但返回的
// 原文同样占上下文）。
func runContextRecall(
	ctx context.Context, deps recallDeps, temp *model.TaskTemp, parentTurnID int64,
	req workspacepkg.ContextRecallRequest,
) (string, error) {
	if used, allowed := takeRecallQuota(temp.TaskID); !allowed {
		log.Printf("[ctx-recall][%s] quota exceeded (used=%d), reject", temp.TaskID, used)
		// 返回文本而非 error：配额用尽是预期内的约束，不是调用失败。
		// 回喂引导话术，让模型换路径而不是把它当工具故障重试。
		return recallQuotaExceededMessage(), nil
	}

	if req.TurnID > 0 {
		return recallByTurnID(ctx, deps, temp, req.TurnID)
	}
	return recallByTaskID(ctx, deps, temp, parentTurnID, req.TaskID, req.Query)
}

// ============================================================
// 模式一：按 turn_id 直取原文（零 LLM）
// ============================================================

// recallByTurnID 按 turn_id 取回该轮的原始记录。
//
// 不调 LLM：目标就是一条确定的记录，原文比任何转述都忠实。
func recallByTurnID(
	ctx context.Context, deps recallDeps, temp *model.TaskTemp, turnID int64,
) (string, error) {
	turn, err := deps.turnDAO.GetTurn(ctx, turnID)
	if err != nil {
		return "", fmt.Errorf("recall_context: 读取 turn %d 失败: %w", turnID, err)
	}
	if turn == nil {
		// 明确告知而不是返回空：turn 可能因任务重试被清理，
		// 模型需要知道「这条确实没了」才好换路径，而不是以为自己参数写错了。
		return fmt.Sprintf(
			"未找到 turn_id=%d 的记录（可能已因任务重试被清理）。"+
				"请改用 task_id 取回整段任务记录，或直接重新执行对应的取证动作。",
			turnID), nil
	}
	// 越权防护：只允许取回同一对话内的记录。
	// 缺了这条，模型拿一个猜的 turn_id 就能读到别人对话的内容。
	if turn.ChatID != temp.ChatID {
		log.Printf("[ctx-recall][%s] reject cross-chat turn %d (chat %s != %s)",
			temp.TaskID, turnID, turn.ChatID, temp.ChatID)
		return "", fmt.Errorf(
			"recall_context: turn_id=%d 不属于当前对话，拒绝取回", turnID)
	}

	counter := contextmgr.GetCounter()
	body := renderTurnForRecall(turn)
	if strings.TrimSpace(body) == "" {
		return fmt.Sprintf("turn_id=%d 存在但没有可回看的正文（type=%s，status=%d）。",
			turnID, turn.TurnType.String(), turn.Status), nil
	}
	body = counter.TrimToTokens(body, resolveRecallOutputBudget(ctx, deps, temp))

	return fmt.Sprintf("【取回的历史记录原文】turn_id=%d（task_id=%s，type=%s）\n\n%s",
		turnID, turn.TaskID, turn.TurnType.String(), body), nil
}

// renderTurnForRecall 把一条 turn 渲染成可回看的原文。
//
// 优先走 turnToMessage —— 它产出的正是「这条 turn 当初回放进历史时的样子」，
// 与被压掉的那条消息一一对应，回看语义最准确。
//
// turnToMessage 返回 false 的情况（workflow turn、失败的 LLM 调用、仍在
// 执行中的 turn）回落到直接展示 output：这些 turn 本来不进历史，但模型
// 既然点名要看，给它原始输出总比回一句「没有内容」有用。
func renderTurnForRecall(turn *model.TaskTurn) string {
	if msg, ok := turnToMessage(turn); ok {
		if s := strings.TrimSpace(renderMessageForCompaction(msg)); s != "" {
			return s
		}
	}
	if s := strings.TrimSpace(turn.Output); s != "" {
		return "原始输出：\n" + s
	}
	return ""
}

// ============================================================
// 模式二：按 task_id 取回整段任务记录
// ============================================================

// recallPlan 按 task_id 取回时选定的处理路径。
type recallPlan int

const (
	// recallPlanVerbatim 原样返回全文，不调 LLM。
	recallPlanVerbatim recallPlan = iota
	// recallPlanExtract 按 query 做 map-reduce 提取。
	recallPlanExtract
	// recallPlanNeedQuery 记录过长且没给 query，无法处理，要求补 query。
	recallPlanNeedQuery
)

// planRecall 决定按 task_id 取回时走哪条路径。
//
// 抽成纯函数是为了可测：提取路径要真调 LLM，单测里跑不了，但「什么情况下
// 该走哪条路」恰恰是最容易出错、也最该被钉死的部分。
//
// 决策依据：素材量、有没有 query、以及本次取回的动态输出预算 budget。
//
//	                        无 query                    有 query
//	total ≤ 1500            全文                        全文（提取不划算）
//	1500 < total ≤ budget    全文                        提取
//	total > budget          要求补 query                提取
//
// 为什么「有 query 就提取」而不是只在超上限时提取：模型填 query 表达的
// 就是「我只要这一部分」。此前的实现在放得下时忽略 query 直接给全文 ——
// 实测一次取回返回 9 条交互的全文，其中前两轮 list_dir 的目录清单对模型的
// 问题零信息量，却照样占上下文。既无视了模型的意图，又浪费了压缩省出来的空间。
//
// 为什么极小的记录仍给全文：提取要多一次 LLM 往返（延迟 + token），
// 而一两千 token 的记录模型自己读完全没问题，转述反而有丢细节的风险。
func planRecall(total int, query string, budget int) recallPlan {
	hasQuery := strings.TrimSpace(query) != ""
	if total <= recallVerbatimFloor {
		return recallPlanVerbatim
	}
	if hasQuery {
		return recallPlanExtract
	}
	if total <= budget {
		return recallPlanVerbatim
	}
	return recallPlanNeedQuery
}

// recallByTaskID 按 task_id 取回该任务的全部交互。
//
// 三条路径由 planRecall 决定，见那里的决策表。
func recallByTaskID(
	ctx context.Context, deps recallDeps, temp *model.TaskTemp, parentTurnID int64,
	taskID, query string,
) (string, error) {
	target, err := deps.taskDAO.GetTaskByID(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("recall_context: 读取任务 %s 失败: %w", taskID, err)
	}
	if target == nil {
		return fmt.Sprintf(
			"未找到 task_id=%s 的任务记录。请核对摘要里列出的 task_id，"+
				"或直接重新执行对应的取证动作。", taskID), nil
	}
	if target.ChatID != temp.ChatID {
		log.Printf("[ctx-recall][%s] reject cross-chat task %s (chat %s != %s)",
			temp.TaskID, taskID, target.ChatID, temp.ChatID)
		return "", fmt.Errorf(
			"recall_context: task_id=%s 不属于当前对话，拒绝取回", taskID)
	}

	// 工作流任务（TaskTypeWorkflow）的 turn 只是编排决策，没有「原始对话记录」
	// 可取回；模型真正想下钻的是它的节点。此时列出节点清单（每个节点带 task_id），
	// 模型再按节点 task_id 逐节点取回 —— 这是工作流的二层下钻。
	if target.TaskType == model.TaskTypeWorkflow {
		return recallWorkflowNodes(ctx, temp, target, query)
	}

	msgs, err := buildCurrentHistory(ctx, deps.turnDAO, taskID)
	if err != nil {
		return "", fmt.Errorf("recall_context: 加载任务 %s 的交互记录失败: %w", taskID, err)
	}
	if len(msgs) == 0 {
		return fmt.Sprintf(
			"task_id=%s 没有留下可取回的交互记录（可能是空任务或记录已被清理）。",
			taskID), nil
	}

	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(renderMessageForCompaction(m))
	}
	body := sb.String()

	counter := contextmgr.GetCounter()
	total := counter.CountText(body)

	budget := resolveRecallOutputBudget(ctx, deps, temp)
	switch planRecall(total, query, budget) {
	case recallPlanVerbatim:
		log.Printf("[ctx-recall][%s] recall task %s verbatim (%d tokens, %d msgs, query=%q, budget=%d)",
			temp.TaskID, taskID, total, len(msgs), query, budget)
		return fmt.Sprintf(
			"【取回的历史记录原文】task_id=%s（共 %d 条交互，未经压缩转述）\n\n%s",
			taskID, len(msgs), body), nil

	case recallPlanNeedQuery:
		// 不静默截断：截断会让模型以为「记录就这么多」，而它真正要找的那段
		// 可能恰好在被切掉的部分。要求补 query 才能给出针对性的结果。
		return "", fmt.Errorf(
			"recall_context: task_id=%s 的记录较长（约 %d tokens，当前可取回 %d），"+
				"必须提供 query 说明你要从中查什么", taskID, total, budget)
	}

	// recallPlanExtract：按 query 做 map-reduce 提取
	log.Printf("[ctx-recall][%s] recall task %s via extraction (%d tokens, budget=%d, query=%q)",
		temp.TaskID, taskID, total, budget, query)

	extracted, err := extractFromRecalled(ctx, temp, parentTurnID, taskID, query, body, counter, budget)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"【取回的历史记录（原文约 %d tokens，已按你的 query 提取要点）】task_id=%s\nquery：%s\n\n%s",
		total, taskID, query, extracted), nil
}

// ============================================================
// 工作流二层下钻
// ============================================================

// recallWorkflowNodes 列出工作流的节点清单，供模型二层下钻。
//
// 工作流 task 自身的 turn 是编排决策（submit_plan / 节点调度），没有用户关心
// 的「原始记录」；真正有价值的是它派生的各节点子任务。这里列出所有节点
// （node_id + 名称 + task_id + 状态 + 产出摘要），模型再按节点 task_id 用
// recall_context 逐节点取回。
//
// query 只做「聚焦提示」而非过滤：不提供时列全部节点；提供时在开头说明要
// 查什么，让模型据此挑选相关节点下钻。不真正过滤 —— 过滤可能漏掉相关节点，
// 而列清单本身是轻量的。
func recallWorkflowNodes(
	ctx context.Context, temp *model.TaskTemp, target *model.Task, query string,
) (string, error) {
	var dag model.WorkflowOutputData
	if err := json.Unmarshal([]byte(target.TaskOutput), &dag); err != nil {
		log.Printf("[ctx-recall][%s] parse workflow dag %s failed: %v",
			temp.TaskID, target.TaskID, err)
		return "", fmt.Errorf(
			"recall_context: 工作流 %s 的 DAG 解析失败（%v），无法列出节点", target.TaskID, err)
	}

	var b strings.Builder
	b.WriteString("【工作流节点清单】")
	b.WriteString("工作流 task_id=" + target.TaskID + "\n")
	if s := strings.TrimSpace(dag.PlanSummary); s != "" {
		b.WriteString("总体目标：" + s + "\n")
	}
	if s := strings.TrimSpace(query); s != "" {
		b.WriteString("本次下钻目标：" + s + "\n")
	}
	b.WriteString("\n节点（可按 task_id 用 recall_context 逐节点取回）：\n")

	active := 0
	for i := range dag.Nodes {
		n := &dag.Nodes[i]
		if n.Archived {
			continue
		}
		active++
		name := n.Name
		if name == "" {
			name = n.NodeID
		}
		b.WriteString(fmt.Sprintf("- [%s] %s：%s", n.NodeID, name, taskStatusLabel(n.Status)))
		if n.TaskID != "" {
			b.WriteString(fmt.Sprintf("（task_id=%s）", n.TaskID))
		} else {
			b.WriteString("（未调度，无 task_id）")
		}
		b.WriteString("\n")
		if n.Output != nil {
			if s := strings.TrimSpace(n.Output.ReplyText); s != "" {
				b.WriteString("  产出摘要：" + truncateRunes(s, 400) + "\n")
			}
		}
	}
	if active == 0 {
		b.WriteString("（该工作流没有未归档的节点）\n")
	}
	b.WriteString("\n需要某节点的执行细节或完整产出时，用 recall_context(task_id=\"节点 task_id\", query=\"要查什么\") 取回。")
	return b.String(), nil
}

// ============================================================
// map-reduce 提取
// ============================================================

// recallSystemPrompt 提取用的 system prompt。
//
// 与压缩的 prompt 分开写，因为目标不同：压缩要「保全部要点」，
// 提取要「只答 query、但对命中的部分保留原始细节」。后者尤其重要 ——
// 模型来取回就是因为摘要丢了细节，再给它一份摘要等于白跑。
const recallSystemPrompt = `你是一个历史记录检索助手。用户会给你一段「此前对话的原始工作记录」和一个「检索目标」。

你的任务：从记录中找出与检索目标相关的内容，原样保留其中的关键细节。

要求：
1. 只回答与检索目标相关的部分，无关内容直接略过，不要通篇复述。
2. 命中的部分要保留**原始细节**：具体的报错信息、命令与其输出、代码片段、
   文件路径、字段名、数值、ID 等一律照抄原文，不要改写成「出现了一个错误」
   这类概括 —— 调用方正是因为概括不够才来取回原文的。
3. 记录里找不到相关内容时，明确回答「未在该记录中找到与检索目标相关的内容」，
   不要猜测、不要编造、不要用常识补全。
4. 直接输出结果，不要加开场白或总结段。`

// recallRunner 串起一次取回里的多次 LLM 调用，负责计数与留痕。
type recallRunner struct {
	temp         *model.TaskTemp
	parentTurnID int64
	taskID       string // 被取回的目标 task
	query        string
	client       llm.Client
	extraBody    map[string]interface{}
	callBudget   int // 单次调用的输入 token 预算
	outBudget    int // 单次调用产物的 token 预算
	counter      *contextmgr.Counter
	calls        int
}

// extractFromRecalled 把超长的取回素材按 query 提取成可放进上下文的结果。
//
// 结构与 compactOnce 同构：分块 → map（每块提取一次）→ reduce（逐层合并）。
// 复用 splitTextUnits / groupUnits 两个纯函数，不重复实现分块逻辑。
func extractFromRecalled(
	ctx context.Context, temp *model.TaskTemp, parentTurnID int64,
	targetTaskID, query, body string, counter *contextmgr.Counter, outputBudget int,
) (string, error) {
	client, extraBody, err := resolveCompactClient(ctx, temp.ChatID)
	if err != nil {
		return "", fmt.Errorf("recall_context: 解析模型失败: %w", err)
	}

	// 单次调用的输入预算：与压缩同源（按对话模型上限的 CompactCallRatio）。
	limit := resolveChatContextLimit(ctx, temp.ChatID)
	callBudget := contextmgr.NewThresholds(limit).CompactCall
	if callBudget < minCompactChunk {
		callBudget = minCompactChunk
	}

	r := &recallRunner{
		temp:         temp,
		parentTurnID: parentTurnID,
		taskID:       targetTaskID,
		query:        query,
		client:       client,
		extraBody:    extraBody,
		callBudget:   callBudget,
		outBudget:    recallExtractBudget,
		counter:      counter,
	}

	// 分块预算要扣掉 system prompt 与指令，否则整块刚好等于预算时，
	// 加上 prompt 必然顶出调用上限。
	chunkBudget := callBudget - counter.CountText(recallSystemPrompt) -
		counter.CountText(r.instruction()) - trimSuffixReserve
	if chunkBudget < minCompactChunk {
		chunkBudget = minCompactChunk
	}

	units := splitTextUnits(body, chunkBudget, counter)
	batches := groupUnits(units, chunkBudget)
	if len(batches) > 1 {
		log.Printf("[ctx-recall][%s] recall payload split: %d units -> %d batches (budget=%d)",
			temp.TaskID, len(units), len(batches), chunkBudget)
	}

	// map：每批提取一份
	level, err := r.extractBatches(ctx, batches)
	if err != nil {
		return "", err
	}

	// reduce：逐层合并到 1 份
	for depth := 1; len(level) > 1; depth++ {
		if r.calls >= maxRecallLLMCalls {
			log.Printf("[ctx-recall][%s] WARNING reached %d llm calls at depth %d with %d partials left",
				temp.TaskID, r.calls, depth, len(level))
			break
		}
		parts := make([]compactUnit, len(level))
		for i, s := range level {
			parts[i] = compactUnit{text: s, tokens: counter.CountText(s)}
		}
		next, err := r.extractBatches(ctx, groupUnits(parts, chunkBudget))
		if err != nil {
			return "", err
		}
		// 无法推进时立即停，否则退化成无限循环
		if len(next) >= len(level) {
			break
		}
		level = next
	}

	final := strings.Join(level, "\n\n")
	final = counter.TrimToTokens(final, outputBudget)
	if strings.TrimSpace(final) == "" {
		return "", fmt.Errorf("recall_context: 提取结果为空")
	}
	return final, nil
}

// extractBatches 把若干批素材各提取一份，返回中间结果列表。
//
// 任一批失败即整体失败：部分成功意味着「取回结果漏了中间一段」，
// 而模型无从知晓漏了什么，比直接报错更危险。
func (r *recallRunner) extractBatches(
	ctx context.Context, batches [][]compactUnit,
) ([]string, error) {
	out := make([]string, 0, len(batches))
	for _, batch := range batches {
		parts := make([]string, len(batch))
		for j, u := range batch {
			parts[j] = u.text
		}
		s, err := r.extract(ctx, parts)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// instruction 提取指令（含 query 与产物预算）。
func (r *recallRunner) instruction() string {
	return fmt.Sprintf(
		"\n\n【检索目标】\n%s\n\n请从以上记录中提取与检索目标相关的内容，"+
			"保留原始细节，控制在 %d 个 token 以内。",
		r.query, r.outBudget)
}

// extract 执行一次提取 LLM 调用。
func (r *recallRunner) extract(ctx context.Context, parts []string) (string, error) {
	if r.calls >= maxRecallLLMCalls {
		// 次数用尽：不再调 LLM，直接把素材硬截断当结果。
		// 丢细节但保住「不超限」这条硬约束。
		return r.counter.TrimToTokens(strings.Join(parts, "\n\n"), r.outBudget), nil
	}
	r.calls++

	instruction := r.instruction()
	avail := r.callBudget - r.counter.CountText(recallSystemPrompt) -
		r.counter.CountText(instruction) - trimSuffixReserve
	if avail < minCompactChunk {
		avail = minCompactChunk
	}
	body := strings.Join(parts, "\n\n")
	if r.counter.CountText(body) > avail {
		body = r.counter.TrimToTokens(body, avail)
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: recallSystemPrompt},
		{Role: llm.RoleUser, Content: body + instruction},
	}

	opts := llm.DefaultOptions()
	// 取回要的是忠实摘录而非改写，温度压到接近确定性
	opts.Temperature = 0.2
	opts.ExtraBody = r.extraBody

	// 留痕：纯旁路，落库失败绝不阻断取回
	turn := r.beginTurn(ctx, messages, opts)

	// 不传任何工具：取回是内部动作，从协议层杜绝模型返回 tool_calls
	result, err := r.client.ChatWithTools(ctx, messages, nil, llm.ToolChoiceNone, opts)
	if err != nil {
		failRecallTurn(ctx, turn, err)
		return "", fmt.Errorf("recall_context: LLM 调用失败: %w", err)
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		failRecallTurn(ctx, turn, errors.New("llm returned empty extraction"))
		return "", fmt.Errorf("recall_context: LLM 返回空结果")
	}
	text = r.counter.TrimToTokens(text, r.outBudget)
	finishRecallTurn(ctx, turn, text, result.Usage)
	return text, nil
}

// ============================================================
// 取回调用的调试留痕
// ============================================================

// beginTurn 开启一条取回 turn。任何失败都只记日志并返回 nil ——
// 留痕失败不该让取回本身失败。
func (r *recallRunner) beginTurn(
	ctx context.Context, messages []llm.Message, opts llm.Options,
) *model.TaskTurn {
	if !dao.Ready() || r.temp == nil {
		return nil
	}
	phase := model.TaskPhase(r.temp.Phase)
	if phase == 0 {
		phase = model.PhaseExecuting
	}
	snap := &model.TurnLLMInput{
		Model: r.client.Name(),
		Options: map[string]interface{}{
			"temperature":     opts.Temperature,
			"recall_call_seq": r.calls,
			"recall_task_id":  r.taskID,
			"recall_query":    r.query,
			"parent_turn_id":  r.parentTurnID,
		},
	}
	if len(r.extraBody) > 0 {
		snap.Options["extra_body"] = r.extraBody
	}
	for _, m := range messages {
		if m.Role == llm.RoleSystem && snap.SystemPrompt == "" {
			snap.SystemPrompt = m.Content
			continue
		}
		snap.Messages = append(snap.Messages, model.TurnMessage{
			Role:    string(m.Role),
			Content: m.Content,
		})
	}

	turn, err := (&dao.TaskTurnDAO{}).BeginTurn(ctx, dao.BeginTurnParams{
		TaskID: r.temp.TaskID,
		ChatID: r.temp.ChatID,
		// 父任务必须带上：子任务（工作流节点 / deep_search）内取回时同样留痕，
		// 缺了它这条 turn 会被 buildSegmentedHistory 当成主对话自己的 turn
		// 回放进历史 —— 取回动作本身不该出现在对话里。
		ParentTaskID: r.temp.ParentTaskID,
		TurnType:     model.TurnTypeContextRecall,
		Phase:        phase,
		Stage:        model.TaskStage(r.temp.Stage),
		// Round 与同阶段的 tool_call turn 同源取值，保证调试页时间线的
		// 轮次连续（复用压缩那套取法）。
		Round: compactLoopRound(ctx, r.temp.TaskID, phase),
		Model: r.client.Name(),
		Input: snap,
	})
	if err != nil {
		log.Printf("[ctx-recall][%s] begin recall turn failed (ignore): %v",
			r.temp.TaskID, err)
		return nil
	}
	return turn
}

// finishRecallTurn 回填取回 turn 的输出与 token。
func finishRecallTurn(ctx context.Context, turn *model.TaskTurn, text string, usage llm.Usage) {
	if turn == nil {
		return
	}
	if err := (&dao.TaskTurnDAO{}).FinishTurn(ctx, turn.ID, dao.FinishTurnParams{
		Output:           &model.TurnLLMOutput{Text: text},
		TotalTokens:      usage.TotalTokens,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
	}); err != nil {
		log.Printf("[ctx-recall][%s] finish recall turn %d failed (ignore): %v",
			turn.TaskID, turn.ID, err)
		return
	}
	publishTurnDone(turn.TaskID, turn.ID, turn.TurnSeq)
}

// failRecallTurn 取回调用失败时把 turn 标成 failed。
func failRecallTurn(ctx context.Context, turn *model.TaskTurn, reason error) {
	if turn == nil {
		return
	}
	if err := (&dao.TaskTurnDAO{}).FailTurn(ctx, turn.ID,
		fmt.Errorf("context recall failed: %w", reason), nil); err != nil {
		log.Printf("[ctx-recall][%s] fail recall turn %d failed (ignore): %v",
			turn.TaskID, turn.ID, err)
		return
	}
	publishTurnDone(turn.TaskID, turn.ID, turn.TurnSeq)
}
