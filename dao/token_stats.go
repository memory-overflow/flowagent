package dao

import (
	"context"
	"fmt"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// 任务树的 token 归集
// ============================================================
//
// 为什么需要单独一个文件：任务的 token 消耗不只在它自己的 turn 上。
//
//	对话任务（父）
//	  ├─ 自身的 functional_call turn    → task_turn.task_id = 父任务
//	  └─ deep_search(...)                → 新建子任务（独立 task_id）
//	      └─ 子任务自己的 functional_call / tool_call turn
//	         → task_turn.task_id = 子任务  ← 按父任务 task_id 聚合时**完全看不到**
//
// deep_search 恰恰是消耗大户：它内部是多轮「web_search 定位 → fetch 精读」，
// 每轮都把整页正文塞进上下文。只按父任务聚合会严重低估真实消耗，
// 也会让「上下文水位」这类判断失去依据 —— 而它正是上下文管理的输入。
//
// 因此这里把「任务树」作为统计单位：父任务的消耗 = 自身 turn + 所有后代子任务 turn。

// maxSubTaskDepth 子任务树的展开深度上限。
//
// 实际业务只有两层（对话 → deep_search 子任务；工作流 → 节点子任务），
// deep_search 子任务里的工具集已剔除 deep_search 本身（防递归嵌套）。
// 这里仍按 BFS 展开并设置上限，是为了在数据异常形成环时不会无限循环。
const maxSubTaskDepth = 3

// TaskTokenBreakdown 任务树的 token 消耗拆解
type TaskTokenBreakdown struct {
	// OwnTokens 本任务自身 turn 的消耗
	OwnTokens int64
	// SubTaskTokens 所有后代子任务的消耗之和
	SubTaskTokens int64
	// TotalTokens OwnTokens + SubTaskTokens，即「完成这件事总共烧了多少」
	TotalTokens int64
	// DeepSearchTokens 其中由 deep_search 子任务消耗的部分。
	// 是 SubTaskTokens 的子集 —— 单独拆出来是因为它通常是绝对大头，
	// 混在总里看不出「钱烧在检索上还是烧在主循环上」。
	DeepSearchTokens int64
	// SubTaskCount 归集进来的子任务数量
	SubTaskCount int

	// PromptTokens 输入 token 合计（含子任务）。上下文水位的度量对象。
	PromptTokens int64
	// CompletionTokens 输出 token 合计（含子任务）
	CompletionTokens int64
}

// SumTaskTokensTree 统计「任务树」的 token 消耗：本任务 + 所有后代子任务。
//
// 按层 BFS 展开，每层对每个子任务各做一次三列聚合。子任务数量级很小
// （一次 deep_search 一个；工作流节点通常十几个），N+1 查询可接受；
// 相比把消耗增量冗余到父任务行上，这样没有幂等与并发更新的风险 ——
// task_turn 始终是唯一真源。
//
// 单个子任务聚合失败不中断整体：跳过它继续，避免一个坏子任务让父任务
// 的统计整块消失（降级方向是少算，而不是报错）。
func SumTaskTokensTree(
	ctx context.Context, taskDAO *TaskDAO, turnDAO *TaskTurnDAO, rootTaskID string,
) (*TaskTokenBreakdown, error) {
	if taskDAO == nil || turnDAO == nil {
		return nil, fmt.Errorf("sum task tokens tree: dao not initialized")
	}
	if rootTaskID == "" {
		return nil, fmt.Errorf("sum task tokens tree: task_id is required")
	}

	b := &TaskTokenBreakdown{}

	own, err := turnDAO.SumTokenColumnsByTaskID(ctx, rootTaskID)
	if err != nil {
		return nil, fmt.Errorf("sum own tokens: %w", err)
	}
	b.OwnTokens = own.Total
	b.PromptTokens = own.Prompt
	b.CompletionTokens = own.Completion

	level := []string{rootTaskID}
	for depth := 0; depth < maxSubTaskDepth && len(level) > 0; depth++ {
		var next []string
		for _, parentID := range level {
			subs, err := taskDAO.ListSubTasksByParent(ctx, parentID)
			if err != nil {
				// 降级：少算子任务这一层，父任务自身消耗仍然可用
				continue
			}
			for _, st := range subs {
				if st == nil {
					continue
				}
				sum, err := turnDAO.SumTokenColumnsByTaskID(ctx, st.TaskID)
				if err != nil {
					continue
				}
				b.SubTaskTokens += sum.Total
				b.PromptTokens += sum.Prompt
				b.CompletionTokens += sum.Completion
				b.SubTaskCount++
				if isDeepSearchSubTask(st) {
					b.DeepSearchTokens += sum.Total
				}
				next = append(next, st.TaskID)
			}
		}
		level = next
	}

	b.TotalTokens = b.OwnTokens + b.SubTaskTokens
	return b, nil
}

// isDeepSearchSubTask 判断任务是否为 deep_search 派生的检索子任务。
//
// 依据写在 TaskInput.Data.SubTaskKind 上（创建子任务时落的值）；
// 老数据没写该字段时返回 false，退化为「归入普通子任务」——
// 只是少了一个拆分维度，总量不受影响。
func isDeepSearchSubTask(t *model.Task) bool {
	if t == nil || t.TaskInput.Data == nil {
		return false
	}
	return t.TaskInput.Data.SubTaskKind == model.SubTaskKindDeepSearch
}

// ============================================================
// 对话级批量归集（高频路径专用）
// ============================================================

// ChatTokenIndex 一个对话内所有任务的 token 索引。
//
// 存在的意义是性能：对话详情接口被前端每 5s 轮询一次，且要渲染该对话下
// 的**每一个**任务。若对每个任务各调一次 SumTaskTokensTree，
// 查询数是 O(任务数 × (1 + 子任务数))，轮询压力会直接压到 DB 上。
//
// 这里改为一次 GROUP BY 取回整个对话的聚合结果，再在内存里做父子归并。
// 可行的前提是 task_turn 上同时冗余了 chat_id 与 parent_task_id：
// deep_search 子任务建在父任务的 chat 下（ChatID 继承自 parent），
// 它的 turn 自然也带着同一个 chat_id，一条 SQL 就能全覆盖。
type ChatTokenIndex struct {
	// own task_id → 该任务自身 turn 的消耗
	own map[string]TokenSums
	// sub parent_task_id → 其所有子任务 turn 的消耗合计
	sub map[string]TokenSums
}

// NewChatTokenIndex 用现成的聚合结果直接构造索引。
//
// 正常路径请用 BuildChatTokenIndex；这里是为「索引已在别处算好」的场景
// 留的入口（例如单测里喂一组固定数据验证上层的归集逻辑）。
// own / sub 允许为 nil，取用时按零值处理。
func NewChatTokenIndex(own, sub map[string]TokenSums) *ChatTokenIndex {
	if own == nil {
		own = make(map[string]TokenSums)
	}
	if sub == nil {
		sub = make(map[string]TokenSums)
	}
	return &ChatTokenIndex{own: own, sub: sub}
}

// Own 返回任务自身的消耗（不含子任务）
func (idx *ChatTokenIndex) Own(taskID string) TokenSums {
	if idx == nil {
		return TokenSums{}
	}
	return idx.own[taskID]
}

// Sub 返回该任务所有子任务的消耗合计
func (idx *ChatTokenIndex) Sub(taskID string) TokenSums {
	if idx == nil {
		return TokenSums{}
	}
	return idx.sub[taskID]
}

// Total 返回任务树的总消耗（自身 + 子任务）
func (idx *ChatTokenIndex) Total(taskID string) TokenSums {
	own, sub := idx.Own(taskID), idx.Sub(taskID)
	return TokenSums{
		Prompt:     own.Prompt + sub.Prompt,
		Completion: own.Completion + sub.Completion,
		Total:      own.Total + sub.Total,
	}
}

// BuildChatTokenIndex 一次查询建立整个对话的 token 索引。
//
// 只归并一层父子关系，这与实际的任务形态一致：对话任务 → deep_search 子任务，
// 而 deep_search 子任务内部的工具集已剔除 deep_search 本身（防递归嵌套），
// 不会再往下派生。工作流节点子任务同理挂在工作流任务下，也是一层。
//
// 失败时返回空索引而非错误：token 是展示性信息，聚合不出来只应让这几个
// 字段显示 0，不该把整个对话详情接口拖挂。
func BuildChatTokenIndex(ctx context.Context, chatID string) *ChatTokenIndex {
	idx := &ChatTokenIndex{
		own: make(map[string]TokenSums),
		sub: make(map[string]TokenSums),
	}
	if chatID == "" || DB() == nil {
		return idx
	}

	var rows []struct {
		TaskID       string
		ParentTaskID string
		Prompt       int64
		Completion   int64
		Total        int64
	}
	if err := DB().WithContext(ctx).
		Model(&model.TaskTurn{}).
		Where("chat_id = ?", chatID).
		Select(`task_id AS task_id,
			parent_task_id AS parent_task_id,
			COALESCE(SUM(prompt_tokens), 0) AS prompt,
			COALESCE(SUM(completion_tokens), 0) AS completion,
			COALESCE(SUM(total_tokens), 0) AS total`).
		Group("task_id, parent_task_id").
		Scan(&rows).Error; err != nil {
		// 降级为空索引，调用方展示 0
		return idx
	}

	for _, r := range rows {
		sums := TokenSums{Prompt: r.Prompt, Completion: r.Completion, Total: r.Total}
		// 自身消耗：同一 task_id 可能因 parent_task_id 分组出现多行
		// （理论上不会，但存量数据里该列可能有空/非空混用），累加而非覆盖。
		cur := idx.own[r.TaskID]
		idx.own[r.TaskID] = TokenSums{
			Prompt:     cur.Prompt + sums.Prompt,
			Completion: cur.Completion + sums.Completion,
			Total:      cur.Total + sums.Total,
		}
		// 归到父任务名下
		if r.ParentTaskID != "" {
			p := idx.sub[r.ParentTaskID]
			idx.sub[r.ParentTaskID] = TokenSums{
				Prompt:     p.Prompt + sums.Prompt,
				Completion: p.Completion + sums.Completion,
				Total:      p.Total + sums.Total,
			}
		}
	}
	return idx
}
