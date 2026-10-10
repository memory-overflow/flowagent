// Package actuator — 对话级工作流的绑定与增量扩展
//
// 语义（v1.4 起一个对话可以有多条工作流）：
//
//	无工作流        对话没有任何工作流任务（conversation.workflow_task_id 为空）
//	规划判定 new    新建一条工作流任务（task_type=workflow），并设为对话的激活工作流
//	规划判定 append 在指定的已有工作流上追加节点，并按其当前状态调整状态：
//	                 · 执行中   → 不改（新节点直接并入正在跑的 DAG）
//	                 · 其他     → 回到「等待执行」
//
// 每轮规划由模型决定「续接已有工作流」还是「新开一条」—— 同一个对话里
// 用户先后提两件不相干的事是完全正常的，硬塞进同一条 DAG 反而让依赖关系
// 变得莫名其妙。conversation.workflow_task_id 只记录「当前激活」的那一条，
// 前端默认展示它，用户可在工作流列表里切换查看其它条。
//
// 不可续接的情况（见 model.WorkflowOutputData.EvaluateAppendPolicy）：
//   - 用户主动停止的工作流 —— 停止说明规划本身有问题，无后续利用价值
//   - 失败工作流中「失败节点及其全部后代」—— 依赖永远无法满足，只能依赖
//     那些真正跑完的上游节点（尽量复用已有产出）
//
// 工作流任务与对话任务是两条独立的生命周期：
// 对话任务负责「规划 + 回复用户」，判定 workflow 后当轮即收口；
// 工作流任务负责「真正执行 DAG」，停在 StageWorkflowWait 等编排调度器接管
// （编排逻辑见 workflow_actuator.go）。
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// workflowPlanResult 一次工作流规划落库后的结果，用于生成给用户的回复
type workflowPlanResult struct {
	// TaskID 本次落在的工作流任务 ID
	TaskID string
	// Created 本次是否新建了工作流（false 表示在已有工作流上追加）
	Created bool
	// Title 该工作流的标题（新建时由模型给出）
	Title string
	// AddedNodes 本轮新增的节点
	AddedNodes []model.WorkflowNodeData
	// DAG 合并后的完整工作流
	DAG *model.WorkflowOutputData
	// PrunedNodes 本次因失败分支剪枝而归档的节点数（仅续接失败工作流时 > 0）
	PrunedNodes int
}

// ============================================================
// 读取对话的工作流集合
// ============================================================

// workflowCandidate 一条可被本轮规划续接的候选工作流
type workflowCandidate struct {
	// TaskID 工作流任务 ID
	TaskID string
	// Title 工作流短标题（缺失时回落到 plan_summary）
	Title string
	// Status 工作流任务当前状态
	Status model.TaskStatus
	// DAG 该工作流的节点集合（解析失败时是空 DAG，不影响判定）
	DAG *model.WorkflowOutputData
	// Policy 可追加性判定结果
	Policy model.WorkflowAppendPolicy
}

// workflowContext 规划期拿到的「对话已有工作流」全貌。
//
// v1.4 起一个对话可以有多条工作流，规划期既要能给模型看到全部候选
// （决定续接哪条），也要能定位「当前激活」的那条作为默认续接目标。
type workflowContext struct {
	// ActiveID 对话当前激活的工作流任务 ID（可能为空）
	ActiveID string
	// Candidates 全部工作流，按创建时间升序（与前端切换器的顺序一致）
	Candidates []*workflowCandidate
}

// ByID 按任务 ID 查找候选
func (c *workflowContext) ByID(taskID string) *workflowCandidate {
	if c == nil || strings.TrimSpace(taskID) == "" {
		return nil
	}
	for _, cand := range c.Candidates {
		if cand.TaskID == taskID {
			return cand
		}
	}
	return nil
}

// DefaultAppendTarget 选择默认续接目标：优先当前激活且可追加的那条，
// 否则取最近一条可追加的（用户最近创建的更符合「本轮话题」的直觉）。
//
// 返回 nil 表示没有任何可续接的工作流，本次只能新建。
func (c *workflowContext) DefaultAppendTarget() *workflowCandidate {
	if c == nil {
		return nil
	}
	if active := c.ByID(c.ActiveID); active != nil && active.Policy.Appendable {
		return active
	}
	for i := len(c.Candidates) - 1; i >= 0; i-- {
		if c.Candidates[i].Policy.Appendable {
			return c.Candidates[i]
		}
	}
	return nil
}

// loadWorkflowCandidates 读取一个对话下的全部工作流及其可追加性判定。
//
// 全集直接以 task 表（chat_id + task_type=workflow）为准，不再依赖
// conversation.workflow_task_id 指向单条 —— 后者降级为「激活态」标记。
//
// 脏数据（task_output 解不开 / 任务查不到）不阻断规划：解析失败按空 DAG
// 处理，只记日志。规划是增强项，不该因为一条坏记录让用户完全用不了。
func loadWorkflowCandidates(
	ctx context.Context, convDAO *dao.ConversationDAO, taskDAO *dao.TaskDAO, chatID string,
) (*workflowContext, error) {
	activeID, err := convDAO.GetActiveWorkflowTaskID(ctx, chatID)
	if err != nil {
		return nil, err
	}
	tasks, err := taskDAO.ListWorkflowTasksByChatID(ctx, chatID)
	if err != nil {
		return nil, err
	}

	out := &workflowContext{ActiveID: activeID}
	for _, t := range tasks {
		dag := parseWorkflowOutput(t.TaskOutput, chatID)
		out.Candidates = append(out.Candidates, &workflowCandidate{
			TaskID: t.TaskID,
			Title:  workflowTitle(t, dag),
			Status: t.Status,
			DAG:    dag,
			Policy: dag.EvaluateAppendPolicy(t.Status),
		})
	}
	return out, nil
}

// workflowTitle 取工作流标题：task.title 优先，回落 DAG 内的 title，再回落摘要截断。
func workflowTitle(t *model.Task, dag *model.WorkflowOutputData) string {
	if s := strings.TrimSpace(t.Title); s != "" {
		return s
	}
	if s := strings.TrimSpace(dag.Title); s != "" {
		return s
	}
	return truncateRunes(dag.PlanSummary, 20)
}

// ============================================================
// 本轮规划的目标工作流解析
// ============================================================

// planWorkflowTarget 本轮规划最终落在哪条工作流上。
//
// 解析发生在模型给出 submit_plan 之后、落库之前：模型给的 workflow_id
// 可能指向一条不可追加的工作流（用户已停止 / 无可用锚点），此时不能报错
// 降级 single_agent —— 那是把「选错续接对象」这种小错放大成整轮能力丢失。
// 统一回退成「新建工作流」，后续依赖校验会自然把指向旧节点的部分拒掉并重规划。
type planWorkflowTarget struct {
	// Action 最终动作：PlanActionNew / PlanActionAppend
	Action string
	// TaskID append 时的目标工作流任务 ID
	TaskID string
	// DAG append 时的已有节点集合；new 时为 nil
	DAG *model.WorkflowOutputData
	// Policy append 时的可追加性判定（含允许依赖的锚点集合）
	Policy model.WorkflowAppendPolicy
	// Candidate append 时的候选对象，落库后要更新它的激活态与标题
	Candidate *workflowCandidate
}

// IsAppend 本次是否续接到已有工作流
func (t *planWorkflowTarget) IsAppend() bool {
	return t != nil && t.Action == model.PlanActionAppend && t.TaskID != ""
}

// resolveWorkflowTarget 解析本轮规划的目标工作流。
//
// forceNew 来自用户显式要求「新开一个工作流」（对话任务 task_input 的标记），
// 此时无论模型给什么都不续接 —— 用户判断优先于模型判断。
func resolveWorkflowTarget(
	resp *planningLLMResp, wfs *workflowContext, forceNew bool,
) *planWorkflowTarget {
	newTarget := func() *planWorkflowTarget {
		return &planWorkflowTarget{Action: model.PlanActionNew}
	}
	if resp.Mode != model.PlanModeWorkflow {
		return nil
	}
	if forceNew {
		return newTarget()
	}

	if resp.WorkflowAction == model.PlanActionNew {
		return newTarget()
	}
	if resp.WorkflowAction == model.PlanActionAppend {
		cand := wfs.ByID(resp.WorkflowID)
		if cand == nil {
			log.Printf("[func-call-actuator] append target %q not found in chat workflows, fallback to new",
				resp.WorkflowID)
			return newTarget()
		}
		if !cand.Policy.Appendable {
			log.Printf("[func-call-actuator] append target %q not appendable (%s), fallback to new",
				resp.WorkflowID, cand.Policy.Reason)
			return newTarget()
		}
		return &planWorkflowTarget{
			Action:    model.PlanActionAppend,
			TaskID:    cand.TaskID,
			DAG:       cand.DAG,
			Policy:    cand.Policy,
			Candidate: cand,
		}
	}

	// 模型没给 action：按默认续接目标推断。
	// 老数据与弱模型都可能不带这个字段，兜底保证行为与 v1.3 一致（能续接就续接）。
	if cand := wfs.DefaultAppendTarget(); cand != nil {
		return &planWorkflowTarget{
			Action:    model.PlanActionAppend,
			TaskID:    cand.TaskID,
			DAG:       cand.DAG,
			Policy:    cand.Policy,
			Candidate: cand,
		}
	}
	return newTarget()
}

// ============================================================
// 规划期的已有工作流上下文渲染
// ============================================================

// workflowRenderLimits 上下文渲染的截断上限。
//
// 一个对话可能累积多条工作流、每条几十个节点，全量塞进 prompt 会把规划器
// 的上下文吃满。只渲染「可依赖」的节点（已废弃的分支对规划毫无价值），
// 并对节点数与描述长度设上限。
const (
	maxRenderWorkflows     = 6
	maxRenderNodesPerWF    = 40
	maxRenderNodeDescRunes = 160
)

// renderWorkflowContext 把对话已有的工作流渲染为规划期上下文。
//
// 关键：只把「可作为依赖锚点」的节点标为可依赖，并显式列出不可追加的原因。
// 模型照着这份清单填 depends_on，后端再按同一份锚点集合做硬校验 ——
// 前后口径一致，模型才不会反复踩到「依赖了废弃节点」这类错误。
func renderWorkflowContext(wfs *workflowContext, forceNew bool) string {
	var b strings.Builder
	if wfs == nil || len(wfs.Candidates) == 0 {
		b.WriteString("【对话已有工作流】(空 —— 本轮若判定 workflow 模式，将新建第 1 条工作流)")
		return b.String()
	}
	if forceNew {
		b.WriteString("【对话已有工作流】\n")
		b.WriteString("用户本轮明确要求「新开一个工作流」，因此本轮不可续接任何已有工作流。")
		b.WriteString("若判定为 workflow 模式，请填 workflow_action=\"new\" 并给出 workflow_title。\n")
		b.WriteString("已有工作流（仅供判断话题是否重复，不可作为依赖）：")
		for _, c := range wfs.Candidates {
			fmt.Fprintf(&b, "\n- id=%s | %s | %s", c.TaskID, c.Title, statusText(c.Status))
		}
		return b.String()
	}

	b.WriteString("【对话已有工作流】\n")
	b.WriteString("判断本轮任务是否属于其中某一条的延续：是则 workflow_action=\"append\" 并填 workflow_id；")
	b.WriteString("若本轮是新话题，则 workflow_action=\"new\" 并填 workflow_title。\n")

	shown := 0
	for _, c := range wfs.Candidates {
		if shown >= maxRenderWorkflows {
			fmt.Fprintf(&b, "\n（还有 %d 条更早的工作流未列出）", len(wfs.Candidates)-shown)
			break
		}
		shown++

		mark := ""
		if c.TaskID == wfs.ActiveID {
			mark = " [当前激活]"
		}
		fmt.Fprintf(&b, "\n## 工作流 %s | id=%s | %s%s\n", c.Title, c.TaskID, statusText(c.Status), mark)
		if c.DAG != nil && strings.TrimSpace(c.DAG.PlanSummary) != "" {
			b.WriteString("目标：" + truncateRunes(c.DAG.PlanSummary, 200) + "\n")
		}
		if !c.Policy.Appendable {
			fmt.Fprintf(&b, "可追加：否 —— %s\n", c.Policy.Reason)
			continue
		}
		fmt.Fprintf(&b, "可追加：是（可复用的已完成节点 %d 个）\n", c.Policy.ReusableCount)

		anchors := 0
		for _, n := range c.DAG.Nodes {
			if anchors >= maxRenderNodesPerWF {
				b.WriteString("- ...（更多可依赖节点已省略，可用 id 仍可参考已有命名规律）\n")
				break
			}
			if !c.Policy.AnchorIDs[n.NodeID] {
				continue
			}
			anchors++
			fmt.Fprintf(&b, "- 可依赖 id=%s | %s | %s\n  desc: %s\n",
				n.NodeID, n.Name, statusText(n.Status),
				truncateRunes(n.Description, maxRenderNodeDescRunes))
		}
		if anchors == 0 {
			b.WriteString("- （该工作流尚无已有节点）\n")
		}
	}
	return b.String()
}

// statusText 把状态枚举渲染成人能读的中文，供规划器判断复用价值
func statusText(s model.TaskStatus) string {
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
	default:
		return "未知"
	}
}

// parseWorkflowOutput 解析工作流任务的 task_output，失败按空 DAG 处理
func parseWorkflowOutput(raw, chatID string) *model.WorkflowOutputData {
	dag := &model.WorkflowOutputData{}
	if strings.TrimSpace(raw) == "" {
		return dag
	}
	if err := json.Unmarshal([]byte(raw), dag); err != nil {
		log.Printf("[func-call-actuator] chat %s workflow output parse err: %v (raw=%s)",
			chatID, err, truncate(raw, 200))
		return &model.WorkflowOutputData{}
	}
	return dag
}

// ============================================================
// 落库：新建或扩展工作流
// ============================================================

// applyWorkflowPlan 把本轮规划出的节点落到目标工作流上。
//
// 两条路径（由 resolveWorkflowTarget 决定）：
//   - 新建 → 建一条新的工作流任务（含首批节点）+ 设为对话激活工作流
//   - 续接 → 在已有 DAG 上追加节点，并按当前状态调整工作流状态
//
// 两条路径都要更新对话的激活工作流：用户刚提的需求对应的 DAG 变化，
// 应该立刻出现在他眼前，而不是停在之前那条上。
func applyWorkflowPlan(
	ctx context.Context,
	convDAO *dao.ConversationDAO, taskDAO *dao.TaskDAO,
	temp *model.TaskTemp,
	resp *planningLLMResp, target *planWorkflowTarget,
	planCtx *model.WorkflowPlanContext,
) (*workflowPlanResult, error) {
	newNodes := buildWorkflowNodes(resp.NewNodes, temp.TaskID)

	var (
		result *workflowPlanResult
		err    error
	)
	if target.IsAppend() {
		result, err = appendWorkflowNodes(ctx, taskDAO, temp, resp, target, newNodes, planCtx)
	} else {
		result, err = createWorkflowTask(ctx, taskDAO, temp, resp, newNodes, planCtx)
	}
	if err != nil {
		return nil, err
	}

	if err := convDAO.SetActiveWorkflowTask(ctx, temp.ChatID, result.TaskID); err != nil {
		// 激活态只是展示用，失败不影响已落库的 DAG，记日志即可
		log.Printf("[func-call-actuator][%s] set active workflow %s failed: %v",
			temp.TaskID, result.TaskID, err)
	}
	return result, nil
}

// mergePlanContext 合并新旧两份规划期上下文。
//
// 合并规则（与 PlanSummary 的「新覆盖旧」一致，但文件清单是累加的）：
//   - 两者都为空 → nil
//   - 新为空     → 保留旧（本轮没调研，旧结论仍然有效）
//   - 旧为空     → 用新
//   - 都有       → 结论取新（本轮结论面向当前需求），文件清单并集去重
//
// 文件清单不做「以新为准」：旧轮次落盘的文件不会因为新规划而消失，
// 丢掉它们会让节点失去一部分可读素材。
func mergePlanContext(old, fresh *model.WorkflowPlanContext) *model.WorkflowPlanContext {
	if fresh.IsEmpty() {
		return old
	}
	if old.IsEmpty() {
		return fresh
	}
	merged := *fresh
	merged.Files = mergeOutputFiles(old.Files, fresh.Files)
	return &merged
}

// buildWorkflowNodes 把 LLM 给的节点转成持久化结构。
// 节点状态一律从「排队中」起步，真正的执行状态由后续编排流程更新。
func buildWorkflowNodes(nodes []planningLLMNode, sourceTaskID string) []model.WorkflowNodeData {
	now := time.Now().UnixMilli()
	out := make([]model.WorkflowNodeData, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, model.WorkflowNodeData{
			NodeID:       n.NodeID,
			Name:         n.Name,
			Description:  n.Description,
			Input:        n.Input,
			Context:      n.Context,
			DependsOn:    append([]string(nil), n.DependsOn...),
			Status:       model.TaskStatusQueued,
			SourceTaskID: sourceTaskID,
			CreateTs:     now,
		})
	}
	return out
}

// fillAppendNodeContext 续接节点的上下文兜底。
//
// 续接轮的用户新输入不进工作流全局 TaskInput（那是创建轮的原文，见
// createWorkflowTask），必须靠节点级 context 承载。模型漏填节点 context 时
// 用本轮输入兜底，保证续接诉求不丢 —— 否则续接节点不知道用户这轮要它做什么。
func fillAppendNodeContext(newNodes []model.WorkflowNodeData, userInput string) []model.WorkflowNodeData {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return newNodes
	}
	for i := range newNodes {
		if strings.TrimSpace(newNodes[i].Context) == "" {
			newNodes[i].Context = userInput
		}
	}
	return newNodes
}

// createWorkflowTask 新建一条工作流任务。
//
// v1.4 起一个对话可以有多条工作流，因此不再需要 v1.3 那套「CAS 绑定」——
// 那时一个对话只能有一条，要靠「仅当未绑定时更新」防止并发建出两条。
// 现在新建永远成功，绑定退化为「设为激活态」，由调用方统一处理。
func createWorkflowTask(
	ctx context.Context,
	taskDAO *dao.TaskDAO,
	temp *model.TaskTemp,
	resp *planningLLMResp, newNodes []model.WorkflowNodeData,
	planCtx *model.WorkflowPlanContext,
) (*workflowPlanResult, error) {
	title := strings.TrimSpace(resp.WorkflowTitle)
	if title == "" {
		// 模型没给标题时回落摘要，保证切换器里这条工作流不是空白
		title = truncateRunes(resp.PlanSummary, 20)
	}
	dag := &model.WorkflowOutputData{
		Nodes:       newNodes,
		PlanSummary: resp.PlanSummary,
		Title:       title,
		// 规划期调研结论随 DAG 一次性落库：后续每个节点执行时都会注入，
		// 不必各自重搜一遍同样的内容。
		PlanContext: planCtx,
	}
	outJSON, err := json.Marshal(dag)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow output: %w", err)
	}

	workflowTaskID := uuid.New().String()
	now := time.Now()
	task := &model.Task{
		TaskID:   workflowTaskID,
		ChatID:   temp.ChatID,
		TaskType: model.TaskTypeWorkflow,
		Title:    title,
		// 工作流的输入 = 用户原始输入（纯文本，不含附件清单）。
		//
		// plan_summary 是模型对任务的概括，可能丢关键细节（用户提到的具体
		// 文件路径、文件名、格式要求等）—— 节点看不到主对话历史，若只拿
		// 概括就拿不回这些细节。因此这里存用户原文，与 DAG.PlanSummary 分开：
		// 原文保真（buildSubTaskPrompt 注入【用户原始输入】），概括锚定方向。
		// 附件清单由 loadChatInputFiles 单独渲染，这里不重复拼。
		TaskInput: model.JsonField[*model.TaskInputData]{
			Data: &model.TaskInputData{InputText: strings.TrimSpace(temp.TaskInput.Data.InputText)},
		},
		TaskOutput: string(outJSON),
		// 停在 workflow_wait 等编排调度器接管。绝不能用 functional_call_wait ——
		// 那会让工作流任务被对话任务的调度器捞去跑一遍单 Agent 循环。
		Stage:    int(model.StageWorkflowWait),
		Phase:    int(model.PhaseExecuting),
		Status:   model.TaskStatusQueued,
		Progress: 0,
		// TurnCount 归零：工作流任务的 turn 由编排流程自己记
		CreatedAt: now,
		UpdatedAt: now,
	}

	// CreateTask 双写 task + task_temp，必须在事务里 ——
	// 否则第二条插入失败会留下一个调度器永远看不到的孤立 task 行。
	//
	// 用新的 DAO 实例承载事务，不去改共享 taskDAO 的 Tx 字段：
	// 那个实例被多个 actuator goroutine 并发使用，写它就是数据竞争。
	if err := dao.Transaction(func(tx *gorm.DB) error {
		return (&dao.TaskDAO{Tx: tx}).CreateTask(ctx, task)
	}); err != nil {
		return nil, fmt.Errorf("create workflow task: %w", err)
	}

	log.Printf("[func-call-actuator][%s] created workflow task %s for chat %s (title=%q, %d nodes)",
		temp.TaskID, workflowTaskID, temp.ChatID, title, len(newNodes))
	return &workflowPlanResult{
		TaskID:     workflowTaskID,
		Created:    true,
		Title:      title,
		AddedNodes: newNodes,
		DAG:        dag,
	}, nil
}

// appendWorkflowNodes 在已有工作流上追加节点，并按规则调整工作流状态。
//
// 追加节点与工作流执行器（driveWorkflow 周期回写 DAG）并发写同一 task_output，
// 存在「读-改-写」丢失更新竞态。这里用 dag_version 乐观锁：冲突（版本已被并发
// 写入推进）时重读最新 DAG 重新合并追加，最多重试 maxAppendAttempts 次。
//
// 续接失败工作流时额外做一次剪枝（pruneAbandonedBranch）：失败节点及其后代
// 被打上归档标记，它们不再参与调度、成败判定与进度统计，只留在 DAG 里供追溯。
// 不剪枝的话，工作流重新排队后会因为「历史失败节点仍在」被立刻再次判失败，
// 而那些停在 queued 的后代又永远等不到依赖，整条工作流再也无法收敛。
func appendWorkflowNodes(
	ctx context.Context,
	taskDAO *dao.TaskDAO,
	temp *model.TaskTemp,
	resp *planningLLMResp,
	target *planWorkflowTarget,
	newNodes []model.WorkflowNodeData,
	planCtx *model.WorkflowPlanContext,
) (*workflowPlanResult, error) {
	const maxAppendAttempts = 3
	workflowTaskID := target.TaskID

	// 续接节点的上下文兜底：见 fillAppendNodeContext。
	newNodes = fillAppendNodeContext(newNodes, temp.TaskInput.Data.InputText)

	for attempt := 1; ; attempt++ {
		task, err := taskDAO.GetTaskByID(ctx, workflowTaskID)
		if err != nil {
			return nil, fmt.Errorf("get workflow task: %w", err)
		}
		if task == nil {
			return nil, fmt.Errorf("target workflow task %s not found", workflowTaskID)
		}

		// 以刚读到的任务为准重解 DAG。
		//
		// target.DAG 是规划开始时（LLM 调用之前）的快照，那期间可能过了几十秒，
		// 节点执行状态早已变化。直接拿旧快照写回去会把新状态覆盖成旧的。
		merged := parseWorkflowOutput(task.TaskOutput, temp.ChatID)

		// 剪枝必须在追加之前做：剪的是「历史失败分支」，与本次新节点无关，
		// 顺序反了会让新节点也被误判进废弃集合（若新节点依赖了已完成节点，
		// 而该节点恰好是某个失败节点的下游，就会出错）。
		pruned := pruneAbandonedBranch(merged)
		merged.Nodes = append(merged.Nodes, newNodes...)
		if resp.PlanSummary != "" {
			merged.PlanSummary = resp.PlanSummary
		}
		// 调研上下文按轮次合并：结论以最新一轮为准（本轮规划面向当前需求，
		// 与旧结论并列会让节点分不清哪些仍适用），但旧的文件清单要保留 ——
		// 那些文件仍在工作区里可读。
		merged.PlanContext = mergePlanContext(merged.PlanContext, planCtx)
		merged.Version = task.DAGVersion + 1

		outJSON, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("marshal merged workflow output: %w", err)
		}

		nextStatus, changed := model.NextWorkflowStatus(task.Status)
		// 状态需要回到「等待执行」时，调度阶段也要一并回退到 workflow_wait，
		// 否则状态是排队、阶段还停在 doing 或 finished，编排调度器扫不到它。
		nextStage := model.TaskStage(task.Stage)
		if changed {
			nextStage = model.StageWorkflowWait
		}

		err = taskDAO.UpdateWorkflowDAG(ctx, dao.UpsertWorkflowDAGParams{
			TaskID:        workflowTaskID,
			Output:        string(outJSON),
			Status:        nextStatus,
			Stage:         nextStage,
			ExpectVersion: task.DAGVersion,
		})
		if err == nil {
			log.Printf("[func-call-actuator][%s] appended %d node(s) to workflow %s "+
				"(total=%d, status %d → %d, changed=%v, pruned=%d, version=%d)",
				temp.TaskID, len(newNodes), workflowTaskID, len(merged.Nodes),
				task.Status, nextStatus, changed, pruned, merged.Version)
			return &workflowPlanResult{
				TaskID:      workflowTaskID,
				Created:     false,
				Title:       workflowTitle(task, merged),
				AddedNodes:  newNodes,
				DAG:         merged,
				PrunedNodes: pruned,
			}, nil
		}

		if !errors.Is(err, dao.ErrWorkflowVersionConflict) {
			return nil, err
		}
		if attempt >= maxAppendAttempts {
			return nil, fmt.Errorf("append workflow nodes: version conflict after %d attempts", attempt)
		}
		log.Printf("[func-call-actuator][%s] workflow %s version conflict (have %d), retry %d/%d",
			temp.TaskID, workflowTaskID, task.DAGVersion, attempt, maxAppendAttempts)
	}
}

// pruneAbandonedBranch 把「失败节点及其全部后代」标记为归档/废弃，返回归档数量。
//
// 为什么必须做：失败节点的下游依赖永远无法满足，它们卡在 queued 状态。
// 工作流被续接后重新排队，执行器会重新扫一遍全部节点 —— 历史失败节点会被
// 再次计入 anyFailed（工作流立刻被判失败），而那些卡住的后代又让
// 「全部节点完成」这个收敛条件永远不成立。剪掉它们，工作流才有机会跑完。
//
// 为什么保留在 DAG 里而不是删除：失败现场是排查依据，用户点开节点还能看到
// 当初为什么失败。归档标记让它们退出调度与统计，同时保留可追溯性 ——
// 这是「保留并灰显」而非「物理删除」的实现。
//
// 失败节点本身保留原 status（failed），只加归档标记；其未终态的后代
// 则置为 Abandoned，表示「从未执行、也不会再执行」。
func pruneAbandonedBranch(dag *model.WorkflowOutputData) int {
	if dag == nil || len(dag.Nodes) == 0 {
		return 0
	}

	children := make(map[string][]string, len(dag.Nodes))
	var roots []string
	for i := range dag.Nodes {
		n := &dag.Nodes[i]
		for _, dep := range n.DependsOn {
			children[dep] = append(children[dep], n.NodeID)
		}
		if n.Archived {
			continue
		}
		if n.Status == model.TaskStatusFailed || n.Status == model.TaskStatusPartialFailed ||
			n.Status == model.TaskStatusStopped {
			roots = append(roots, n.NodeID)
		}
	}
	if len(roots) == 0 {
		return 0
	}

	// BFS 从失败节点出发标记整个下游子树
	dead := make(map[string]bool, len(dag.Nodes))
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if dead[cur] {
			continue
		}
		dead[cur] = true
		queue = append(queue, children[cur]...)
	}

	pruned := 0
	for i := range dag.Nodes {
		n := &dag.Nodes[i]
		if !dead[n.NodeID] {
			continue
		}
		if n.Archived {
			continue // 上一轮已归档，幂等
		}
		n.Archived = true
		if n.Status == model.TaskStatusFailed || n.Status == model.TaskStatusPartialFailed {
			n.ArchiveReason = "该节点执行失败，其所在分支已废弃，不再执行"
		} else if n.Status == model.TaskStatusStopped {
			n.ArchiveReason = "该节点已被停止，其所在分支已废弃，不再执行"
		} else {
			// 排队中/执行中的下游：上游已失败，它永远等不到依赖
			n.Status = model.TaskStatusAbandoned
			n.ArchiveReason = "上游节点失败，该分支的依赖无法满足，已废弃"
		}
		pruned++
	}
	return pruned
}

// ============================================================
// 给用户的执行流程概述
// ============================================================

// renderWorkflowReply 生成对话任务的回复：一段大致的执行流程说明。
//
// 判定为 workflow 后，对话任务本身不再干活（活交给工作流任务），
// 但不能什么都不回 —— 用户发了消息就该得到反馈。这里把「我准备怎么做」
// 讲清楚：判定理由 + 本轮新增的步骤 + 依赖关系。
//
// 输出 markdown：前端的回复区本来就按 markdown 渲染。
func renderWorkflowReply(resp *planningLLMResp, result *workflowPlanResult) string {
	var b strings.Builder

	if result.Created {
		if result.Title != "" {
			fmt.Fprintf(&b, "已为本次对话创建新工作流 **%s**，并规划出以下执行流程：\n\n", result.Title)
		} else {
			b.WriteString("已为本次对话**创建工作流**，并规划出以下执行流程：\n\n")
		}
	} else {
		fmt.Fprintf(&b, "已在工作流 **%s** 上**追加**以下执行步骤：\n\n", result.Title)
	}

	if s := strings.TrimSpace(resp.PlanSummary); s != "" {
		b.WriteString("**整体思路**：" + s + "\n\n")
	} else if s := strings.TrimSpace(resp.Reason); s != "" {
		b.WriteString("**判定理由**：" + s + "\n\n")
	}

	// 节点名映射，让 depends_on 显示成人能读的名字而不是 id
	nameOf := make(map[string]string, len(result.DAG.Nodes))
	for _, n := range result.DAG.Nodes {
		nameOf[n.NodeID] = n.Name
	}

	b.WriteString("**本轮新增步骤**\n\n")
	for i, n := range result.AddedNodes {
		fmt.Fprintf(&b, "%d. **%s**", i+1, n.Name)
		if desc := strings.TrimSpace(n.Description); desc != "" {
			b.WriteString("：" + truncateRunes(desc, 200))
		}
		b.WriteString("\n")
		if deps := renderDepNames(n.DependsOn, nameOf); deps != "" {
			fmt.Fprintf(&b, "   - 依赖：%s\n", deps)
		}
	}

	if !result.Created && result.DAG != nil {
		total := result.DAG.ActiveNodeCount()
		if total > len(result.AddedNodes) {
			fmt.Fprintf(&b, "\n当前工作流共 %d 个步骤（含此前已规划的 %d 个）。",
				total, total-len(result.AddedNodes))
		}
		if result.PrunedNodes > 0 {
			fmt.Fprintf(&b, "\n另有 %d 个失败分支上的步骤已废弃（图中灰显），不再执行。",
				result.PrunedNodes)
		}
	}
	b.WriteString("\n\n可点击回复下方的「查看工作流」直达该工作流的流程图，点击任一节点可看该步骤的详情。")
	return b.String()
}

// renderDepNames 把依赖的 node_id 列表渲染成「名称(id)」形式
func renderDepNames(deps []string, nameOf map[string]string) string {
	if len(deps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(deps))
	for _, d := range deps {
		if name, ok := nameOf[d]; ok && name != "" {
			parts = append(parts, fmt.Sprintf("%s（%s）", name, d))
			continue
		}
		parts = append(parts, d)
	}
	return strings.Join(parts, "、")
}
