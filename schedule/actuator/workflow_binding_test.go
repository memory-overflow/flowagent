package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// buildWorkflowNodes
// ============================================================

// TestBuildWorkflowNodes 新节点一律从「排队中」起步，并记住是哪轮对话提出的。
func TestBuildWorkflowNodes(t *testing.T) {
	nodes := buildWorkflowNodes([]planningLLMNode{
		{
			NodeID: "n1_search", Name: "检索", Description: "搜集素材",
			Input:     model.TaskInputData{InputText: "关键词"},
			DependsOn: []string{},
		},
		{NodeID: "n2_write", Name: "撰写", DependsOn: []string{"n1_search"}},
	}, "task-abc")

	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	for _, n := range nodes {
		if n.Status != model.TaskStatusQueued {
			t.Errorf("node %s status = %d, want queued", n.NodeID, n.Status)
		}
		if n.SourceTaskID != "task-abc" {
			t.Errorf("node %s source_task_id = %q, want task-abc", n.NodeID, n.SourceTaskID)
		}
		if n.CreateTs == 0 {
			t.Errorf("node %s missing create_ts", n.NodeID)
		}
	}
	if got := nodes[1].DependsOn; len(got) != 1 || got[0] != "n1_search" {
		t.Errorf("depends_on = %v, want [n1_search]", got)
	}
}

// TestBuildWorkflowNodes_CopiesDependsOn depends_on 必须深拷贝。
// 复用 LLM 返回的底层数组，后续对 DAG 的修改会串改到规划快照上。
func TestBuildWorkflowNodes_CopiesDependsOn(t *testing.T) {
	deps := []string{"n1"}
	nodes := buildWorkflowNodes([]planningLLMNode{
		{NodeID: "n2", Name: "步骤", DependsOn: deps},
	}, "task-1")

	nodes[0].DependsOn[0] = "mutated"
	if deps[0] != "n1" {
		t.Errorf("source slice was mutated: %v", deps)
	}
}

// TestBuildWorkflowNodes_CopiesContext 节点级 context 必须从 LLM 返回值传到持久化结构。
func TestBuildWorkflowNodes_CopiesContext(t *testing.T) {
	nodes := buildWorkflowNodes([]planningLLMNode{
		{NodeID: "n1", Name: "补充", Context: "用户本轮新增诉求"},
	}, "task-1")
	if nodes[0].Context != "用户本轮新增诉求" {
		t.Errorf("Context = %q，未从 planningLLMNode 传过来", nodes[0].Context)
	}
}

// TestFillAppendNodeContext 续接节点 context 兜底：模型漏填时用本轮用户输入补。
func TestFillAppendNodeContext(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", Context: "模型已填的上下文"},
		{NodeID: "n2"},                 // 漏填
		{NodeID: "n3", Context: "   "}, // 空白也算漏填
	}
	got := fillAppendNodeContext(nodes, "  本轮用户新诉求：补充对比表格  ")

	if got[0].Context != "模型已填的上下文" {
		t.Errorf("已填的 context 不该被覆盖：%q", got[0].Context)
	}
	if got[1].Context != "本轮用户新诉求：补充对比表格" {
		t.Errorf("漏填的节点未兜底：%q", got[1].Context)
	}
	if got[2].Context != "本轮用户新诉求：补充对比表格" {
		t.Errorf("空白 context 未兜底：%q", got[2].Context)
	}

	// 用户输入为空：不改动
	empty := []model.WorkflowNodeData{{NodeID: "n1"}}
	if got := fillAppendNodeContext(empty, "  "); got[0].Context != "" {
		t.Errorf("用户输入为空时不该改动：%q", got[0].Context)
	}
}

// ============================================================
// parseWorkflowOutput
// ============================================================

// TestParseWorkflowOutput_DirtyData 脏数据不能让整个对话卡住，
// 一律降级成空 DAG 继续跑。
func TestParseWorkflowOutput_DirtyData(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", "[1,2,3]"} {
		dag := parseWorkflowOutput(raw, "chat-1")
		if dag == nil {
			t.Fatalf("parseWorkflowOutput(%q) returned nil", raw)
		}
		if len(dag.Nodes) != 0 {
			t.Errorf("parseWorkflowOutput(%q) nodes = %d, want 0", raw, len(dag.Nodes))
		}
	}

	dag := parseWorkflowOutput(`{"plan_summary":"S","nodes":[{"node_id":"n1","name":"步骤"}]}`, "chat-1")
	if dag.PlanSummary != "S" || len(dag.Nodes) != 1 {
		t.Errorf("valid DAG parsed wrong: %+v", dag)
	}
}

// ============================================================
// renderWorkflowReply
// ============================================================

// TestRenderWorkflowReply_Created 首次创建工作流时，回复要说明是「新建」，
// 并列出全部新增步骤 —— 这是用户本轮唯一能看到的反馈。
func TestRenderWorkflowReply_Created(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", Name: "检索资料", Description: "搜集素材"},
		{NodeID: "n2", Name: "撰写报告", Description: "整理成文", DependsOn: []string{"n1"}},
	}
	reply := renderWorkflowReply(
		&planningLLMResp{
			Mode:        model.PlanModeWorkflow,
			Reason:      "需要多步骤",
			PlanSummary: "先检索再撰写",
		},
		&workflowPlanResult{
			TaskID:     "wf-1",
			Created:    true,
			AddedNodes: nodes,
			DAG:        &model.WorkflowOutputData{Nodes: nodes, PlanSummary: "先检索再撰写"},
		},
	)

	for _, want := range []string{"创建", "先检索再撰写", "检索资料", "撰写报告", "搜集素材"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q, got:\n%s", want, reply)
		}
	}
	// 依赖要显示成人能读的名字，而不是裸 id
	if !strings.Contains(reply, "检索资料（n1）") {
		t.Errorf("dependency should be rendered with node name, got:\n%s", reply)
	}
}

// TestRenderWorkflowReply_Appended 追加轮要讲清「追加」而非「新建」，
// 并交代整个工作流现在有多少步。
func TestRenderWorkflowReply_Appended(t *testing.T) {
	added := []model.WorkflowNodeData{{NodeID: "n3", Name: "审校", DependsOn: []string{"n2"}}}
	all := []model.WorkflowNodeData{
		{NodeID: "n1", Name: "检索资料"},
		{NodeID: "n2", Name: "撰写报告"},
		added[0],
	}
	reply := renderWorkflowReply(
		&planningLLMResp{Mode: model.PlanModeWorkflow, Reason: "补一步审校"},
		&workflowPlanResult{
			TaskID:     "wf-1",
			Created:    false,
			AddedNodes: added,
			DAG:        &model.WorkflowOutputData{Nodes: all},
		},
	)

	if !strings.Contains(reply, "追加") {
		t.Errorf("reply should say appended, got:\n%s", reply)
	}
	if strings.Contains(reply, "创建") {
		t.Errorf("append reply must not claim creation, got:\n%s", reply)
	}
	// 3 个步骤，其中此前已有 2 个
	if !strings.Contains(reply, "共 3 个步骤") || !strings.Contains(reply, "已规划的 2 个") {
		t.Errorf("reply should summarize totals, got:\n%s", reply)
	}
	if !strings.Contains(reply, "撰写报告（n2）") {
		t.Errorf("dependency name missing, got:\n%s", reply)
	}
}

// TestRenderWorkflowReply_NoSummaryFallsBackToReason 没给摘要时用判定理由兜底，
// 不能让用户拿到一段没有任何缘由说明的步骤清单。
func TestRenderWorkflowReply_NoSummaryFallsBackToReason(t *testing.T) {
	nodes := []model.WorkflowNodeData{{NodeID: "n1", Name: "步骤一"}}
	reply := renderWorkflowReply(
		&planningLLMResp{Mode: model.PlanModeWorkflow, Reason: "任务较复杂"},
		&workflowPlanResult{
			Created:    true,
			AddedNodes: nodes,
			DAG:        &model.WorkflowOutputData{Nodes: nodes},
		},
	)
	if !strings.Contains(reply, "任务较复杂") {
		t.Errorf("reply should fall back to reason, got:\n%s", reply)
	}
}

// ============================================================
// renderDepNames
// ============================================================

func TestRenderDepNames(t *testing.T) {
	nameOf := map[string]string{"n1": "检索", "n2": ""}

	if got := renderDepNames(nil, nameOf); got != "" {
		t.Errorf("no deps → got %q, want empty", got)
	}
	if got := renderDepNames([]string{"n1"}, nameOf); got != "检索（n1）" {
		t.Errorf("got %q, want 检索（n1）", got)
	}
	// 名字为空或 id 不认识时退回裸 id，不能输出「（n2）」这种残缺形式
	if got := renderDepNames([]string{"n2", "n9"}, nameOf); got != "n2、n9" {
		t.Errorf("got %q, want n2、n9", got)
	}
}

// ============================================================
// 多工作流：可追加性判定 / 剪枝 / 上下文渲染
// ============================================================

// TestEvaluateAppendPolicy_Stopped 需求 4：用户主动停止的工作流不可追加。
// 停止说明规划本身有问题，再在它上面续接只是把错误放大。
func TestEvaluateAppendPolicy_Stopped(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusCompleted},
	}}
	p := dag.EvaluateAppendPolicy(model.TaskStatusStopped)
	if p.Appendable {
		t.Fatalf("stopped workflow must not be appendable: %+v", p)
	}
	if p.Reason == "" {
		t.Error("reason is required for frontend display")
	}
}

// TestEvaluateAppendPolicy_FailedOnlyCompletedAnchors 需求 5：失败工作流仍可追加，
// 但锚点只能是「失败节点的已完成上游」，失败节点及其后代必须排除。
func TestEvaluateAppendPolicy_FailedOnlyCompletedAnchors(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusCompleted},
		{NodeID: "n2", Status: model.TaskStatusFailed, DependsOn: []string{"n1"}},
		{NodeID: "n3", Status: model.TaskStatusQueued, DependsOn: []string{"n2"}},
		{NodeID: "n4", Status: model.TaskStatusRunning, DependsOn: []string{"n1"}},
	}}
	p := dag.EvaluateAppendPolicy(model.TaskStatusFailed)
	if !p.Appendable {
		t.Fatalf("failed workflow should still be appendable: %+v", p)
	}
	if !p.AnchorIDs["n1"] {
		t.Error("completed upstream must be an anchor")
	}
	if p.AnchorIDs["n2"] || p.AnchorIDs["n3"] {
		t.Errorf("failed node and its descendants must not be anchors: %+v", p.AnchorIDs)
	}
	// 失败工作流里「未完成」的节点也不可依赖：它们在收口时已被停止，没有产出
	if p.AnchorIDs["n4"] {
		t.Error("non-completed node of a failed workflow must not be an anchor")
	}
}

// TestEvaluateAppendPolicy_RunningAllLiveNodes 执行中的工作流可追加，
// 排队/执行中的节点也能作为锚点（本轮新节点可以挂在它们后面等它们跑完）。
func TestEvaluateAppendPolicy_RunningAllLiveNodes(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusCompleted},
		{NodeID: "n2", Status: model.TaskStatusRunning},
		{NodeID: "n3", Status: model.TaskStatusQueued},
	}}
	p := dag.EvaluateAppendPolicy(model.TaskStatusRunning)
	if !p.Appendable {
		t.Fatalf("running workflow should be appendable: %+v", p)
	}
	for _, id := range []string{"n1", "n2", "n3"} {
		if !p.AnchorIDs[id] {
			t.Errorf("%s should be an anchor", id)
		}
	}
	if p.ReusableCount != 1 {
		t.Errorf("reusable = %d, want 1", p.ReusableCount)
	}
}

// TestEvaluateAppendPolicy_NothingReusable 有历史节点但一个都复用不了时不可追加，
// 引导模型新开工作流而不是硬续接。
func TestEvaluateAppendPolicy_NothingReusable(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusFailed},
		{NodeID: "n2", Status: model.TaskStatusQueued, DependsOn: []string{"n1"}},
	}}
	p := dag.EvaluateAppendPolicy(model.TaskStatusFailed)
	if p.Appendable {
		t.Fatalf("no reusable node → must not be appendable: %+v", p)
	}
}

// TestPruneAbandonedBranch 失败节点保留原状态只加归档，卡在下游的节点置为
// abandoned —— 这样执行器排除归档节点后，工作流重新排队才有机会跑完。
func TestPruneAbandonedBranch(t *testing.T) {
	dag := &model.WorkflowOutputData{Nodes: []model.WorkflowNodeData{
		{NodeID: "n1", Status: model.TaskStatusCompleted},
		{NodeID: "n2", Status: model.TaskStatusFailed, DependsOn: []string{"n1"}},
		{NodeID: "n3", Status: model.TaskStatusQueued, DependsOn: []string{"n2"}},
		{NodeID: "n4", Status: model.TaskStatusQueued, DependsOn: []string{"n3"}},
		// 与失败分支无关的旁支，不该被误剪
		{NodeID: "n5", Status: model.TaskStatusCompleted, DependsOn: []string{"n1"}},
	}}

	n := pruneAbandonedBranch(dag)
	if n != 3 {
		t.Fatalf("pruned = %d, want 3 (n2,n3,n4)", n)
	}
	byID := map[string]*model.WorkflowNodeData{}
	for i := range dag.Nodes {
		byID[dag.Nodes[i].NodeID] = &dag.Nodes[i]
	}
	if !byID["n2"].Archived || byID["n2"].Status != model.TaskStatusFailed {
		t.Errorf("failed node: keep status + archived, got %+v", byID["n2"])
	}
	if byID["n3"].Status != model.TaskStatusAbandoned || !byID["n3"].Archived {
		t.Errorf("stuck descendant must be abandoned, got %+v", byID["n3"])
	}
	if byID["n4"].Status != model.TaskStatusAbandoned {
		t.Errorf("transitive descendant must be abandoned, got %+v", byID["n4"])
	}
	if byID["n5"].Archived {
		t.Error("unrelated branch must not be pruned")
	}
	for _, id := range []string{"n2", "n3", "n4"} {
		if byID[id].ArchiveReason == "" {
			t.Errorf("%s missing archive reason", id)
		}
	}

	// 幂等：再剪一次不应重复计数
	if again := pruneAbandonedBranch(dag); again != 0 {
		t.Errorf("second prune must be no-op, got %d", again)
	}
}

// TestDefaultAppendTarget 默认续接目标优先「激活且可追加」的那条。
func TestDefaultAppendTarget(t *testing.T) {
	ok1 := appendableWF("wf-1", model.TaskStatusCompleted,
		model.WorkflowNodeData{NodeID: "n1", Status: model.TaskStatusCompleted})
	ok2 := appendableWF("wf-2", model.TaskStatusCompleted,
		model.WorkflowNodeData{NodeID: "n2", Status: model.TaskStatusCompleted})
	stopped := appendableWF("wf-3", model.TaskStatusStopped,
		model.WorkflowNodeData{NodeID: "n3", Status: model.TaskStatusCompleted})

	wfs := &workflowContext{ActiveID: "wf-2", Candidates: []*workflowCandidate{ok1, ok2, stopped}}
	if got := wfs.DefaultAppendTarget(); got == nil || got.TaskID != "wf-2" {
		t.Fatalf("want active appendable wf-2, got %+v", got)
	}

	// 激活的那条不可追加时，退到最近一条可追加的
	wfs.ActiveID = "wf-3"
	if got := wfs.DefaultAppendTarget(); got == nil || got.TaskID != "wf-2" {
		t.Fatalf("want latest appendable wf-2, got %+v", got)
	}

	// 全都不可追加 → 只能新建
	all := &workflowContext{ActiveID: "wf-3", Candidates: []*workflowCandidate{stopped}}
	if got := all.DefaultAppendTarget(); got != nil {
		t.Errorf("want nil (force new), got %+v", got)
	}
}

// TestRenderWorkflowContext_ForceNew 用户要求新开工作流时，上下文必须明确
// 告知不可续接，否则模型仍会照着清单填 append。
func TestRenderWorkflowContext_ForceNew(t *testing.T) {
	wfs := &workflowContext{
		ActiveID: "wf-1",
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusCompleted,
				model.WorkflowNodeData{NodeID: "n1", Status: model.TaskStatusCompleted}),
		},
	}
	s := renderWorkflowContext(wfs, true)
	if !strings.Contains(s, "新开") || !strings.Contains(s, "不可续接") {
		t.Errorf("force-new context missing instruction:\n%s", s)
	}
}

// TestRenderWorkflowContext_ListsAnchorsOnly 上下文里列出的可依赖节点
// 必须与后端硬校验用的锚点集合一致 —— 前后口径不一致会让模型反复踩坑。
func TestRenderWorkflowContext_ListsAnchorsOnly(t *testing.T) {
	wfs := &workflowContext{
		Candidates: []*workflowCandidate{
			appendableWF("wf-1", model.TaskStatusFailed,
				model.WorkflowNodeData{NodeID: "n1_ok", Status: model.TaskStatusCompleted},
				model.WorkflowNodeData{NodeID: "n2_bad", Status: model.TaskStatusFailed,
					DependsOn: []string{"n1_ok"}}),
		},
	}
	s := renderWorkflowContext(wfs, false)
	if !strings.Contains(s, "n1_ok") {
		t.Errorf("usable anchor should be listed:\n%s", s)
	}
	if strings.Contains(s, "n2_bad") {
		t.Errorf("dead node must not be advertised as dependency:\n%s", s)
	}
	if !strings.Contains(s, "可追加：是") {
		t.Errorf("should mark appendable:\n%s", s)
	}
}
