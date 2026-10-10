package handler

import (
	"testing"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
)

// 工作流 token 归集的三个要点：
//   1. 节点消耗要含它派生的 deep_search 孙任务（否则严重低估）
//   2. 工作流合计要含工作流任务自身在规划期的调用
//   3. 没登记进 DAG 的游离子任务也要算进来，不能漏
// 以下用例逐个钉住。

func tokenSums(prompt, completion int64) dao.TokenSums {
	return dao.TokenSums{
		Prompt:     prompt,
		Completion: completion,
		Total:      prompt + completion,
	}
}

func TestBuildWorkflowNodeViews(t *testing.T) {
	// 结构：
	//   工作流 wf（自身规划期消耗 100/50）
	//     ├─ 节点 n1 → 子任务 t1（自身 1000/200，派生 deep_search d1 3000/300）
	//     ├─ 节点 n2 → 子任务 t2（自身 500/100，无孙任务）
	//     └─ 节点 n3（尚未派发，无 task_id）
	idx := dao.NewChatTokenIndex(
		map[string]dao.TokenSums{
			"wf": tokenSums(100, 50),
			"t1": tokenSums(1000, 200),
			"d1": tokenSums(3000, 300),
			"t2": tokenSums(500, 100),
		},
		map[string]dao.TokenSums{
			"wf": tokenSums(1500, 300), // t1 + t2 自身
			"t1": tokenSums(3000, 300), // d1
		},
	)
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", Name: "调研", TaskID: "t1", Status: 3},
		{NodeID: "n2", Name: "写作", TaskID: "t2", Status: 3},
		{NodeID: "n3", Name: "待派发", Status: 1},
	}

	views, usage := buildWorkflowNodeViews(nodes, idx, "wf", nil)

	if len(views) != 3 {
		t.Fatalf("节点视图数量应为 3，实际 %d", len(views))
	}

	// 节点 n1 = 自身 1200 + deep_search 3300 = 4500
	gotN1 := views[0]["token_usage"].(WorkflowTokenUsage)
	if gotN1.TotalTokens != 4500 {
		t.Errorf("n1 总 token 应为 4500（含 deep_search 孙任务），实际 %d", gotN1.TotalTokens)
	}
	if gotN1.PromptTokens != 4000 || gotN1.CompletionTokens != 500 {
		t.Errorf("n1 输入/输出拆分错误：prompt=%d completion=%d", gotN1.PromptTokens, gotN1.CompletionTokens)
	}

	// 节点 n2 = 自身 600
	gotN2 := views[1]["token_usage"].(WorkflowTokenUsage)
	if gotN2.TotalTokens != 600 {
		t.Errorf("n2 总 token 应为 600，实际 %d", gotN2.TotalTokens)
	}

	// 未派发节点没有 task_id，不应挂 token_usage
	if _, ok := views[2]["token_usage"]; ok {
		t.Error("未派发节点（无 task_id）不应带 token_usage")
	}

	// 工作流合计 = wf 自身 150 + n1 4500 + n2 600 = 5250
	if usage.TotalTokens != 5250 {
		t.Errorf("工作流总 token 应为 5250，实际 %d", usage.TotalTokens)
	}
	if usage.PromptTokens != 4600 || usage.CompletionTokens != 650 {
		t.Errorf("工作流输入/输出拆分错误：prompt=%d completion=%d", usage.PromptTokens, usage.CompletionTokens)
	}
}

// 游离子任务：工作流名下有个子任务没登记进 DAG（sub 比节点自身之和多），
// 差额必须补进工作流合计，否则这部分消耗会凭空消失。
func TestBuildWorkflowNodeViewsOrphanSubTask(t *testing.T) {
	idx := dao.NewChatTokenIndex(
		map[string]dao.TokenSums{
			"wf": tokenSums(10, 5),
			"t1": tokenSums(100, 20),
		},
		map[string]dao.TokenSums{
			"wf": tokenSums(300, 60), // t1 自身 120 + 游离任务 240（未登记在 DAG）
		},
	)
	nodes := []model.WorkflowNodeData{{NodeID: "n1", TaskID: "t1", Status: 3}}

	_, usage := buildWorkflowNodeViews(nodes, idx, "wf", nil)
	// wf 自身 15 + n1 120 + 游离 240 = 375
	if usage.TotalTokens != 375 {
		t.Errorf("应把未登记进 DAG 的子任务消耗补进合计，期望 375，实际 %d", usage.TotalTokens)
	}
}

// 索引构建失败（nil）时：节点照常返回，消耗显示 0，不能把面板拖挂。
func TestBuildWorkflowNodeViewsNilIndex(t *testing.T) {
	nodes := []model.WorkflowNodeData{{NodeID: "n1", TaskID: "t1", Status: 3}}

	views, usage := buildWorkflowNodeViews(nodes, nil, "wf", nil)
	if len(views) != 1 {
		t.Fatalf("索引缺失时节点仍应返回，实际 %d", len(views))
	}
	if views[0]["node_id"] != "n1" {
		t.Errorf("节点视图字段丢失：%v", views[0])
	}
	if usage.TotalTokens != 0 {
		t.Errorf("索引缺失时消耗应为 0，实际 %d", usage.TotalTokens)
	}
}
