package actuator

import (
	"testing"

	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestClassifyHistorySegments 验证历史段的归类：工作流摘要段必须单独计
// WorkflowSummary，不能混进 Detail。
//
// 这是「有工作流却显示工作流占比 0」的回归用例：规划期与执行期同样记录
// turn，用量弹窗取最近一条顶层 turn；若把摘要段整块计 Detail，摘要 token
// 就会被记错块，前端自然看不到工作流那一块。
func TestClassifyHistorySegments(t *testing.T) {
	counter := contextmgr.GetCounter()
	base := contextBlocks{SystemPrompt: 100, Tools: 50}
	mk := func() []historySegment {
		return []historySegment{
			{TaskID: "t1", Kind: segKindConversation, Messages: []llm.Message{
				{Role: llm.RoleUser, Content: "上一轮的问题"},
				{Role: llm.RoleAssistant, Content: "上一轮的回答"},
			}},
			{TaskID: "wf1", Kind: segKindWorkflowSummary, Messages: []llm.Message{
				{Role: llm.RoleAssistant, Content: "【此前工作流执行摘要】各节点状态：…"},
			}},
		}
	}

	got, compressible := classifyHistorySegments(mk(), counter, base)
	if got.WorkflowSummary == 0 {
		t.Fatalf("workflow summary segment must land in WorkflowSummary: %+v", got)
	}
	if got.Detail == 0 {
		t.Fatalf("conversation segment should land in Detail: %+v", got)
	}
	// 固定开销必须被带进来，不能从零开始算
	if got.SystemPrompt != base.SystemPrompt || got.Tools != base.Tools {
		t.Fatalf("base blocks lost: %+v", got)
	}
	// 工作流摘要段不可压：它本身已是压缩产物
	if len(compressible) != 1 || compressible[0].TaskID != "t1" {
		t.Fatalf("only conversation segments should be compressible: %+v", compressible)
	}
	// 段级 token 要缓存下来，供压缩选取直接复用
	if compressible[0].Tokens == 0 {
		t.Fatalf("segment tokens not cached: %+v", compressible[0])
	}

	// 摘要不能混进 Detail：把 wf 段去掉后 Detail 应保持不变
	withoutWF, _ := classifyHistorySegments(mk()[:1], counter, base)
	if got.Detail != withoutWF.Detail {
		t.Fatalf("workflow summary leaked into Detail: with=%d without=%d", got.Detail, withoutWF.Detail)
	}
	if withoutWF.WorkflowSummary != 0 {
		t.Fatalf("workflow summary should be 0 without wf segment: %d", withoutWF.WorkflowSummary)
	}
}

// TestApplyTail 共用组装入口的尾部追加分支：
//   - 无 tail：原样返回，分块就是压缩给出的基础分块
//   - 有 tail：消息追加到末尾、连续 assistant 被折叠、分块只增 UserAndCurrent
//   - 有 tail 且顶穿底线：判死，而不是静默放行
//
// 这是规划期重规划轮与执行期共用同一条规则的那道接缝 —— 折叠与判死
// 一旦只在一侧生效，另一侧就会带着非法序列或超限请求去调 provider。
func TestApplyTail(t *testing.T) {
	const limit = 8000 // ceiling = 6400
	// 基础分块刻意留在底线之下（4200 < 6400），否则第一分支就判死了，
	// 测不到「tail 的增量是否把总量顶穿底线」这个真正要验的点。
	base := contextBlocks{
		SystemPrompt: 2000, Tools: 1000, WorkflowSummary: 300,
		Compacted: 400, Detail: 0, UserAndCurrent: 500,
	}
	// 序列末尾是 assistant（当前 task 摘要），正是会被 tail 撞出连续
	// assistant 的形状 —— 真实数据 chat 8a3c060c 就是这一形态。
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleAssistant, Content: "【此前对话的历史工作记录】"},
	}

	// 1. 无 tail：原样返回
	plain, err := applyTail("t1", msgs, base, nil, limit)
	if err != nil {
		t.Fatalf("empty tail should not fail: %v", err)
	}
	if len(plain.Messages) != len(msgs) {
		t.Fatalf("empty tail must not touch messages: %d vs %d", len(plain.Messages), len(msgs))
	}
	if plain.Blocks.Total != base.Total() || plain.Blocks.ContextLimit != limit {
		t.Fatalf("empty tail should return base snapshot: %+v", plain.Blocks)
	}

	// 2. 有 tail：追加 + 折叠 + 只增当前交互
	tail := []llm.Message{
		{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{{Name: "submit_plan"}}},
		{Role: llm.RoleTool, ToolName: "submit_plan", Content: "规划不合法：输出节点不唯一"},
	}
	got, err := applyTail("t1", msgs, base, tail, limit)
	if err != nil {
		t.Fatalf("tail under ceiling should pass: %v", err)
	}
	// assistant(摘要) 与 assistant(submit_plan) 折叠成一条，tool 结果留在最后
	if len(got.Messages) != 3 {
		t.Fatalf("want 3 messages after collapse, got %d: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[1].Role != llm.RoleAssistant || len(got.Messages[1].ToolCalls) != 1 {
		t.Fatalf("collapsed assistant must keep tool calls: %+v", got.Messages[1])
	}
	if got.Messages[2].Role != llm.RoleTool {
		t.Fatalf("last message must be the tool feedback: %+v", got.Messages[2])
	}
	// 连续同角色必须清零，否则 Gemini 直接 400
	for i := 1; i < len(got.Messages); i++ {
		if got.Messages[i].Role == got.Messages[i-1].Role &&
			got.Messages[i].Role != llm.RoleTool {
			t.Fatalf("adjacent same role at %d: %+v", i, got.Messages)
		}
	}
	extra := contextmgr.GetCounter().CountMessages(tail)
	if got.Blocks.UserAndCurrent != base.UserAndCurrent+extra {
		t.Fatalf("UserAndCurrent = %d, want %d", got.Blocks.UserAndCurrent, base.UserAndCurrent+extra)
	}
	if got.Blocks.Total != base.Total()+extra {
		t.Fatalf("Total = %d, want %d", got.Blocks.Total, base.Total()+extra)
	}
	// 其余块不能被 tail 改动
	if got.Blocks.SystemPrompt != base.SystemPrompt || got.Blocks.Detail != base.Detail ||
		got.Blocks.Compacted != base.Compacted || got.Blocks.WorkflowSummary != base.WorkflowSummary {
		t.Fatalf("tail must not touch other blocks: %+v", got.Blocks)
	}

	// 3. 顶穿底线：判死（tail 不参与压缩，只能在这里拦住）
	over := contextBlocks{SystemPrompt: 6000, UserAndCurrent: 500} // 已 6500 > ceiling 6400
	if _, err := applyTail("t-over", msgs, over, tail, limit); err == nil {
		t.Fatalf("tail pushing total over ceiling must fail")
	}
}

// TestTailTurnBlocks 组装后追加的尾部消息（规划期重规划反馈）只增加
// 「当前交互」这一块，其余块必须与压缩给出的基础分块一致。
func TestTailTurnBlocks(t *testing.T) {
	const limit = 8000
	base := contextBlocks{
		SystemPrompt: 2595, Tools: 2013, WorkflowSummary: 682,
		Compacted: 800, Detail: 0, UserAndCurrent: 940,
	}
	plain := tailTurnBlocks(base, nil, limit)
	if plain.UserAndCurrent != base.UserAndCurrent || plain.Total != base.Total() {
		t.Fatalf("no feedback should return base snapshot: %+v", plain)
	}
	if plain.ContextLimit != limit {
		t.Fatalf("ContextLimit = %d, want %d", plain.ContextLimit, limit)
	}

	feedback := []llm.Message{
		{Role: llm.RoleAssistant, Content: "（上一次提交被拒）"},
		{Role: llm.RoleTool, Content: "规划不合法：输出节点不唯一", ToolName: "submit_plan"},
	}
	got := tailTurnBlocks(base, feedback, limit)
	extra := contextmgr.GetCounter().CountMessages(feedback)
	if got.UserAndCurrent != base.UserAndCurrent+extra {
		t.Fatalf("UserAndCurrent = %d, want %d", got.UserAndCurrent, base.UserAndCurrent+extra)
	}
	if got.Total != plain.Total+extra {
		t.Fatalf("Total = %d, want %d", got.Total, plain.Total+extra)
	}
	// 其余块不能被反馈改动
	if got.SystemPrompt != base.SystemPrompt || got.Detail != base.Detail ||
		got.Compacted != base.Compacted || got.WorkflowSummary != base.WorkflowSummary {
		t.Fatalf("feedback must not touch other blocks: %+v", got)
	}
}
