// Package actuator — agent 会话历史重建
//
// agent loop 的形态是 functional_call ↔ tool_call 反复流转，
// 每次进入 functional_call 都需要拿到「截至目前的完整对话历史」再调 LLM。
//
// 历史不放在内存里，而是每次从 task_turn 表重建。原因：
//   - 调度器可能多副本部署，任务的不同 turn 可能落在不同进程
//   - 进程重启后内存态丢失，但任务仍在 DB 里等待继续推进
//   - task_turn 本就是完整的执行记录，重建历史是它的自然用途
//
// 范围是「整个对话」而非单个 task：用户连续追问时（每次追问是一个新 task），
// 模型必须看到之前几轮问了什么、调过哪些工具，否则会重复劳动或答非所问。
//
// 重建规则（按时间升序遍历该 chat 的所有 turn）：
//   - 进入新 task 时插入该 task 的 user 输入，否则模型看到的是
//     一串无来由的 assistant/tool 消息，不知道在回答什么问题
//   - 但该 task 若一条历史消息都没产出（典型：LLM 调用失败的 task），
//     它的 user 输入也不插入 —— 否则会堆出连续的 user 消息
//   - functional_call turn 只有成功才入历史（失败的调用没有产出）
//   - 规划期（phase=planning）的调用额外过滤 submit_plan 协议工具
//     （执行期没有该工具，回放会 400）
//   - tool_call turn 成功失败都入（失败必须回喂，否则模型会重复请求同一工具）
//   - workflow turn 跳过
package actuator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// ============================================================
// before 段历史缓存 —— 同一 task 执行期间只构建一次
// ============================================================
//
// agent loop 每轮 functional_call ↔ tool_call 都会重建历史。
// 其中「本 task 之前的历史 task 交互」（before 段）在同一个 task 推进期间
// 是恒定不变的 —— 当前 task 是唯一在推进的任务，历史 task 早已收尾。
// 只有 current 段（本 task 的 turn）随工具调用增长。
//
// 因此把 before 段按 taskID 缓存起来，每轮只重建 current 段，
// 省掉重复的「拉全对话 turn + 反查各 task user 输入 + 序列化」开销。
// 缓存的是**分段**结构而不是拍平后的消息：规划期要按段计量各块 token
// （工作流摘要段必须单独成块，见 contextBlocks.WorkflowSummary），
// 拍平之后就认不出哪条消息是工作流摘要了。
type agentHistoryCache struct {
	mu     sync.Mutex
	before map[string][]historySegment // key: taskID
}

// agentHistoryCacheMaxEntries 缓存条目上限，超过则整体清空（进程内缓存，量级有限，
// 用清空兜底防内存泄漏，避免为过期清理引入复杂逻辑）。
const agentHistoryCacheMaxEntries = 500

var globalAgentHistoryCache = &agentHistoryCache{before: make(map[string][]historySegment)}

// getBeforeSegs 取缓存的分段历史。
func (c *agentHistoryCache) getBeforeSegs(taskID string) ([]historySegment, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.before[taskID]
	return v, ok
}

func (c *agentHistoryCache) setBeforeSegs(taskID string, segs []historySegment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.before) >= agentHistoryCacheMaxEntries {
		c.before = make(map[string][]historySegment)
	}
	c.before[taskID] = segs
}

func (c *agentHistoryCache) delete(taskID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.before, taskID)
}

// buildAgentHistory 从 task_turn 表重建 agent loop 的对话历史。
//
// 覆盖整个对话（chatID）的全部 turn，包含历史 task，并按 currentTaskID
// 切成两段返回：
//   - before：本 task 之前的历史（含各历史 task 的 user 提问）
//   - current：本 task 内已发生的 assistant/tool 交互
//
// 调用方需在两段之间插入本轮 user 输入，得到时序正确的完整消息序列：
//
//	system + before + 本轮 user 输入 + current
//
// 不含 system prompt —— 那属于调用方职责。
//
// before 段（历史 task 交互）在同一 task 执行期间恒定不变，按 taskID 缓存；
// 缓存命中时只重建 current 段（本 task 的 turn），省掉重复的全量拉取。
func buildAgentHistory(
	ctx context.Context,
	turnDAO *dao.TaskTurnDAO, taskDAO *dao.TaskDAO,
	chatID, currentTaskID string,
) (before, current []llm.Message, err error) {
	segs, current, err := buildAgentHistorySegments(ctx, turnDAO, taskDAO, chatID, currentTaskID)
	if err != nil {
		return nil, nil, err
	}
	return flattenSegments(segs), current, nil
}

// buildAgentHistorySegments 与 buildAgentHistory 同源，但保留段结构。
//
// 规划期用这个：它要按段计量（工作流摘要段单独成块），拍平后信息就丢了。
// 两者共用同一份缓存，不会重复拉全量 turn。
func buildAgentHistorySegments(
	ctx context.Context,
	turnDAO *dao.TaskTurnDAO, taskDAO *dao.TaskDAO,
	chatID, currentTaskID string,
) ([]historySegment, []llm.Message, error) {
	if cached, ok := globalAgentHistoryCache.getBeforeSegs(currentTaskID); ok {
		current, err := buildCurrentHistory(ctx, turnDAO, currentTaskID)
		if err != nil {
			return nil, nil, err
		}
		return cached, current, nil
	}

	segs, current, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, nil)
	if err != nil {
		return nil, nil, err
	}
	globalAgentHistoryCache.setBeforeSegs(currentTaskID, segs)
	return segs, current, nil
}

// buildFullHistory 完整重建两段历史（缓存 miss 时调用）。
func buildFullHistory(
	ctx context.Context,
	turnDAO *dao.TaskTurnDAO, taskDAO *dao.TaskDAO,
	chatID, currentTaskID string,
) (before, current []llm.Message, err error) {
	segs, current, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, nil)
	if err != nil {
		return nil, nil, err
	}
	return flattenSegments(segs), current, nil
}

// ============================================================
// 分段历史 —— 上下文压缩的基本单位
// ============================================================

// segmentKind 历史段的类型，决定它能否被压缩。
type segmentKind int

const (
	// segKindConversation 普通对话 task 的交互段。可被压缩。
	segKindConversation segmentKind = iota
	// segKindWorkflowSummary 工作流执行摘要段。
	//
	// 它本身已经是「把整个 DAG 的子任务 turn 压成一段文本」的产物
	// （见 renderWorkflowSummary），属于五块输入模型里独立的第 3 块，
	// 不再参与二次压缩：既省一次 LLM 调用，也避免「摘要的摘要」
	// 把工作流最终产出（用户真正要的交付物）越压越失真。
	segKindWorkflowSummary
)

// historySegment 历史里归属同一个 task 的连续消息段。
//
// 引入它的原因：压缩要求「一个 task 的所有 turn 要么都压、要么都不压」。
// 原先的 buildFullHistory 直接产出拍平的 []llm.Message，task 归属在
// 拍平过程中就丢了，无法在消息序列上还原 task 边界 —— 于是先分段、
// 再按需拍平。
type historySegment struct {
	// TaskID 归属任务。segKindWorkflowSummary 段为工作流任务 ID。
	TaskID string
	Kind   segmentKind
	// Messages 该段的消息（对话段含该 task 的 user 提问 + assistant/tool 交互）。
	Messages []llm.Message
	// Tokens 该段的 token 数，由 contextmgr 计量后缓存，避免压缩循环里重复编码。
	Tokens int
}

// buildSegmentedHistory 重建历史并按 task 分段。
//
// covered 为「已被压缩摘要覆盖」的 task ID 集合：命中的 task 其 turn
// 一律不回放原始详情（由调用方注入的摘要代表），这是压缩生效的关键。
// 传 nil 表示不做任何压缩过滤（等价于原始全量回放）。
//
// 返回的 before 是分段形态、current 仍是拍平的消息列表 —— 当前 task
// 按决策永不参与压缩，没有分段的必要。
func buildSegmentedHistory(
	ctx context.Context,
	turnDAO *dao.TaskTurnDAO, taskDAO *dao.TaskDAO,
	chatID, currentTaskID string,
	covered map[string]bool,
) (before []historySegment, current []llm.Message, err error) {
	turns, err := turnDAO.ListTurnsByChatID(ctx, chatID)
	if err != nil {
		return nil, nil, fmt.Errorf("build agent history: list turns: %w", err)
	}
	if len(turns) == 0 {
		return nil, nil, nil
	}

	// 历史 task 的 user 输入，用于在 task 边界插入
	inputs := loadTaskInputs(ctx, taskDAO, chatID)

	// 工作流任务摘要：子任务 turn 不逐条回放，而是压缩成一份摘要（见优化4）。
	workflowSummaries := loadWorkflowSummaries(ctx, taskDAO, turns)

	segs := make([]historySegment, 0, 16)
	currentMsgs := make([]llm.Message, 0, 8)
	lastTaskID := ""

	// pendingUser 暂存「历史 task 的 user 提问」，等该 task 真的产出
	// assistant/tool 消息时才落地。
	//
	// 失败的 task（LLM 报错、stage 中途失败）往往一条历史消息都产不出：
	// planning turn 被 turnToMessage 跳过，functional_call 失败被过滤。
	// 若无条件插入提问，会得到一串连续的 user 消息 —— 模型看到用户
	// 连问 5 遍同一句话却没有任何回应，容易误判为「之前答错了」或强行改写答案，
	// 部分 provider 也会因连续同角色消息报错。
	pendingUser := ""

	// lastParentTaskID 上一条 turn 的父任务（子任务 turn 才有值）。
	//
	// 子任务的 turn 不逐条回放（那会把抓取的网页全文等巨量 tool 结果全塞进
	// 上下文，是历史任务上下文超限的直接根因），改为在「跨入某个父任务的
	// 子任务」这一边界注入一次该父任务的摘要。
	//
	// 边界必须按**父任务身份**判定，不能退化成「现在是否在子任务里」的布尔量：
	// 一个对话里可能先后存在多个父任务（先 deep_search 子任务、后工作流子任务），
	// 且它们的子任务 turn 在 id 序上是交错的。用布尔量时，只要前一段子任务
	// 把标志置起来，后面真工作流的边界就被吞掉 —— 实测 chat 8a3c060c 的
	// 工作流摘要因此一次都没注入（摘要本身生成成功，430 字符，就是没落地）。
	lastParentTaskID := ""

	// injectedSummary 已注入过摘要的父任务。
	// 交错回放会让同一父任务的边界出现多次，这里保证只注入第一份
	// （内容每次都由 loadWorkflowSummaries 取当前最新 DAG 快照，
	// 多份完全一样，留一份即可）。
	injectedSummary := make(map[string]bool)

	// appendToSeg 把消息追加到归属 taskID 的对话段；段不存在则新建。
	// 同一 task 的 turn 在 ListTurnsByChatID 的排序下是连续的，
	// 因此只需检查末尾段即可，无需全表查找。
	appendToSeg := func(taskID string, msgs ...llm.Message) {
		if n := len(segs); n > 0 &&
			segs[n-1].Kind == segKindConversation && segs[n-1].TaskID == taskID {
			segs[n-1].Messages = append(segs[n-1].Messages, msgs...)
			return
		}
		segs = append(segs, historySegment{
			TaskID:   taskID,
			Kind:     segKindConversation,
			Messages: append([]llm.Message{}, msgs...),
		})
	}

	for _, t := range turns {
		// 上下文压缩 turn 不是对话的一部分，只是压缩动作自身的留痕，
		// 直接跳过（turnToMessage 的默认分支同样会丢弃它，这里提前拦下
		// 是为了不让它改动 lastTaskID / pendingUser 这些边界状态）。
		if t.TurnType == model.TurnTypeContextCompact {
			continue
		}

		// 工作流子任务的 turn（parent_task_id 非空）：跳过原始回放，边界处注入摘要。
		if t.ParentTaskID != "" {
			if t.ParentTaskID != lastParentTaskID {
				// 跨入**另一个父任务**的子任务：先把触发它的用户提问落地，
				// 再注入这份父任务的摘要（代替全部子任务 tool turn）。
				if pendingUser != "" {
					appendToSeg(lastTaskID, llm.Message{
						Role:    llm.RoleUser,
						Content: pendingUser,
					})
					pendingUser = ""
				}
				if summary := workflowSummaries[t.ParentTaskID]; summary != "" &&
					!injectedSummary[t.ParentTaskID] {
					// 独立成段：它不参与压缩，必须能在段级别被识别出来
					segs = append(segs, historySegment{
						TaskID: t.ParentTaskID,
						Kind:   segKindWorkflowSummary,
						Messages: []llm.Message{{
							Role:    llm.RoleAssistant,
							Content: summary,
						}},
					})
					injectedSummary[t.ParentTaskID] = true
				}
				lastParentTaskID = t.ParentTaskID
			}
			lastTaskID = t.TaskID
			continue
		}

		isCurrent := t.TaskID == currentTaskID

		// 已被压缩摘要覆盖的历史 task：整段跳过，不回放原始详情。
		// 放在 isCurrent 判定之后是多余的（当前 task 永不入 covered），
		// 但显式排除更抗未来改动。
		if !isCurrent && covered[t.TaskID] {
			lastTaskID = t.TaskID
			pendingUser = ""
			continue
		}

		// 跨到新的历史 task：暂存这个 task 的 user 提问。
		// 本 task 的输入由调用方插入，这里不重复。
		if t.TaskID != lastTaskID {
			lastTaskID = t.TaskID
			if !isCurrent {
				pendingUser = inputs[t.TaskID]
			} else {
				pendingUser = ""
			}
		}

		msg, ok := turnToMessage(t)
		if !ok {
			// 不产消息的 turn（失败的 turn、规划期 turn 等）**不能**重置边界：
			// 工作流跑的过程中，用户每发一条新消息都会先产生一个规划期 turn，
			// 它夹在两段子任务 turn 中间却没有任何消息产出。若在这里重置，
			// 下一段子任务 turn 会重新触发边界判定 —— 实测同一条工作流的
			// 摘要被重复注入 3 次（626 token × 3），纯属浪费上下文。
			continue
		}
		lastParentTaskID = ""
		if isCurrent {
			// 来源 turn 记账（见 buildCurrentHistory），供当前 task 压缩水位使用。
			msg.SourceTurnID = t.ID
			currentMsgs = append(currentMsgs, msg)
		} else {
			// 该 task 确实有产出，此时提问才有意义
			if pendingUser != "" {
				appendToSeg(t.TaskID, llm.Message{
					Role:    llm.RoleUser,
					Content: pendingUser,
				})
				pendingUser = ""
			}
			appendToSeg(t.TaskID, msg)
		}
	}

	// 摘要去重由上面的 injectedSummary 保证：每个父任务只在第一次跨入时
	// 注入一份，因此这里无需再按 TaskID 做「保留最后一份」的后处理。
	//
	// （历史做法是先允许重复注入、再在末尾删掉前面的。交错回放下重复份数
	// 不可控，直接按父任务身份判重更省心，位置也更贴合时序。）

	// 段内清理孤立 tool 结果。
	//
	// 按段而非全局做：一个 task 的 turn 是连续的，assistant tool_calls 与
	// 其 tool 结果必然同段，段内配对是自足的。反而全局做会让「前一个 task
	// 末尾的 assistant」与「后一个 task 开头的 tool」被误判成一对。
	out := make([]historySegment, 0, len(segs))
	for i := range segs {
		segs[i].Messages = sortToolResultsByIndex(dropOrphanToolResults(segs[i].Messages))
		if len(segs[i].Messages) == 0 {
			continue // 清理后空段直接丢弃
		}
		out = append(out, segs[i])
	}

	// 与 buildCurrentHistory 同源：当前段同样要老化，否则缓存未命中时
	// （首轮 / 缓存被清）会退化成全量回放。
	return out, mergeAdjacentUsers(sortToolResultsByIndex(dropOrphanToolResults(ageCurrentToolResults(currentMsgs)))), nil
}

// flattenSegments 把分段历史拍平成消息列表。
//
// 相邻 user 的折叠在拍平后统一做：段内不会出现连续 user，但
// 「某 task 只剩一条 user 提问」与「下一个 task 的提问」拍平后会相邻。
func flattenSegments(segs []historySegment) []llm.Message {
	if len(segs) == 0 {
		return nil
	}
	total := 0
	for i := range segs {
		total += len(segs[i].Messages)
	}
	out := make([]llm.Message, 0, total)
	for i := range segs {
		out = append(out, segs[i].Messages...)
	}
	return mergeAdjacentUsers(out)
}

// buildCurrentHistory 只重建 current 段（本 task 已发生的交互）。
// before 段命中缓存时调用，避免重复拉取整个对话的 turn。
func buildCurrentHistory(
	ctx context.Context, turnDAO *dao.TaskTurnDAO, currentTaskID string,
) ([]llm.Message, error) {
	turns, err := turnDAO.ListTurnsByTaskID(ctx, currentTaskID)
	if err != nil {
		return nil, fmt.Errorf("build current history: %w", err)
	}
	msgs := make([]llm.Message, 0, 8)
	for _, t := range turns {
		if msg, ok := turnToMessage(t); ok {
			// 记下来源 turn：当前 task 的交互被压缩时，水位要记录「压掉了
			// 哪些 turn」，后续才能按 turn 取回原始记录。
			msg.SourceTurnID = t.ID
			msgs = append(msgs, msg)
		}
	}
	// 工具结果老化：长任务里几十条工具结果常驻上下文，只保留最近若干条全文，
	// 更早的降级为摘要级 / 带取回句柄的占位（见 tool_result_aging.go）。
	// 放在 dropOrphan 之前：老化只改内容不动角色，配对关系不受影响。
	return mergeAdjacentUsers(sortToolResultsByIndex(dropOrphanToolResults(ageCurrentToolResults(msgs)))), nil
}

// mergeAdjacentUsers 折叠相邻的 user 消息，只保留最后一条。
//
// pendingUser 机制已经消除了「失败 task 留下孤立提问」的主因，
// 这里作为兜底：仍可能出现连续 user（例如某 task 只产出了被
// dropOrphanToolResults 丢弃的孤立 tool 消息）。
//
// 保留最后一条而非第一条 —— 连续提问时后面的通常是修正或补充，
// 更接近用户当下的真实意图。内容完全相同时等价。
func mergeAdjacentUsers(messages []llm.Message) []llm.Message {
	if len(messages) < 2 {
		return messages
	}
	out := make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == llm.RoleUser && len(out) > 0 && out[len(out)-1].Role == llm.RoleUser {
			out[len(out)-1] = m // 覆盖前一条 user
			continue
		}
		out = append(out, m)
	}
	return out
}

// collapseAdjacentSameRole 把相邻的同角色消息合并成一条。
//
// 为什么必须有这一步：压缩摘要是以 assistant 消息注入的，而它相邻的
// 往往也是 assistant —— 「历史摘要」后面紧跟「工作流摘要」段，末尾的
// 「当前 task 摘要」前面也可能就是 assistant。真实数据里 task 7c09cf38
// 的第 44 轮输入就是 assistant, assistant, user, assistant。
//
// 后果因 provider 而异：
//   - OpenAI 兼容协议：接受，但模型会把两段当成一个说话人连续输出，归因变差；
//   - Anthropic：服务端自动合并成一轮，不报错但行为同上；
//   - Gemini：**直接 400** —— contents 要求 user/model 严格交替。
//
// 与其逐个 provider 兜，不如在组装出口统一折叠，任何协议都拿到交替序列。
//
// 刻意不合并 tool 结果：一次并行调用本就会产出多条连续 tool 消息，
// 合并它们会破坏 functionResponse 的结构。
//
// 带 ToolCalls 的 assistant 照常合并，工具调用按序并入前一条 ——
// 真实数据里就撞上了这个形状（chat 8a3c060c）：当前摘要后紧跟着一条
// 带 2 个 deep_search 调用的 assistant，不合并就留下连续 assistant。
// 合并后 tool_calls 仍在这条消息上、结果仍紧随其后，配对关系不受影响；
// 反倒修好了「带 tool_calls 的 assistant 后面不是工具结果」这个本就会被
// provider 判 400 的非法形态。
//
// 多模态消息同理，图片按序并入前一条（content 拼接 + Images 累加）。
//
// system 消息不参与合并（整段序列里它本就只出现一次在最前）。
func collapseAdjacentSameRole(messages []llm.Message) []llm.Message {
	if len(messages) < 2 {
		return messages
	}
	out := make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		if len(out) == 0 {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant {
			out = append(out, m)
			continue
		}
		if prev.Role != m.Role {
			out = append(out, m)
			continue
		}
		switch {
		case prev.Content == "":
			prev.Content = m.Content
		case m.Content == "":
			// 保留 prev 原本的内容
		default:
			prev.Content = prev.Content + "\n\n" + m.Content
		}
		if len(m.Images) > 0 {
			prev.Images = append(prev.Images, m.Images...)
		}
		if len(m.ToolCalls) > 0 {
			prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
		}
	}
	return out
}

// turnToMessage 把单条 turn 转成消息，返回 false 表示该 turn 不入历史。
func turnToMessage(t *model.TaskTurn) (llm.Message, bool) {
	switch t.TurnType {
	case model.TurnTypeFunctionalCall, model.TurnTypePlanningLegacy:
		// LLM 调用只有成功才入历史。失败的调用没有产出，
		// 放进去反而会让模型看到空的 assistant 消息。
		if t.Status != model.TurnStatusSuccess {
			return llm.Message{}, false
		}
		// 规划期的调用要额外滤掉 submit_plan（执行期没有该工具，回放会 400）。
		// 判据是 phase 而非 turn_type —— 两者现在是正交维度。
		if t.IsPlanning() {
			return planningTurnToMessage(t)
		}
		return functionalCallTurnToMessage(t)

	case model.TurnTypeToolCall:
		// 工具调用无论成功失败都要入历史。
		// 失败必须回喂 —— 否则模型看到「自己请求了工具却没有任何结果」，
		// 下一轮会重复请求同一个失败工具，形成死循环。
		// 把错误信息给它，它才能换参数、换工具或向用户说明。
		if t.Status == model.TurnStatusRunning {
			// 仍在执行中的 turn 还没有结果
			return llm.Message{}, false
		}
		return toolCallTurnToMessage(t)

	default:
		// workflow turn 不进 agent 对话历史
		return llm.Message{}, false
	}
}

// loadTaskInputs 读取该对话下所有 task 的 user 输入文本。
// 查询失败不阻断 —— 缺了 user 消息历史仍可用，只是上下文弱一些。
func loadTaskInputs(
	ctx context.Context, taskDAO *dao.TaskDAO, chatID string,
) map[string]string {
	if taskDAO == nil {
		return nil
	}
	tasks, err := taskDAO.ListTasksByChatID(ctx, chatID)
	if err != nil {
		return nil
	}
	inputs := make(map[string]string, len(tasks))
	for _, t := range tasks {
		// 历史轮次只渲染附件清单、不做内联预览（wsDir 传空）：
		// 预览内容会随每轮历史被反复回放，迅速吃满上下文；
		// 模型若需要历史附件的内容，可按清单里的路径 read_file。
		if text := renderUserInput(t.TaskInput.Data, ""); strings.TrimSpace(text) != "" {
			inputs[t.TaskID] = text
		}
	}
	return inputs
}

// loadWorkflowSummaries 从 turn 列表里找出所有工作流任务（子任务 turn 的
// ParentTaskID），读取其 task_output 并渲染成摘要。key 为工作流任务 ID。
//
// 对话级单 Agent 任务构建 LLM 请求时，工作流任务不逐条回放子任务 tool turn，
// 而是压缩成「每节点状态 + 最终产出」摘要，防止上下文被抓取全文等巨量结果撑爆。
//
// 注意：并非所有带 ParentTaskID 的子任务都属于工作流。deep_search 产生的搜索
// 子任务其 ParentTaskID 指向的是对话任务（task_type=conversation），它的结论
// 已经通过 deep_search 工具结果回喂主对话，此处须按 task_type 过滤掉，否则会
// 给父对话任务错误地注入一句「此前对话通过工作流处理了若干子步骤」占位摘要。
func loadWorkflowSummaries(
	ctx context.Context, taskDAO *dao.TaskDAO, turns []*model.TaskTurn,
) map[string]string {
	if taskDAO == nil {
		return nil
	}
	wfIDs := make(map[string]bool)
	for _, t := range turns {
		if t.ParentTaskID != "" {
			wfIDs[t.ParentTaskID] = true
		}
	}
	if len(wfIDs) == 0 {
		return nil
	}

	summaries := make(map[string]string, len(wfIDs))
	for wfID := range wfIDs {
		wf, err := taskDAO.GetTaskByID(ctx, wfID)
		if err != nil || wf == nil {
			continue
		}
		// 只对真正的 workflow 任务渲染摘要；deep_search 子任务的父对话任务跳过。
		if wf.TaskType != model.TaskTypeWorkflow {
			continue
		}
		summary := ""
		var dag model.WorkflowOutputData
		if json.Unmarshal([]byte(wf.TaskOutput), &dag) == nil {
			summary = renderWorkflowSummary(&dag, wfID)
		}
		if summary == "" {
			// 兜底：任务输出缺失，仍给一句占位，避免模型完全丢失
			// 「这里发生过一次工作流」的上下文。
			summary = "此前对话通过工作流处理了若干子步骤（执行细节见调试页）。"
		}
		summaries[wfID] = summary
	}
	return summaries
}

// workflowSummaryMaxLeafOutputChars 摘要中「最终产出（叶子节点）」的最大字符数。
//
// 最终产出是用户真正要的交付物（文章、报告、代码等），应尽量完整保留，
// 否则下游对话任务（如「把文章发给我」）拿不到全文。真正会撑爆上下文的是
// 中间调研节点抓取的网页全文，那些已经在摘要里只列状态、不输出内容。
// 这里设一个较大但可控的安全上限，仅用于兜底「叶子产出本身是超大文本」的极端情况。
const workflowSummaryMaxLeafOutputChars = 30000

// renderWorkflowSummary 把工作流 DAG 压缩成一段简短摘要：
// 总体目标 + 每个节点的状态 + 最终整体产出（叶子节点的输出）。
//
// wfTaskID 是这段摘要的检索句柄：摘要本身是「已压缩」产物，模型想知道
// 某个节点当时到底做了什么时，只能凭这里的 ID 回取原始记录；没有它，
// 模型手里就只有一段没有指针的散文。
func renderWorkflowSummary(wf *model.WorkflowOutputData, wfTaskID string) string {
	if wf == nil || len(wf.Nodes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【此前工作流执行摘要】\n")
	if wfTaskID != "" {
		b.WriteString("工作流任务 ID：" + wfTaskID + "\n")
	}
	if s := strings.TrimSpace(wf.PlanSummary); s != "" {
		b.WriteString("总体目标：" + s + "\n")
	}
	// 告知句柄用途：下面每个节点都带 task_id，但只给 ID 不说用途时模型
	// 不会想到能回取。工具名从常量取，避免与真实注册名不一致。
	b.WriteString("（本摘要只保留结论。需要某个节点的执行细节或原始产出时，用 " +
		model.RecallContextToolName + "(task_id=\"节点的 task_id\", query=\"要查什么\") 取回。）\n")

	// 找出叶子节点（没有被任何其他节点依赖的节点），其输出即最终产出。
	// 已归档节点自身不作为叶子（它的输出不是交付物），但它的 depends_on 仍要
	// 记账：否则它那个「已完成的上游」会被误判成叶子，把一份中间产物当成
	// 最终产出讲给用户。
	hasDownstream := make(map[string]bool, len(wf.Nodes))
	for i := range wf.Nodes {
		for _, dep := range wf.Nodes[i].DependsOn {
			hasDownstream[dep] = true
		}
	}

	b.WriteString("各节点状态：\n")
	var leafOutputs []string
	for i := range wf.Nodes {
		n := &wf.Nodes[i]
		if n.Archived {
			continue
		}
		name := n.Name
		if name == "" {
			name = n.NodeID
		}
		// 节点 ID 与它的任务 ID 一并带上：节点名会重名、也会在追加规划里
		// 被改写，只有 ID 是稳定句柄。未调度的节点还没有 task_id，退化成
		// 只给节点 ID（工作流任务 ID 仍可用于整体下钻）。
		if n.TaskID != "" {
			fmt.Fprintf(&b, "- [%s] %s（task_id=%s）：%s\n",
				n.NodeID, name, n.TaskID, taskStatusLabel(n.Status))
		} else {
			fmt.Fprintf(&b, "- [%s] %s：%s\n", n.NodeID, name, taskStatusLabel(n.Status))
		}
		if !hasDownstream[n.NodeID] && n.Output != nil {
			if text := strings.TrimSpace(n.Output.ReplyText); text != "" {
				leafOutputs = append(leafOutputs,
					name+"：\n"+truncateRunes(text, workflowSummaryMaxLeafOutputChars))
			}
		}
	}

	if len(leafOutputs) > 0 {
		b.WriteString("\n最终整体产出：\n")
		for _, out := range leafOutputs {
			b.WriteString(out)
			b.WriteString("\n")
		}
	}

	// 工作流产物清单。必须带上：长文已落盘时 reply_text 只剩摘要，
	// 用户后续追问「文章写得怎么样了」时，模型要能按路径 read_file 取全文，
	// 而不是基于摘要复述或臆造。
	if len(wf.OutputFiles) > 0 {
		b.WriteString("\n工作流产出的文件（可用 read_file 读取完整内容）：\n")
		for _, f := range wf.OutputFiles {
			p := f.Path
			if p == "" {
				p = f.Name
			}
			fmt.Fprintf(&b, "- `%s`", p)
			if f.Size > 0 {
				fmt.Fprintf(&b, "（%s）", humanSize(f.Size))
			}
			// 带上可下载访问链接：用户追问「产物在哪 / 给个链接」时，模型
			// 直接引用这个 URL 即可，不要自己编 sandbox:/mnt/data 之类的假路径。
			if f.URL != "" {
				fmt.Fprintf(&b, "\n  下载链接：%s", f.URL)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// taskStatusLabel 返回任务状态的中文标签（摘要展示用）。
func taskStatusLabel(s model.TaskStatus) string {
	switch s {
	case model.TaskStatusQueued:
		return "排队中"
	case model.TaskStatusRunning:
		return "执行中"
	case model.TaskStatusCompleted:
		return "已完成"
	case model.TaskStatusFailed:
		return "失败"
	case model.TaskStatusStopped:
		return "已终止"
	case model.TaskStatusPartialFailed:
		return "部分失败"
	case model.TaskStatusAbandoned:
		return "已废弃"
	default:
		return fmt.Sprintf("状态%d", int(s))
	}
}

// dropOrphanToolResults 丢弃前面没有对应 assistant tool_calls 的工具结果消息。
//
// 会出现这种情况：某个 task 的 functional_call turn 失败（被过滤），
// 但它之前发起的 tool_call turn 已经落库。留下孤立的 tool 消息，
// Gemini 会因「functionResponse 没有对应的 functionCall」判 400。
func dropOrphanToolResults(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	// pending 记录上一条 assistant 消息声明了哪些工具调用
	pending := map[string]int{}

	for _, m := range messages {
		switch m.Role {
		case llm.RoleAssistant:
			pending = map[string]int{}
			for _, tc := range m.ToolCalls {
				pending[tc.Name]++
			}
			out = append(out, m)

		case llm.RoleTool:
			if pending[m.ToolName] <= 0 {
				continue // 孤立的工具结果，丢弃
			}
			pending[m.ToolName]--
			out = append(out, m)

		default:
			// 新的 user 提问意味着上一轮已经收尾
			pending = map[string]int{}
			out = append(out, m)
		}
	}
	return out
}

// sortToolResultsByIndex 把每个 assistant(tool_calls) 之后的连续 tool 结果段，
// 按 ToolCallIndex（声明顺序）稳定重排。
//
// 工具调用是并发执行的，turn_seq 按完成顺序分配、未必等于声明顺序；重建历史
// 时若不重排，「参数（如 read_file 的 path）与返回内容」会错位，模型拿到对不上
// 的结果反复重试，最终触发死循环熔断。
//
// 旧数据没有 index（全 0）时，稳定排序保持原序（= turn_seq 顺序），行为不变。
func sortToolResultsByIndex(messages []llm.Message) []llm.Message {
	for i := 0; i < len(messages); i++ {
		if messages[i].Role != llm.RoleAssistant || len(messages[i].ToolCalls) == 0 {
			continue
		}
		// 找到其后连续的 tool 结果段 [i+1, j)
		j := i + 1
		for j < len(messages) && messages[j].Role == llm.RoleTool {
			j++
		}
		if j-i-1 > 1 {
			seg := messages[i+1 : j]
			sort.SliceStable(seg, func(a, b int) bool {
				return seg[a].ToolCallIndex < seg[b].ToolCallIndex
			})
		}
		i = j - 1
	}
	return messages
}

// planningTurnToMessage 把规划期的一次 LLM 调用还原成 assistant 消息。
//
// 与 functionalCallTurnToMessage 的唯一差别：过滤掉 submit_plan 协议工具调用。
//
// 为什么必须过滤：submit_plan 只在规划期注入。切到执行期后工具列表里没有它，
// 而历史里却带着「assistant 调用了 submit_plan」，Gemini / OpenAI 都会因
// 「functionCall 引用了未声明的工具」判 400。
// 规划结论通过 system prompt 旁路注入（见 renderPlanContext），不靠历史传递。
//
// 调研用的真实工具调用要保留 —— 它们后面跟着 tool 结果消息，
// 少了这条 assistant 消息，那些结果就成了孤立消息被丢弃。
func planningTurnToMessage(t *model.TaskTurn) (llm.Message, bool) {
	if t.Output == "" {
		return llm.Message{}, false
	}
	var out model.TurnLLMOutput
	if err := json.Unmarshal([]byte(t.Output), &out); err != nil {
		return llm.Message{}, false
	}

	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: out.Text,
	}
	for _, tc := range out.ToolCalls {
		if tc.Name == model.SubmitPlanToolName {
			// direct_reply 的答案只存在于 submit_plan 参数里 —— 这轮根本没有执行期，
			// 过滤掉就等于「用户问了、助手没答」，下一轮模型看不到自己说过什么，
			// 追问（「那第二点展开说说」）会接不上。这里把 reply 还原成普通
			// assistant 文本入历史（工具调用本身仍然剔除，理由同上）。
			if reply := directReplyFromArgs(tc.Arguments); reply != "" {
				msg.Content = joinNonEmpty(msg.Content, reply)
			}
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			Name:      tc.Name,
			Arguments: tc.Arguments,
			Signature: tc.Signature,
		})
	}

	// 纯 submit_plan 的那条 turn 过滤后什么都不剩，不入历史
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		return llm.Message{}, false
	}
	return msg, true
}

// directReplyFromArgs 从 submit_plan 参数里取 direct_reply 模式的回复正文；
// 其它模式或缺失时返回空串。
func directReplyFromArgs(args map[string]interface{}) string {
	if mode, _ := args["mode"].(string); mode != model.PlanModeDirectReply {
		return ""
	}
	reply, _ := args["reply"].(string)
	return strings.TrimSpace(reply)
}

// joinNonEmpty 用空行拼接两段文本，任一为空时直接返回另一段。
func joinNonEmpty(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}

// functionalCallTurnToMessage 把一次 LLM 调用的 turn 还原成 assistant 消息
func functionalCallTurnToMessage(t *model.TaskTurn) (llm.Message, bool) {
	if t.Output == "" {
		return llm.Message{}, false
	}
	var out model.TurnLLMOutput
	if err := json.Unmarshal([]byte(t.Output), &out); err != nil {
		return llm.Message{}, false
	}

	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: out.Text,
	}
	for _, tc := range out.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			Name:      tc.Name,
			Arguments: tc.Arguments,
			// 原样带回签名。Gemini 3 校验当前 turn 内每个 step 的首个
			// functionCall 必须有签名，缺失会 400。
			Signature: tc.Signature,
		})
	}

	// 既无文本也无工具调用的空消息不入历史
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		return llm.Message{}, false
	}
	return msg, true
}

// toolCallTurnToMessage 把一次工具调用的 turn 还原成 tool 结果消息。
//
// 失败的 turn 也会生成消息，内容为错误说明，让 LLM 知道工具为何没成功。
// 回喂前对超长结果做截断，避免 fetch/读文件等大结果撑爆上下文。
func toolCallTurnToMessage(t *model.TaskTurn) (llm.Message, bool) {
	// 工具名优先取冗余列，回退到 input 快照；同时取声明序号 index。
	toolName := t.ToolName
	index := 0
	if t.Input != "" {
		var in model.TurnToolInput
		if err := json.Unmarshal([]byte(t.Input), &in); err == nil {
			if toolName == "" {
				toolName = in.ToolName
			}
			index = in.Index
		}
	}
	if toolName == "" {
		return llm.Message{}, false
	}

	content := ""
	if t.Output != "" {
		var out model.TurnToolOutput
		if err := json.Unmarshal([]byte(t.Output), &out); err == nil {
			content = out.Content
		}
	}

	// 失败的 turn：output 里没有说明时用 error_msg 兜底，
	// 并显式标注为错误，避免模型把错误信息当成正常结果。
	if t.Status == model.TurnStatusFailed {
		if strings.TrimSpace(content) == "" {
			content = t.ErrorMsg
		}
		if strings.TrimSpace(content) == "" {
			content = "工具执行失败，未提供错误详情"
		}
		content = "[工具执行失败] " + content
	} else if strings.TrimSpace(content) == "" {
		content = "(工具无返回内容)"
	}

	return llm.Message{
		Role:          llm.RoleTool,
		ToolName:      toolName,
		Content:       truncateToolResult(content),
		ToolCallIndex: index,
	}, true
}

// maxToolResultChars 回喂 LLM 的单条工具结果最大字符数。
// 超长时保留头尾各一半 + 中间省略标记。
const maxToolResultChars = 4000

// truncateToolResult 按 rune 截断超长工具结果，保留头尾。
// 只影响回喂 LLM 的内容；turn 里存的仍是完整结果，调试页可看全量。
func truncateToolResult(s string) string {
	runes := []rune(s)
	if len(runes) <= maxToolResultChars {
		return s
	}
	keep := maxToolResultChars / 2
	head := string(runes[:keep])
	tail := string(runes[len(runes)-keep:])
	return fmt.Sprintf("%s\n…(中间 %d 字符已截断)…\n%s", head, len(runes)-2*keep, tail)
}
