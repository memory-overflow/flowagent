package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// TestBuildSubTaskPrompt 节点子任务的 prompt 必须由五部分构成：
// 用户原始输入 / 工作流总体目标 / 本步骤说明 / 前置步骤输出 / 用户附件清单。
//
// 这些是子任务唯一能拿到的「来龙去脉」（它看不到主对话历史），缺任何
// 一项节点都会在不完整的信息下作业。
func TestBuildSubTaskPrompt(t *testing.T) {
	wfTask := &model.Task{
		TaskID: "wf1",
		ChatID: "chat1",
		TaskInput: model.JsonField[*model.TaskInputData]{
			Data: &model.TaskInputData{InputText: "帮我写一份季度报告"},
		},
	}
	dag := &model.WorkflowOutputData{
		PlanSummary: "调研三个竞品并输出对比报告",
		Nodes: []model.WorkflowNodeData{
			{
				NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted,
				Output: &model.AgentOutputData{
					ReplyText:   "竞品 A/B/C 的核心指标已收集",
					OutputFiles: []model.FileInfo{{Name: "raw.csv", Path: "raw.csv", Size: 2048}},
				},
			},
			{NodeID: "n2", Name: "撰写", Description: "按检索结果成文", DependsOn: []string{"n1"}},
		},
	}
	node := &dag.Nodes[1]

	got := buildSubTaskPrompt(wfTask, dag, node, nil, "")

	for _, want := range []string{
		"【用户原始输入】", "帮我写一份季度报告",
		"【工作流总体目标】", "调研三个竞品并输出对比报告",
		"【你负责的步骤】", "步骤名称：撰写", "步骤说明：按检索结果成文",
		"【前置步骤的输出】", "检索 的输出", "竞品 A/B/C 的核心指标已收集",
		// 上游落盘的文件必须带路径，否则下游只能基于摘要臆造
		"该步骤产出的文件", "raw.csv",
		"请完成上述这一个步骤",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("subtask prompt missing %q:\n%s", want, got)
		}
	}
	// 本步骤自己的输出不该出现在自己的 prompt 里
	if strings.Contains(got, "【前置步骤的输出】\n\n◆ 撰写") {
		t.Fatalf("node must not see its own output as upstream: %s", got)
	}
}

// TestBuildSubTaskPromptIncludesUserInput 用户原始输入必须注入节点 prompt，
// 且**有 plan_summary 时也要注入**。
//
// 这是「规划期读到文件路径却丢失」的根因：plan_summary 是模型概括，
// 可能丢掉用户原文里的关键细节（文件路径、文件名）。修复前用户原文只在
// plan_summary 缺失的兜底路径才进 prompt，正常路径下节点拿到的只有概括。
func TestBuildSubTaskPromptIncludesUserInput(t *testing.T) {
	wfTask := &model.Task{
		TaskID: "wf1",
		ChatID: "chat1",
		TaskInput: model.JsonField[*model.TaskInputData]{
			Data: &model.TaskInputData{InputText: "根据 docs/evals/fixtures/notes 下的 4 份 Markdown 笔记写本周周报"},
		},
	}
	// plan_summary 非空、且丢了文件路径 —— 正是 bug 现场
	dag := &model.WorkflowOutputData{
		PlanSummary: "根据工作笔记生成周报",
		Nodes:       []model.WorkflowNodeData{{NodeID: "n1", Name: "撰写"}},
	}

	got := buildSubTaskPrompt(wfTask, dag, &dag.Nodes[0], nil, "")

	if !strings.Contains(got, "【用户原始输入】") {
		t.Fatalf("user input not injected:\n%s", got)
	}
	if !strings.Contains(got, "docs/evals/fixtures/notes") {
		t.Fatalf("用户原文里的文件路径丢失了：\n%s", got)
	}
	if !strings.Contains(got, "根据工作笔记生成周报") {
		t.Fatalf("plan_summary 也该保留：\n%s", got)
	}
}

// TestBuildSubTaskPromptIncludesNodeContext 节点级 context 必须注入【本步骤背景】段。
//
// 续接节点的 context 承载「本轮用户新增的诉求」—— 全局的「用户原始输入」
// 是创建轮原文，续接轮的新输入只有靠这里传进来，丢了节点就不知道要干什么。
func TestBuildSubTaskPromptIncludesNodeContext(t *testing.T) {
	wfTask := &model.Task{
		TaskID: "wf1",
		ChatID: "chat1",
		TaskInput: model.JsonField[*model.TaskInputData]{
			Data: &model.TaskInputData{InputText: "调研三家竞品写报告"},
		},
	}

	// 有 context 的节点：注入【本步骤背景】
	dag := &model.WorkflowOutputData{
		PlanSummary: "调研竞品出报告",
		Nodes: []model.WorkflowNodeData{{
			NodeID:  "n3",
			Name:    "补充对比表格",
			Context: "用户本轮新增诉求：把报告补充一个价格/性能/生态三维对比表格。",
		}},
	}
	got := buildSubTaskPrompt(wfTask, dag, &dag.Nodes[0], nil, "")
	for _, want := range []string{
		"【本步骤背景】",
		"把报告补充一个价格/性能/生态三维对比表格",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("node context 未注入（缺 %q）：\n%s", want, got)
		}
	}

	// 无 context 的节点：不出现【本步骤背景】
	plain := &model.WorkflowNodeData{NodeID: "n1", Name: "撰写"}
	bare := buildSubTaskPrompt(wfTask, &model.WorkflowOutputData{
		PlanSummary: "调研竞品出报告",
		Nodes:       []model.WorkflowNodeData{*plain},
	}, plain, nil, "")
	if strings.Contains(bare, "【本步骤背景】") {
		t.Fatalf("无 node context 不应渲染【本步骤背景】：\n%s", bare)
	}
}

// TestBuildSubTaskPromptIncludesInputFiles 用户附件必须进节点 prompt。
//
// 这是「按我上传的表格写报告」这类需求跑不通的根因：附件只存在于主对话
// 的 task_input 里，而子任务刻意不带对话历史 —— 文件明明就在同一个工作
// 区，却没人告诉节点它存在。修复前只有 plan_summary 缺失的兜底路径才会
// 渲染附件，正常路径下清单永远为空。
func TestBuildSubTaskPromptIncludesInputFiles(t *testing.T) {
	wfTask := &model.Task{
		TaskID: "wf1",
		ChatID: "chat1",
		TaskInput: model.JsonField[*model.TaskInputData]{
			Data: &model.TaskInputData{InputText: "按表格写报告"},
		},
	}
	dag := &model.WorkflowOutputData{
		PlanSummary: "按上传的表格写报告",
		Nodes:       []model.WorkflowNodeData{{NodeID: "n1", Name: "撰写"}},
	}
	files := []model.FileInfo{
		{Name: "data.xlsx", Path: "uploads/data.xlsx", Size: 4096, FileType: 4},
	}

	got := buildSubTaskPrompt(wfTask, dag, &dag.Nodes[0], files, "")

	// 有 plan_summary 时也要渲染附件 —— 修复前这条路径是空的
	for _, want := range []string{
		"## 用户附件", "uploads/data.xlsx", "data.xlsx",
		"处理方式", "read_file",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("input files not rendered into subtask prompt (missing %q):\n%s", want, got)
		}
	}

	// 无附件时不应留下空清单
	bare := buildSubTaskPrompt(wfTask, dag, &dag.Nodes[0], nil, "")
	if strings.Contains(bare, "## 用户附件") {
		t.Fatalf("no files should not render an empty manifest: %s", bare)
	}
}
