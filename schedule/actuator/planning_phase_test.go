package actuator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// ============================================================
// splitPlanningToolCalls —— 规划期的三路分流
// ============================================================

func TestSplitPlanningToolCalls(t *testing.T) {
	cases := []struct {
		name      string
		calls     []llm.ToolCall
		wantPlan  bool
		wantReals []string
	}{
		{"empty", nil, false, nil},
		{
			"only submit_plan",
			[]llm.ToolCall{{Name: model.SubmitPlanToolName}},
			true, nil,
		},
		{
			"only research tools",
			[]llm.ToolCall{{Name: "search"}, {Name: "read_file"}},
			false, []string{"search", "read_file"},
		},
		{
			// 模型同时提交与调研：两者都要能被识别出来，
			// 调用方才能决定「以提交为准 + 剔除未执行的调用」
			"mixed",
			[]llm.ToolCall{{Name: "search"}, {Name: model.SubmitPlanToolName}, {Name: "fetch"}},
			true, []string{"search", "fetch"},
		},
		{
			// 重复提交属协议异常，取首个即可，不应报错或漏判
			"duplicated submit_plan",
			[]llm.ToolCall{
				{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{"mode": "workflow"}},
				{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{"mode": "single_agent"}},
			},
			true, nil,
		},
	}

	for _, c := range cases {
		planCall, reals := splitPlanningToolCalls(c.calls)
		if (planCall != nil) != c.wantPlan {
			t.Errorf("%s: planCall present = %v, want %v", c.name, planCall != nil, c.wantPlan)
		}
		if len(reals) != len(c.wantReals) {
			t.Errorf("%s: real calls = %v, want %v", c.name, callNames(reals), c.wantReals)
			continue
		}
		for i, want := range c.wantReals {
			if reals[i].Name != want {
				t.Errorf("%s: real call[%d] = %q, want %q", c.name, i, reals[i].Name, want)
			}
		}
	}
}

// TestSplitPlanningToolCalls_DuplicateTakesFirst 重复提交时以首个为准
func TestSplitPlanningToolCalls_DuplicateTakesFirst(t *testing.T) {
	planCall, _ := splitPlanningToolCalls([]llm.ToolCall{
		{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{"mode": "workflow"}},
		{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{"mode": "single_agent"}},
	})
	if planCall == nil {
		t.Fatal("want a plan call")
	}
	if got := planCall.Arguments["mode"]; got != "workflow" {
		t.Errorf("want first submit_plan (workflow), got %v", got)
	}
}

// ============================================================
// withOnlyPlanCall —— 剔除未执行的工具调用
// ============================================================

// TestWithOnlyPlanCall 提交规划时，那些不会被执行的工具调用必须从 turn 快照剔除。
// 留着会在历史重建时变成「请求了工具却无响应」的孤立 assistant 消息，provider 判 400。
func TestWithOnlyPlanCall(t *testing.T) {
	planCall := llm.ToolCall{
		Name:      model.SubmitPlanToolName,
		Arguments: map[string]interface{}{"mode": "single_agent"},
	}
	orig := &llm.ChatResult{
		Text: "计划如下",
		ToolCalls: []llm.ToolCall{
			{Name: "search"},
			planCall,
			{Name: "fetch"},
		},
	}

	trimmed := withOnlyPlanCall(orig, planCall)
	if len(trimmed.ToolCalls) != 1 || trimmed.ToolCalls[0].Name != model.SubmitPlanToolName {
		t.Errorf("want only submit_plan kept, got %v", callNames(trimmed.ToolCalls))
	}
	if trimmed.Text != orig.Text {
		t.Errorf("text should be preserved, got %q", trimmed.Text)
	}
	// 不能就地改原对象 —— 调用方后续还要按原始返回打日志
	if len(orig.ToolCalls) != 3 {
		t.Errorf("original result must not be mutated, got %v", callNames(orig.ToolCalls))
	}
}

// ============================================================
// decodeAndValidatePlan —— 规划参数解析与校验
// ============================================================

// noWorkflows 没有任何历史工作流的上下文 —— 等价于旧版的「空 DAG 首轮规划」。
func noWorkflows() *workflowContext { return &workflowContext{} }

// appendableWF 构造一条「可追加」的候选工作流（所有节点都是可依赖锚点）。
func appendableWF(id string, status model.TaskStatus, nodes ...model.WorkflowNodeData) *workflowCandidate {
	dag := &model.WorkflowOutputData{Nodes: nodes}
	return &workflowCandidate{
		TaskID: id,
		Title:  id,
		Status: status,
		DAG:    dag,
		Policy: dag.EvaluateAppendPolicy(status),
	}
}

func TestDecodeAndValidatePlan_SingleAgent(t *testing.T) {
	call := &llm.ToolCall{
		Name: model.SubmitPlanToolName,
		Arguments: map[string]interface{}{
			"mode":   "single_agent",
			"reason": "问题很简单",
		},
	}
	resp, _, err := decodeAndValidatePlan(call, noWorkflows(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Mode != "single_agent" || resp.Reason != "问题很简单" {
		t.Errorf("got %+v", resp)
	}
}

func TestDecodeAndValidatePlan_Rejects(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"nil args", nil},
		{"unknown mode", map[string]interface{}{"mode": "magic"}},
		// workflow 模式必须带节点，否则规划没有实际内容
		{"workflow without nodes", map[string]interface{}{"mode": "workflow", "reason": "复杂"}},
		{
			// 首轮至少 2 个节点才构成 DAG
			"first round with single node",
			map[string]interface{}{
				"mode":   "workflow",
				"reason": "复杂",
				"new_nodes": []interface{}{
					map[string]interface{}{
						"node_id": "n1", "name": "步骤一", "description": "d",
						"input": map[string]interface{}{"input_text": "x"}, "depends_on": []interface{}{},
					},
				},
			},
		},
	}

	for _, c := range cases {
		call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: c.args}
		if _, _, err := decodeAndValidatePlan(call, noWorkflows(), false); err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}

// TestValidatePlanningResp_IncrementalDeps 追加轮可以只加 1 个节点，
// 且 depends_on 允许引用已有 workflow 节点。
//
// 已有 DAG 必须满足「单一输出节点」约束（n2_analyze 是当前唯一叶子），
// 追加 n3_write 后 n3_write 成为新的唯一叶子，仍通过校验。
func TestValidatePlanningResp_IncrementalDeps(t *testing.T) {
	existing := &model.WorkflowOutputData{
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n1_search", Name: "检索"},
			{NodeID: "n2_analyze", Name: "分析", DependsOn: []string{"n1_search"}},
		},
	}
	resp := &planningLLMResp{
		Mode:   "workflow",
		Reason: "追加撰写步骤",
		NewNodes: []planningLLMNode{
			{NodeID: "n3_write", Name: "撰写", DependsOn: []string{"n2_analyze"}},
		},
	}
	if err := validatePlanningResp(resp, existing, nil); err != nil {
		t.Errorf("incremental single node should pass: %v", err)
	}

	// id 与已有节点冲突
	resp.NewNodes[0].NodeID = "n1_search"
	if err := validatePlanningResp(resp, existing, nil); err == nil {
		t.Error("conflicting node_id should be rejected")
	}

	// 依赖不存在的节点
	resp.NewNodes[0].NodeID = "n3_write"
	resp.NewNodes[0].DependsOn = []string{"n9_ghost"}
	if err := validatePlanningResp(resp, existing, nil); err == nil {
		t.Error("dependency on unknown node should be rejected")
	}

	// 自依赖
	resp.NewNodes[0].DependsOn = []string{"n3_write"}
	if err := validatePlanningResp(resp, existing, nil); err == nil {
		t.Error("self dependency should be rejected")
	}
}

// ============================================================
// 单一输出节点校验
// ============================================================

// workflowNodeArg 构造 submit_plan 里单个节点的参数。
func workflowNodeArg(id, name string, deps ...string) map[string]interface{} {
	depsArr := make([]interface{}, 0, len(deps))
	for _, d := range deps {
		depsArr = append(depsArr, d)
	}
	return map[string]interface{}{
		"node_id": id, "name": name, "description": "d",
		"input":      map[string]interface{}{"input_text": "x"},
		"depends_on": depsArr,
	}
}

func TestComputeSinkNodeIDs(t *testing.T) {
	existing := &model.WorkflowOutputData{
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n1"},
			{NodeID: "n2", DependsOn: []string{"n1"}},
		},
	}
	newNodes := []planningLLMNode{
		{NodeID: "n3", DependsOn: []string{"n2"}},
	}
	sinks := computeSinkNodeIDs(existing, newNodes)
	if len(sinks) != 1 || sinks[0] != "n3" {
		t.Errorf("want single sink [n3], got %v", sinks)
	}
}

// TestComputeSinkNodeIDs_AppendSkipsOldSink 续接场景：新节点跳过原输出节点、
// 直接挂到已完成上游时，不应把原输出节点算进叶子候选，否则会误判多输出节点。
//
// 对应真实 bad case（0e859c8f）：原工作流 n8→n9→n10（n10 是原输出），
// 追加 n11 依赖 n9（跳过 n10）。旧逻辑把 n10 和 n11 都算叶子 → 2 个被拒；
// 新逻辑只统计新增节点 {n11} → 1 个叶子通过。
func TestComputeSinkNodeIDs_AppendSkipsOldSink(t *testing.T) {
	existing := &model.WorkflowOutputData{
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n8", Status: model.TaskStatusCompleted},
			{NodeID: "n9", Status: model.TaskStatusCompleted, DependsOn: []string{"n8"}},
			{NodeID: "n10", Status: model.TaskStatusCompleted, DependsOn: []string{"n9"}},
		},
	}
	newNodes := []planningLLMNode{{NodeID: "n11", DependsOn: []string{"n9"}}}
	sinks := computeSinkNodeIDs(existing, newNodes)
	if len(sinks) != 1 || sinks[0] != "n11" {
		t.Errorf("append skipping old sink should yield single sink [n11], got %v", sinks)
	}
}

// TestValidatePlanningResp_AppendSkipsOldSink 完整校验链：续接时新节点跳过原
// 输出节点、依赖已完成上游，应通过输出节点校验（不再被判多输出节点）。
func TestValidatePlanningResp_AppendSkipsOldSink(t *testing.T) {
	existing := &model.WorkflowOutputData{
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n8", Name: "上游", Status: model.TaskStatusCompleted},
			{NodeID: "n9", Name: "中间", Status: model.TaskStatusCompleted, DependsOn: []string{"n8"}},
			{NodeID: "n10", Name: "原输出", Status: model.TaskStatusCompleted, DependsOn: []string{"n9"}},
		},
	}
	resp := &planningLLMResp{
		Mode:   "workflow",
		Reason: "看第一帧效果",
		NewNodes: []planningLLMNode{
			{NodeID: "n11", Name: "渲染动画第一帧", DependsOn: []string{"n9"}},
		},
	}
	if err := validatePlanningResp(resp, existing, nil); err != nil {
		t.Errorf("append skipping old sink should pass, got: %v", err)
	}
}

// TestValidatePlanningResp_AppendStillRejectsMultipleNewSinks 续接时若「本轮新增
// 节点」内部就有多个叶子（没汇聚），仍要拒绝 —— 收窄的是候选范围，不是放弃约束。
func TestValidatePlanningResp_AppendStillRejectsMultipleNewSinks(t *testing.T) {
	existing := &model.WorkflowOutputData{
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n1", Name: "上游", Status: model.TaskStatusCompleted},
		},
	}
	resp := &planningLLMResp{
		Mode:   "workflow",
		Reason: "并行追加",
		NewNodes: []planningLLMNode{
			{NodeID: "n2", Name: "分支A", DependsOn: []string{"n1"}},
			{NodeID: "n3", Name: "分支B", DependsOn: []string{"n1"}},
		},
	}
	if err := validatePlanningResp(resp, existing, nil); err == nil {
		t.Error("append with two new sinks (no converge) should be rejected")
	}
}

func TestValidatePlanningResp_RejectsMultipleOutputNodes(t *testing.T) {
	// 三个并行子步骤彼此独立、没有汇总节点 → 3 个输出节点，应被拒
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":   "workflow",
		"reason": "并行调研",
		"new_nodes": []interface{}{
			workflowNodeArg("n1", "A"),
			workflowNodeArg("n2", "B"),
			workflowNodeArg("n3", "C"),
		},
	}}
	_, _, err := decodeAndValidatePlan(call, noWorkflows(), false)
	if err == nil {
		t.Fatal("want output-node error, got nil")
	}
	var outErr *replanError
	if !errors.As(err, &outErr) {
		t.Fatalf("want *replanError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "n1") || !strings.Contains(err.Error(), "n2") {
		t.Errorf("error should list offending nodes, got: %v", err)
	}
}

func TestValidatePlanningResp_RejectsCycleNoOutputNode(t *testing.T) {
	// n1 依赖 n2、n2 依赖 n1 → 依赖环，没有任何输出节点
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":   "workflow",
		"reason": "错误依赖",
		"new_nodes": []interface{}{
			workflowNodeArg("n1", "A", "n2"),
			workflowNodeArg("n2", "B", "n1"),
		},
	}}
	_, _, err := decodeAndValidatePlan(call, noWorkflows(), false)
	if err == nil {
		t.Fatal("want output-node error for cycle, got nil")
	}
	var outErr *replanError
	if !errors.As(err, &outErr) {
		t.Fatalf("want *replanError, got %T: %v", err, err)
	}
}

func TestValidatePlanningResp_SingleOutputNode(t *testing.T) {
	// n4_summary 汇聚 n1/n2/n3 → 唯一输出节点，应通过
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":   "workflow",
		"reason": "并行调研后汇总",
		"new_nodes": []interface{}{
			workflowNodeArg("n1", "A"),
			workflowNodeArg("n2", "B"),
			workflowNodeArg("n3", "C"),
			workflowNodeArg("n4_summary", "汇总", "n1", "n2", "n3"),
		},
	}}
	if _, _, err := decodeAndValidatePlan(call, noWorkflows(), false); err != nil {
		t.Fatalf("converged plan should pass: %v", err)
	}
}

// ============================================================
// 重规划反馈
// ============================================================

func TestReplanFeedbackMessages(t *testing.T) {
	planCall := &llm.ToolCall{
		Name:      model.SubmitPlanToolName,
		Arguments: map[string]interface{}{"mode": "workflow"},
	}
	outErr := newOutputNodeError([]string{"n1", "n2"})
	// 反馈只生成消息对，拼接交给共用组装入口（它会一并做重新折叠）
	msgs := append([]llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		replanFeedbackMessages(planCall, outErr)...)

	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].ToolCalls) != 1 ||
		msgs[1].ToolCalls[0].Name != model.SubmitPlanToolName {
		t.Errorf("middle message should be assistant submit_plan, got %+v", msgs[1])
	}
	if msgs[2].Role != llm.RoleTool || msgs[2].ToolName != model.SubmitPlanToolName {
		t.Errorf("last message should be tool feedback, got %+v", msgs[2])
	}
	if !strings.Contains(msgs[2].Content, "n1") || !strings.Contains(msgs[2].Content, "n2") {
		t.Errorf("feedback should list offending nodes, got: %s", msgs[2].Content)
	}
}

// ============================================================
// fallbackPlan —— 降级
// ============================================================

// TestFallbackPlan 降级规划必须是合法的 single_agent，
// 否则 finishPlanning 会因校验不过而把任务判失败 —— 那就失去降级的意义了。
func TestFallbackPlan(t *testing.T) {
	fb := fallbackPlan("模型未提交结构化规划")
	if fb.Mode != "single_agent" {
		t.Errorf("fallback mode = %q, want single_agent", fb.Mode)
	}
	if fb.Reason == "" {
		t.Error("fallback should carry a reason for think_process")
	}
	if err := validatePlanningResp(fb, nil, nil); err != nil {
		t.Errorf("fallback plan must be valid: %v", err)
	}
}

// ============================================================
// renderPlanContext —— 执行期的规划上下文注入
// ============================================================

// TestRenderPlanContext_WorkflowReturnsEmpty workflow 模式的任务在规划收口时
// 就已结束（活移交给对话绑定的工作流任务），根本不会进执行期。
//
// 因此这里必须返回空串：万一状态异常真走到了执行期，注入计划步骤会让模型
// 把已移交出去的活再做一遍，用户会看到两份互相矛盾的回复。
func TestRenderPlanContext_WorkflowReturnsEmpty(t *testing.T) {
	planJSON := mustJSON(t, planningLLMResp{
		Mode:        model.PlanModeWorkflow,
		Reason:      "需要多步骤",
		PlanSummary: "先检索再归纳",
		NewNodes: []planningLLMNode{
			{
				NodeID: "n1", Name: "检索资料", Description: "搜集素材",
				Input: model.TaskInputData{InputText: "关键词 A"},
			},
			{NodeID: "n2", Name: "归纳", Description: "整理成文"},
		},
	})

	if got := renderPlanContext(planJSON); got != "" {
		t.Errorf("workflow plan must not be injected into executing phase, got:\n%s", got)
	}
}

func TestRenderPlanContext_SingleAgent(t *testing.T) {
	planJSON := mustJSON(t, planningLLMResp{Mode: model.PlanModeSingleAgent, Reason: "问题简单"})
	got := renderPlanContext(planJSON)
	if !strings.Contains(got, "单 Agent") || !strings.Contains(got, "问题简单") {
		t.Errorf("unexpected single_agent context:\n%s", got)
	}
}

// TestRenderPlanContext_Empty 无规划 / 脏数据时返回空串，
// 不能往 system prompt 里塞一个空的"执行计划"标题误导模型。
func TestRenderPlanContext_Empty(t *testing.T) {
	for _, in := range []string{"", "   ", "not json", `{}`, `{"mode":""}`} {
		if got := renderPlanContext(in); got != "" {
			t.Errorf("renderPlanContext(%q) = %q, want empty", in, got)
		}
	}
}

// ============================================================
// 混元 new_nodes 序列化为字符串 + depends_on 漏填 的容错
// ============================================================

// TestDecodeAndValidatePlan_NewNodesAsJSONString 复现混元（hy4-preview）真实输出：
// new_nodes 被序列化成一个 JSON 字符串（而非数组），且每个节点都漏填 depends_on，
// 依赖只写在 input_text 里。修复前这会直接 decode 失败降级 single_agent；
// 修复后应正确解析出 4 个节点，并推断出 n4_report 依赖 n1/n2/n3，从而通过
// 「单一输出节点」校验、以 workflow 模式继续。
func TestDecodeAndValidatePlan_NewNodesAsJSONString(t *testing.T) {
	nodesJSON := `[
		{"node_id":"n1_profile","name":"简历信息梳理与事实核查","description":"梳理候选人画像","input":{"input_text":"简历全文..."}},
		{"node_id":"n2_market","name":"目标岗位与市场要求对标调研","description":"调研岗位要求","input":{"input_text":"调研目标..."}},
		{"node_id":"n3_project","name":"项目与技术深度评估","description":"评估项目深度","input":{"input_text":"待评估内容..."}},
		{"node_id":"n4_report","name":"汇总输出优劣势分析报告","description":"汇总 n1_profile、n2_market、n3_project 三份结果","input":{"input_text":"输入：n1_profile 的输出、n2_market 的输出、n3_project 的输出"}}
	]`
	call := &llm.ToolCall{
		Name: model.SubmitPlanToolName,
		Arguments: map[string]interface{}{
			"mode":   "workflow",
			"reason": "多维度分析简历",
			// 关键：new_nodes 是字符串，不是数组
			"new_nodes": nodesJSON,
		},
	}
	resp, _, err := decodeAndValidatePlan(call, noWorkflows(), false)
	if err != nil {
		t.Fatalf("new_nodes as string should decode and validate: %v", err)
	}
	if resp.Mode != "workflow" || len(resp.NewNodes) != 4 {
		t.Fatalf("want 4 workflow nodes, got mode=%q nodes=%d", resp.Mode, len(resp.NewNodes))
	}

	var summary *planningLLMNode
	for i := range resp.NewNodes {
		if resp.NewNodes[i].NodeID == "n4_report" {
			summary = &resp.NewNodes[i]
		}
	}
	if summary == nil {
		t.Fatal("n4_report not found")
	}
	if len(summary.DependsOn) != 3 {
		t.Fatalf("n4_report should infer 3 deps from input_text, got %v", summary.DependsOn)
	}
	for _, want := range []string{"n1_profile", "n2_market", "n3_project"} {
		found := false
		for _, d := range summary.DependsOn {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("n4_report deps missing %s, got %v", want, summary.DependsOn)
		}
	}
}

// TestDecodeToolCallArgs_KnownContext 回归：模型填的 known_context 必须被解析出来。
//
// 根因回归：planningLLMResp 有自定义 UnmarshalJSON，rawResp 结构里漏了
// known_context 字段，导致模型填的 known_context 被静默丢弃 → DAG 无 plan_context
// → 执行期节点拿不到规划期已知信息。
func TestDecodeToolCallArgs_KnownContext(t *testing.T) {
	args := map[string]interface{}{
		"mode":          "workflow",
		"reason":        "写周报",
		"plan_summary":  "基于笔记写周报",
		"known_context": "已读取 4 篇笔记，路径在 docs/notes/ 下；本周完成订单导出模块。",
		"new_nodes": []map[string]interface{}{
			{"node_id": "n1", "name": "归纳", "description": "归纳", "input": map[string]interface{}{"input_text": "归纳"}, "depends_on": []string{}},
		},
	}
	resp, err := decodeToolCallArgs(args)
	if err != nil {
		t.Fatalf("decodeToolCallArgs: %v", err)
	}
	if resp.KnownContext == "" {
		t.Fatalf("known_context 被丢弃了")
	}
	if !strings.Contains(resp.KnownContext, "4 篇笔记") {
		t.Errorf("known_context 内容不对：%q", resp.KnownContext)
	}
}

// TestDecodeToolCallArgs_NodeContext 回归：节点填的 context 必须被解析出来。
func TestDecodeToolCallArgs_NodeContext(t *testing.T) {
	args := map[string]interface{}{
		"mode":   "workflow",
		"reason": "写周报",
		"new_nodes": []map[string]interface{}{
			{
				"node_id": "n1", "name": "归纳", "description": "归纳",
				"context":    "本节点要把 4 篇笔记归纳成结构化素材，不写最终周报。",
				"input":      map[string]interface{}{"input_text": "归纳"},
				"depends_on": []string{},
			},
		},
	}
	resp, err := decodeToolCallArgs(args)
	if err != nil {
		t.Fatalf("decodeToolCallArgs: %v", err)
	}
	if len(resp.NewNodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(resp.NewNodes))
	}
	if resp.NewNodes[0].Context == "" {
		t.Fatalf("节点 context 被丢弃了")
	}
	if !strings.Contains(resp.NewNodes[0].Context, "归纳成结构化素材") {
		t.Errorf("节点 context 内容不对：%q", resp.NewNodes[0].Context)
	}
}

// TestInferMissingDeps_KeepsExplicitDeps 显式声明的 depends_on 不能被推断逻辑改动。
func TestInferMissingDeps_KeepsExplicitDeps(t *testing.T) {
	resp := &planningLLMResp{
		Mode: "workflow",
		NewNodes: []planningLLMNode{
			{NodeID: "n1", Name: "A", Description: "d"},
			{NodeID: "n2", Name: "B", Description: "d", DependsOn: []string{"n1"}},
		},
	}
	inferMissingDeps(resp, nil, nil)
	if len(resp.NewNodes[1].DependsOn) != 1 || resp.NewNodes[1].DependsOn[0] != "n1" {
		t.Errorf("explicit deps should be preserved, got %v", resp.NewNodes[1].DependsOn)
	}
	// n1 描述里没有引用，不应被补任何依赖
	if len(resp.NewNodes[0].DependsOn) != 0 {
		t.Errorf("n1 should have no inferred deps, got %v", resp.NewNodes[0].DependsOn)
	}
}

// TestInferMissingDeps_SkipsReverseReference 反向引用过滤：上游节点文本里提到下游
// （"供 n2 使用"），而下游已显式依赖本节点时，不得反向把下游填进本节点的依赖——
// 否则会制造 n1 ⇄ n2 的环（线上 bug：黄金风衣企鹅 task 5acfbbdf 因此死锁）。
func TestInferMissingDeps_SkipsReverseReference(t *testing.T) {
	resp := &planningLLMResp{
		Mode: "workflow",
		NewNodes: []planningLLMNode{
			{NodeID: "n1", Name: "生成图片", Description: "生成角色主视图，供 n2 使用"},
			{NodeID: "n2", Name: "生成360", Description: "d", DependsOn: []string{"n1"}},
		},
	}
	inferMissingDeps(resp, nil, nil)
	if len(resp.NewNodes[0].DependsOn) != 0 {
		t.Fatalf("n1 不应被反向推断出依赖 n2（会成环），got %v", resp.NewNodes[0].DependsOn)
	}
	if len(resp.NewNodes[1].DependsOn) != 1 || resp.NewNodes[1].DependsOn[0] != "n1" {
		t.Fatalf("n2 显式依赖应保留，got %v", resp.NewNodes[1].DependsOn)
	}
}

// TestInferMissingDeps_StillInfersForward 正向引用（汇总节点提到上游）仍要推断。
func TestInferMissingDeps_StillInfersForward(t *testing.T) {
	resp := &planningLLMResp{
		Mode: "workflow",
		NewNodes: []planningLLMNode{
			{NodeID: "n1", Name: "检索", Description: "d"},
			{NodeID: "n2", Name: "分析", Description: "d", DependsOn: []string{"n1"}},
			{NodeID: "n3", Name: "汇总", Description: "汇总 n1 与 n2 的结果"},
		},
	}
	inferMissingDeps(resp, nil, nil)
	got := resp.NewNodes[2].DependsOn
	if len(got) != 2 {
		t.Fatalf("n3 应正向推断出依赖 n1、n2，got %v", got)
	}
}

// TestDetectPlanningCycle 环检测：n1⇄n2 双向依赖应报错，正常链应通过。
func TestDetectPlanningCycle(t *testing.T) {
	// 环
	if err := detectPlanningCycle(nil, []planningLLMNode{
		{NodeID: "n1", DependsOn: []string{"n2"}},
		{NodeID: "n2", DependsOn: []string{"n1"}},
	}); err == nil {
		t.Fatalf("n1⇄n2 环应被检测到")
	} else if !strings.Contains(err.Error(), "环") {
		t.Fatalf("错误信息应包含『环』, got %v", err)
	}
	// 三节点环
	if err := detectPlanningCycle(nil, []planningLLMNode{
		{NodeID: "n1", DependsOn: []string{"n3"}},
		{NodeID: "n2", DependsOn: []string{"n1"}},
		{NodeID: "n3", DependsOn: []string{"n2"}},
	}); err == nil {
		t.Fatalf("三节点环应被检测到")
	}
	// 正常链
	if err := detectPlanningCycle(nil, []planningLLMNode{
		{NodeID: "n1", DependsOn: nil},
		{NodeID: "n2", DependsOn: []string{"n1"}},
		{NodeID: "n3", DependsOn: []string{"n2"}},
	}); err != nil {
		t.Fatalf("正常链不应报错, got %v", err)
	}
	// 无依赖
	if err := detectPlanningCycle(nil, []planningLLMNode{
		{NodeID: "n1", DependsOn: nil},
	}); err != nil {
		t.Fatalf("单节点不应报错, got %v", err)
	}
}

// TestIsStandaloneIDToken 标识符 token 精确匹配：id 必须作为独立标识符出现，
// 不能是更长标识符的子串（n1 不应命中 n10 或 n1_profile）。
func TestIsStandaloneIDToken(t *testing.T) {
	cases := []struct {
		haystack, id string
		want         bool
	}{
		{"汇总 n1_profile、n2_market", "n1_profile", true},
		{"n1_profile 的输出", "n1_profile", true},
		{"基于 n1_profile 的结果", "n1_profile", true},
		{"abc n10 xyz", "n1", false}, // n1 不应命中 n10
		{"n1_foo", "n1", false},      // n1 不应命中 n1_foo
		{"x n1 y", "n1", true},       // 独立 token 命中
		{"n1n1", "n1", false},        // 连续拼接不命中
	}
	for _, c := range cases {
		if got := isStandaloneIDToken(c.haystack, c.id); got != c.want {
			t.Errorf("isStandaloneIDToken(%q, %q) = %v, want %v", c.haystack, c.id, got, c.want)
		}
	}
}

// ============================================================
// 多工作流：续接 / 新建 的决策与锚点约束
// ============================================================

// TestDecodeAndValidatePlan_AppendResolvesTarget 模型指定 append + workflow_id 时，
// 解析出的 target 必须指向那条工作流，并把它的 DAG 作为后续校验的已有节点。
func TestDecodeAndValidatePlan_AppendResolvesTarget(t *testing.T) {
	wfs := &workflowContext{
		ActiveID: "wf-1",
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusCompleted,
				model.WorkflowNodeData{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted},
			),
		},
	}
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":            "workflow",
		"reason":          "基于检索结果继续撰写",
		"workflow_action": "append",
		"workflow_id":     "wf-1",
		"new_nodes": []interface{}{
			workflowNodeArg("n2", "撰写", "n1"),
		},
	}}
	resp, target, err := decodeAndValidatePlan(call, wfs, false)
	if err != nil {
		t.Fatalf("append plan should pass: %v", err)
	}
	if !target.IsAppend() || target.TaskID != "wf-1" {
		t.Fatalf("want append target wf-1, got %+v", target)
	}
	if len(resp.NewNodes) != 1 || resp.NewNodes[0].DependsOn[0] != "n1" {
		t.Errorf("unexpected nodes: %+v", resp.NewNodes)
	}
}

// TestDecodeAndValidatePlan_RejectsDependencyOnDeadNode 需求 5 的核心：
// 失败节点及其后代不能作为依赖锚点，依赖它们会被判为可重规划错误（而非直接降级）。
func TestDecodeAndValidatePlan_RejectsDependencyOnDeadNode(t *testing.T) {
	wfs := &workflowContext{
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusFailed,
				model.WorkflowNodeData{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted},
				model.WorkflowNodeData{NodeID: "n2", Name: "分析", Status: model.TaskStatusFailed,
					DependsOn: []string{"n1"}},
				model.WorkflowNodeData{NodeID: "n3", Name: "汇总", Status: model.TaskStatusAbandoned,
					DependsOn: []string{"n2"}},
			),
		},
	}
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":            "workflow",
		"reason":          "补救",
		"workflow_action": "append",
		"workflow_id":     "wf-1",
		"new_nodes": []interface{}{
			workflowNodeArg("n4", "重新分析", "n2"), // n2 已失败 → 非法
		},
	}}
	_, _, err := decodeAndValidatePlan(call, wfs, false)
	if err == nil {
		t.Fatal("dependency on failed node must be rejected")
	}
	var re *replanError
	if !errors.As(err, &re) {
		t.Fatalf("want *replanError (replanable), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "n1") {
		t.Errorf("error should list usable anchors, got: %v", err)
	}
}

// TestDecodeAndValidatePlan_FailedWorkflowCanReuseUpstream 需求 5 的另一半：
// 失败工作流的「已完成上游节点」仍然可依赖，且追加后输出节点唯一性校验要通过
// （已归档的失败分支不得被算成叶子）。
func TestDecodeAndValidatePlan_FailedWorkflowCanReuseUpstream(t *testing.T) {
	wfs := &workflowContext{
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusFailed,
				model.WorkflowNodeData{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted},
				model.WorkflowNodeData{NodeID: "n2", Name: "分析", Status: model.TaskStatusFailed,
					DependsOn: []string{"n1"}},
				model.WorkflowNodeData{NodeID: "n3", Name: "汇总", Status: model.TaskStatusAbandoned,
					DependsOn: []string{"n2"}},
			),
		},
	}
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":            "workflow",
		"reason":          "基于已完成的检索重新规划",
		"workflow_action": "append",
		"workflow_id":     "wf-1",
		"new_nodes": []interface{}{
			workflowNodeArg("n4", "重新分析", "n1"),
		},
	}}
	_, target, err := decodeAndValidatePlan(call, wfs, false)
	if err != nil {
		t.Fatalf("reusing completed upstream of a failed workflow must pass: %v", err)
	}
	if !target.IsAppend() {
		t.Errorf("want append, got %+v", target)
	}
	// 锚点只应包含 n1
	if target.Policy.AnchorIDs["n2"] || target.Policy.AnchorIDs["n3"] {
		t.Errorf("failed branch must not be anchors: %+v", target.Policy.AnchorIDs)
	}
}

// TestDecodeAndValidatePlan_StoppedWorkflowFallsBackToNew 需求 4：
// 用户主动停止的工作流不可追加 —— 模型就算填了 append 也要回退成新建。
func TestDecodeAndValidatePlan_StoppedWorkflowFallsBackToNew(t *testing.T) {
	wfs := &workflowContext{
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusStopped,
				model.WorkflowNodeData{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted},
			),
		},
	}
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":            "workflow",
		"reason":          "继续",
		"workflow_action": "append",
		"workflow_id":     "wf-1",
		"new_nodes": []interface{}{
			workflowNodeArg("n9", "A"),
			workflowNodeArg("n10", "汇总", "n9"),
		},
	}}
	_, target, err := decodeAndValidatePlan(call, wfs, false)
	if err != nil {
		t.Fatalf("should fall back to new workflow: %v", err)
	}
	if target.IsAppend() {
		t.Fatalf("stopped workflow must not be appended, got %+v", target)
	}
	if target.Action != model.PlanActionNew {
		t.Errorf("want action=new, got %q", target.Action)
	}
}

// TestDecodeAndValidatePlan_ForceNew 用户显式要求新开工作流时，
// 即使模型给了 append 也必须按新建处理 —— 用户判断优先于模型判断。
func TestDecodeAndValidatePlan_ForceNew(t *testing.T) {
	wfs := &workflowContext{
		ActiveID: "wf-1",
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusCompleted,
				model.WorkflowNodeData{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted},
			),
		},
	}
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":            "workflow",
		"reason":          "新话题",
		"workflow_action": "append",
		"workflow_id":     "wf-1",
		"new_nodes": []interface{}{
			workflowNodeArg("n9", "A"),
			workflowNodeArg("n10", "汇总", "n9"),
		},
	}}
	_, target, err := decodeAndValidatePlan(call, wfs, true)
	if err != nil {
		t.Fatalf("force-new plan should pass: %v", err)
	}
	if target.IsAppend() || target.Action != model.PlanActionNew {
		t.Errorf("force_new must override model's append, got %+v", target)
	}
}

// TestDecodeAndValidatePlan_UnknownWorkflowIDFallsBackToNew 模型编造/引用了
// 清单外的 workflow_id：回退新建，而不是报错降级 single_agent ——
// 「选错续接对象」是小错，不该放大成整轮能力丢失。
func TestDecodeAndValidatePlan_UnknownWorkflowIDFallsBackToNew(t *testing.T) {
	wfs := &workflowContext{
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusCompleted,
				model.WorkflowNodeData{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted},
			),
		},
	}
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":            "workflow",
		"reason":          "新话题",
		"workflow_action": "append",
		"workflow_id":     "wf-ghost",
		"new_nodes": []interface{}{
			workflowNodeArg("n9", "A"),
			workflowNodeArg("n10", "汇总", "n9"),
		},
	}}
	_, target, err := decodeAndValidatePlan(call, wfs, false)
	if err != nil {
		t.Fatalf("unknown workflow_id should fall back to new: %v", err)
	}
	if target.IsAppend() {
		t.Errorf("want new, got %+v", target)
	}
}

// TestDecodeAndValidatePlan_NoActionDefaultsToAppend 模型没给 workflow_action
// （弱模型 / 老客户端）时，默认续接激活的那条，行为与 v1.3 保持一致。
func TestDecodeAndValidatePlan_NoActionDefaultsToAppend(t *testing.T) {
	wfs := &workflowContext{
		ActiveID: "wf-1",
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusCompleted,
				model.WorkflowNodeData{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted},
			),
		},
	}
	call := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":   "workflow",
		"reason": "继续",
		"new_nodes": []interface{}{
			workflowNodeArg("n2", "撰写", "n1"),
		},
	}}
	_, target, err := decodeAndValidatePlan(call, wfs, false)
	if err != nil {
		t.Fatalf("default append should pass: %v", err)
	}
	if !target.IsAppend() || target.TaskID != "wf-1" {
		t.Errorf("want default append to active wf-1, got %+v", target)
	}
}

// TestComputeSinkNodeIDs_IgnoresArchived 已有节点（含失败/已归档分支）不参与
// 叶子判定 —— 输出节点只从「本轮新增节点」里挑。
//
// 旧实现把已有节点也纳入叶子候选，于是「续接失败工作流」几乎必然被判成
// 多输出节点：失败节点下游那个卡住的节点在合并图里就是个没人依赖的叶子。
// 收窄为「只看新增节点」后，这类残留天然被排除在候选之外。
func TestComputeSinkNodeIDs_IgnoresArchived(t *testing.T) {
	existing := &model.WorkflowOutputData{
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n1", Status: model.TaskStatusCompleted},
			{NodeID: "n2", Status: model.TaskStatusFailed, DependsOn: []string{"n1"}},
			{NodeID: "n3", Status: model.TaskStatusAbandoned, Archived: true, DependsOn: []string{"n2"}},
			{NodeID: "n4", Status: model.TaskStatusAbandoned, Archived: true, DependsOn: []string{"n2"}},
		},
	}
	newNodes := []planningLLMNode{{NodeID: "n5", DependsOn: []string{"n1"}}}
	sinks := computeSinkNodeIDs(existing, newNodes)
	if len(sinks) != 1 || sinks[0] != "n5" {
		t.Errorf("want single sink [n5], got %v", sinks)
	}
}

// ------------------------------------------------------------
// known_context：统一已知信息出口（替代 research_summary/key_findings/source_files）
// ------------------------------------------------------------

// TestLegacyContextText 旧三字段拼成一段自由文本，兼容未升级的旧模型/旧数据。
func TestLegacyContextText(t *testing.T) {
	got := legacyContextText(&planningLLMResp{
		ResearchSummary: "竞品 A 使用 Eino v0.9.x，Apache-2.0 协议。",
		KeyFindings:     []string{"Eino 为 Apache-2.0", "B 竞品闭源"},
		SourceFiles:     []string{"docs/notes/a.md", " /abs/b.md "},
	})
	for _, want := range []string{
		"竞品 A 使用 Eino",
		"关键事实：",
		"- Eino 为 Apache-2.0",
		"读过/定位到的文件：",
		"- docs/notes/a.md",
		"- /abs/b.md", // 首尾空白被 trim
	} {
		if !strings.Contains(got, want) {
			t.Errorf("legacyContextText 缺少 %q：\n%s", want, got)
		}
	}

	// 只有 summary：不出现「关键事实」「文件」小节
	if got := legacyContextText(&planningLLMResp{ResearchSummary: "只有摘要"}); !strings.Contains(got, "只有摘要") ||
		strings.Contains(got, "关键事实") || strings.Contains(got, "文件") {
		t.Errorf("只有 summary 时不该有其它小节：%q", got)
	}

	// 全空：返回空串
	if got := legacyContextText(&planningLLMResp{}); got != "" {
		t.Errorf("全空应返回空串，got %q", got)
	}
}

// TestBuildPlanResearchContextKnownContext known_context 优先，直接进 Context 字段。
func TestBuildPlanResearchContextKnownContext(t *testing.T) {
	pc := buildPlanResearchContext(context.Background(),
		&model.TaskTemp{Task: model.Task{TaskID: "t1", ChatID: "c1"}},
		&planningLLMResp{KnownContext: "已查证：Eino 为 Apache-2.0，文件在 docs/notes/a.md"})
	if pc == nil {
		t.Fatal("known_context 非空却返回 nil")
	}
	if pc.Context != "已查证：Eino 为 Apache-2.0，文件在 docs/notes/a.md" {
		t.Errorf("Context = %q", pc.Context)
	}
	if pc.Source != model.PlanContextSourceModel {
		t.Errorf("Source = %q, want model", pc.Source)
	}
}

// TestBuildPlanResearchContextLegacyFallback known_context 空时，从旧三字段拼一份兼容。
func TestBuildPlanResearchContextLegacyFallback(t *testing.T) {
	pc := buildPlanResearchContext(context.Background(),
		&model.TaskTemp{Task: model.Task{TaskID: "t1", ChatID: "c1"}},
		&planningLLMResp{
			ResearchSummary: "摘要",
			KeyFindings:     []string{"事实1"},
			SourceFiles:     []string{"a.md"},
		})
	if pc == nil {
		t.Fatal("旧字段有值却返回 nil")
	}
	for _, want := range []string{"摘要", "事实1", "a.md"} {
		if !strings.Contains(pc.Context, want) {
			t.Errorf("Context 缺少 %q：%q", want, pc.Context)
		}
	}
	if pc.Source != model.PlanContextSourceModel {
		t.Errorf("Source = %q, want model", pc.Source)
	}
}

// TestBuildPlanResearchContextEmpty 模型没给任何上下文且无落盘产物 → 返回 nil。
func TestBuildPlanResearchContextEmpty(t *testing.T) {
	pc := buildPlanResearchContext(context.Background(),
		&model.TaskTemp{Task: model.Task{TaskID: "t1", ChatID: "c1"}},
		&planningLLMResp{})
	if pc != nil {
		t.Errorf("无任何上下文应返回 nil，got %+v", pc)
	}
}
