package actuator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// mustJSON 序列化辅助
func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestFunctionalCallTurnToMessage_TextOnly 纯文本回复还原为 assistant 消息
func TestFunctionalCallTurnToMessage_TextOnly(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeFunctionalCall,
		Status:   model.TurnStatusSuccess,
		Output:   mustJSON(t, model.TurnLLMOutput{Text: "最终答案"}),
	}
	msg, ok := functionalCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if msg.Role != llm.RoleAssistant {
		t.Errorf("role = %v, want assistant", msg.Role)
	}
	if msg.Content != "最终答案" {
		t.Errorf("content = %q, want 最终答案", msg.Content)
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(msg.ToolCalls))
	}
}

// TestFunctionalCallTurnToMessage_WithToolCalls 带工具调用的 assistant 消息
func TestFunctionalCallTurnToMessage_WithToolCalls(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeFunctionalCall,
		Status:   model.TurnStatusSuccess,
		Output: mustJSON(t, model.TurnLLMOutput{
			ToolCalls: []model.TurnToolCallReq{
				{Name: "srv__search", Arguments: map[string]interface{}{"q": "go"}},
				{Name: "srv__read", Arguments: map[string]interface{}{"path": "/a"}},
			},
		}),
	}
	msg, ok := functionalCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Name != "srv__search" {
		t.Errorf("first tool = %q, want srv__search", msg.ToolCalls[0].Name)
	}
	if msg.ToolCalls[0].Arguments["q"] != "go" {
		t.Errorf("arg q = %v, want go", msg.ToolCalls[0].Arguments["q"])
	}
}

// TestFunctionalCallTurnToMessage_Empty 空输出不入历史
func TestFunctionalCallTurnToMessage_Empty(t *testing.T) {
	cases := []*model.TaskTurn{
		{Output: ""},
		{Output: mustJSON(t, model.TurnLLMOutput{})},
		{Output: "not json"},
	}
	for i, turn := range cases {
		if _, ok := functionalCallTurnToMessage(turn); ok {
			t.Errorf("case %d: expected message to be skipped", i)
		}
	}
}

// TestToolCallTurnToMessage 工具结果还原为 tool 消息
func TestToolCallTurnToMessage(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusSuccess,
		ToolName: "srv__search",
		Output:   mustJSON(t, model.TurnToolOutput{Content: "搜索结果内容"}),
	}
	msg, ok := toolCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if msg.Role != llm.RoleTool {
		t.Errorf("role = %v, want tool", msg.Role)
	}
	if msg.ToolName != "srv__search" {
		t.Errorf("tool name = %q, want srv__search", msg.ToolName)
	}
	if msg.Content != "搜索结果内容" {
		t.Errorf("content = %q", msg.Content)
	}
}

// TestToolCallTurnToMessage_NameFromInput 工具名缺失时回退到 input 快照
func TestToolCallTurnToMessage_NameFromInput(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusSuccess,
		ToolName: "",
		Input:    mustJSON(t, model.TurnToolInput{ToolName: "fallback_tool"}),
		Output:   mustJSON(t, model.TurnToolOutput{Content: "ok"}),
	}
	msg, ok := toolCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if msg.ToolName != "fallback_tool" {
		t.Errorf("tool name = %q, want fallback_tool", msg.ToolName)
	}
}

// TestToolCallTurnToMessage_EmptyOutput 无返回内容时给出占位文本，
// 保证模型知道工具执行过但没有产出，而不是收到空消息
func TestToolCallTurnToMessage_EmptyOutput(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusSuccess,
		ToolName: "srv__noop",
		Output:   mustJSON(t, model.TurnToolOutput{Content: ""}),
	}
	msg, ok := toolCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if msg.Content == "" {
		t.Error("expected placeholder content for empty tool output")
	}
}

// TestToolCallTurnToMessage_NoName 完全拿不到工具名则跳过
func TestToolCallTurnToMessage_NoName(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusSuccess,
	}
	if _, ok := toolCallTurnToMessage(turn); ok {
		t.Error("expected message to be skipped when tool name is unknown")
	}
}

// TestToolCallTurnToMessage_Failed 工具失败必须回喂 LLM。
//
// 这是防死循环的关键：若失败结果不入历史，模型只看到自己发起过调用
// 却没有任何结果，下一轮会重复请求同一个失败工具。
func TestToolCallTurnToMessage_Failed(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusFailed,
		ToolName: "srv__search",
		Output:   mustJSON(t, model.TurnToolOutput{Content: "连接超时", IsError: true}),
	}
	msg, ok := toolCallTurnToMessage(turn)
	if !ok {
		t.Fatal("failed tool turn must still produce a message")
	}
	if msg.Role != llm.RoleTool {
		t.Errorf("role = %v, want tool", msg.Role)
	}
	if !strings.Contains(msg.Content, "连接超时") {
		t.Errorf("content = %q, should contain error detail", msg.Content)
	}
	// 必须显式标注失败，避免模型把错误当正常结果
	if !strings.Contains(msg.Content, "失败") {
		t.Errorf("content = %q, should be marked as failure", msg.Content)
	}
}

// TestToolCallTurnToMessage_FailedFallbackToErrorMsg output 为空时用 error_msg 兜底
func TestToolCallTurnToMessage_FailedFallbackToErrorMsg(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusFailed,
		ToolName: "srv__search",
		ErrorMsg: "unknown tool: srv__search",
	}
	msg, ok := toolCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if !strings.Contains(msg.Content, "unknown tool") {
		t.Errorf("content = %q, should fall back to error_msg", msg.Content)
	}
}

// TestToolCallTurnToMessage_FailedNoDetail 失败且无任何详情时仍给出可读说明
func TestToolCallTurnToMessage_FailedNoDetail(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusFailed,
		ToolName: "srv__x",
	}
	msg, ok := toolCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if strings.TrimSpace(msg.Content) == "" {
		t.Error("expected non-empty content for failed turn without detail")
	}
}

// TestSignaturePersistedThroughTurnSnapshot 签名必须能完整走通
// 「LLM 响应 → turn 快照落库 → 历史重建」全链路。
//
// 这是最关键的不变量：agent 历史从 DB 重建，任一环节丢签名，
// 下一轮回放就会被 Gemini 判 400，整条会话卡死。
func TestSignaturePersistedThroughTurnSnapshot(t *testing.T) {
	// 1. 模拟 LLM 返回：并行调用，只有首个带签名
	result := &llm.ChatResult{
		ToolCalls: []llm.ToolCall{
			{Name: "srv__nav", Arguments: map[string]interface{}{"url": "https://a.com"}, Signature: "SIG_A"},
			{Name: "srv__snap"},
		},
	}

	// 2. 转成 turn 快照（写库前的形态）
	snapshot := buildLLMTurnOutput(result)
	if len(snapshot.ToolCalls) != 2 {
		t.Fatalf("snapshot tool calls = %d, want 2", len(snapshot.ToolCalls))
	}
	if snapshot.ToolCalls[0].Signature != "SIG_A" {
		t.Errorf("snapshot lost signature: %q", snapshot.ToolCalls[0].Signature)
	}

	// 3. 经 JSON 落库再读回
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeFunctionalCall,
		Status:   model.TurnStatusSuccess,
		Output:   mustJSON(t, snapshot),
	}

	// 4. 历史重建
	msg, ok := functionalCallTurnToMessage(turn)
	if !ok {
		t.Fatal("expected message to be built")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("rebuilt tool calls = %d, want 2", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Signature != "SIG_A" {
		t.Errorf("signature lost after DB round-trip: %q", msg.ToolCalls[0].Signature)
	}
	// 并行调用的后续 call 本就无签名，不能被凭空填充
	if msg.ToolCalls[1].Signature != "" {
		t.Errorf("second call should have empty signature, got %q", msg.ToolCalls[1].Signature)
	}
}

// ============================================================
// dropOrphanToolResults —— 孤立工具结果过滤
// ============================================================

// TestDropOrphanToolResults_KeepsMatched 有对应 assistant tool_calls 的结果要保留
func TestDropOrphanToolResults_KeepsMatched(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleUser, Content: "q"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "a"}, {Name: "b"}}},
		{Role: llm.RoleTool, ToolName: "a", Content: "ra"},
		{Role: llm.RoleTool, ToolName: "b", Content: "rb"},
	}
	got := dropOrphanToolResults(in)
	if len(got) != 4 {
		t.Fatalf("expected all 4 messages kept, got %d", len(got))
	}
}

// TestDropOrphanToolResults_DropsOrphan 前面没有对应 tool_calls 的工具结果必须丢弃。
//
// 这种情况会真实发生：某 task 的 functional_call turn 失败（被过滤出历史），
// 但它之前发起的 tool_call turn 已落库。留下孤立 tool 消息，
// Gemini 会因「functionResponse 没有对应的 functionCall」判 400。
func TestDropOrphanToolResults_DropsOrphan(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleUser, Content: "q"},
		// functional_call turn 失败被过滤，这里没有 assistant 消息
		{Role: llm.RoleTool, ToolName: "a", Content: "ra"},
	}
	got := dropOrphanToolResults(in)
	if len(got) != 1 {
		t.Fatalf("orphan tool result should be dropped, got %d messages", len(got))
	}
	if got[0].Role != llm.RoleUser {
		t.Errorf("remaining message should be user, got %s", got[0].Role)
	}
}

// TestDropOrphanToolResults_DropsUnrequestedTool assistant 没请求过的工具名要丢
func TestDropOrphanToolResults_DropsUnrequestedTool(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "a"}}},
		{Role: llm.RoleTool, ToolName: "a", Content: "ra"},
		{Role: llm.RoleTool, ToolName: "zzz", Content: "orphan"},
	}
	got := dropOrphanToolResults(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	for _, m := range got {
		if m.Role == llm.RoleTool && m.ToolName == "zzz" {
			t.Error("unrequested tool result should be dropped")
		}
	}
}

// TestDropOrphanToolResults_ResetsOnNewUserTurn 新的 user 提问后，
// 上一轮的 tool_calls 配额不能继续沿用
func TestDropOrphanToolResults_ResetsOnNewUserTurn(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "a"}}},
		{Role: llm.RoleTool, ToolName: "a", Content: "r1"},
		{Role: llm.RoleUser, Content: "new question"},
		// 新一轮里没有 assistant 请求 a，这条应被丢弃
		{Role: llm.RoleTool, ToolName: "a", Content: "r2"},
	}
	got := dropOrphanToolResults(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
	if got[len(got)-1].Role != llm.RoleUser {
		t.Errorf("last message should be the user question, got %s/%s",
			got[len(got)-1].Role, got[len(got)-1].Content)
	}
}

// TestDropOrphanToolResults_ParallelSameTool 同一工具被并行调用多次，
// 结果数量不能超过请求数量
func TestDropOrphanToolResults_ParallelSameTool(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "a"}, {Name: "a"}}},
		{Role: llm.RoleTool, ToolName: "a", Content: "r1"},
		{Role: llm.RoleTool, ToolName: "a", Content: "r2"},
		{Role: llm.RoleTool, ToolName: "a", Content: "r3-extra"},
	}
	got := dropOrphanToolResults(in)
	toolCount := 0
	for _, m := range got {
		if m.Role == llm.RoleTool {
			toolCount++
		}
	}
	if toolCount != 2 {
		t.Errorf("expected 2 tool results (matching 2 requests), got %d", toolCount)
	}
}

// TestTurnToMessage_FiltersByStatus turn 入历史的准入规则
func TestTurnToMessage_FiltersByStatus(t *testing.T) {
	llmOut := mustJSON(t, model.TurnLLMOutput{Text: "answer"})
	toolOut := mustJSON(t, model.TurnToolOutput{Content: "result"})

	cases := []struct {
		name string
		turn *model.TaskTurn
		want bool
	}{
		{"functional_call success",
			&model.TaskTurn{TurnType: model.TurnTypeFunctionalCall, Status: model.TurnStatusSuccess, Output: llmOut}, true},
		// LLM 调用失败没有产出，放进去只会让模型看到空 assistant 消息
		{"functional_call failed",
			&model.TaskTurn{TurnType: model.TurnTypeFunctionalCall, Status: model.TurnStatusFailed, Output: llmOut}, false},
		{"functional_call running",
			&model.TaskTurn{TurnType: model.TurnTypeFunctionalCall, Status: model.TurnStatusRunning, Output: llmOut}, false},
		{"tool_call success",
			&model.TaskTurn{TurnType: model.TurnTypeToolCall, Status: model.TurnStatusSuccess, ToolName: "t", Output: toolOut}, true},
		// 工具失败必须回喂，否则模型会重复请求同一个失败工具
		{"tool_call failed",
			&model.TaskTurn{TurnType: model.TurnTypeToolCall, Status: model.TurnStatusFailed, ToolName: "t", ErrorMsg: "boom"}, true},
		{"tool_call running",
			&model.TaskTurn{TurnType: model.TurnTypeToolCall, Status: model.TurnStatusRunning, ToolName: "t"}, false},
		// 规划期的 LLM 调用要入历史：它可能调了真实工具做调研，
		// 跳过这条 assistant 消息会让后面的 tool 结果变成孤立消息被丢弃
		{"planning success with text",
			&model.TaskTurn{TurnType: model.TurnTypeFunctionalCall, Phase: int(model.PhasePlanning), Status: model.TurnStatusSuccess, Output: llmOut}, true},
		{"planning failed",
			&model.TaskTurn{TurnType: model.TurnTypeFunctionalCall, Phase: int(model.PhasePlanning), Status: model.TurnStatusFailed, Output: llmOut}, false},
		{"workflow skipped",
			&model.TaskTurn{TurnType: model.TurnTypeWorkflow, Status: model.TurnStatusSuccess}, false},
	}

	for _, c := range cases {
		if _, ok := turnToMessage(c.turn); ok != c.want {
			t.Errorf("%s: got %v want %v", c.name, ok, c.want)
		}
	}
}

// TestPlanningTurnToMessage_FiltersSubmitPlan 规划期 turn 入历史时必须剔除
// submit_plan 协议工具调用。
//
// 它只在规划期注入；切到执行期后工具列表里没有它，历史里却带着
// 「assistant 调用了 submit_plan」，provider 会因引用未声明的工具判 400。
func TestPlanningTurnToMessage_FiltersSubmitPlan(t *testing.T) {
	// 只调了 submit_plan，无文本 → 过滤后什么都不剩，整条不入历史
	onlyPlan := &model.TaskTurn{
		TurnType: model.TurnTypeFunctionalCall,
		Phase:    int(model.PhasePlanning),
		Status:   model.TurnStatusSuccess,
		Output: mustJSON(t, model.TurnLLMOutput{
			ToolCalls: []model.TurnToolCallReq{
				{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{"mode": "single_agent"}},
			},
		}),
	}
	if _, ok := turnToMessage(onlyPlan); ok {
		t.Error("pure submit_plan turn should not enter history")
	}

	// 调研工具 + submit_plan 混合 → 只保留真实工具调用
	mixed := &model.TaskTurn{
		TurnType: model.TurnTypeFunctionalCall,
		Phase:    int(model.PhasePlanning),
		Status:   model.TurnStatusSuccess,
		Output: mustJSON(t, model.TurnLLMOutput{
			ToolCalls: []model.TurnToolCallReq{
				{Name: "search", Arguments: map[string]interface{}{"q": "x"}},
				{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{"mode": "workflow"}},
			},
		}),
	}
	msg, ok := turnToMessage(mixed)
	if !ok {
		t.Fatal("mixed planning turn should enter history")
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "search" {
		t.Errorf("want only the real tool call kept, got %+v", msg.ToolCalls)
	}
	if msg.Role != llm.RoleAssistant {
		t.Errorf("want assistant role, got %v", msg.Role)
	}
}

// ============================================================
// mergeAdjacentUsers —— 连续 user 消息折叠
// ============================================================

func TestMergeAdjacentUsers_CollapsesRun(t *testing.T) {
	// 失败 task 留下的连续提问，应折叠成最后一条
	in := []llm.Message{
		{Role: llm.RoleUser, Content: "q1"},
		{Role: llm.RoleUser, Content: "q2"},
		{Role: llm.RoleUser, Content: "q3"},
	}
	got := mergeAdjacentUsers(in)
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d", len(got))
	}
	if got[0].Content != "q3" {
		t.Errorf("want last question kept, got %q", got[0].Content)
	}
}

func TestMergeAdjacentUsers_KeepsSeparatedUsers(t *testing.T) {
	// 中间有 assistant 的正常轮次不能被折叠
	in := []llm.Message{
		{Role: llm.RoleUser, Content: "q1"},
		{Role: llm.RoleAssistant, Content: "a1"},
		{Role: llm.RoleUser, Content: "q2"},
	}
	got := mergeAdjacentUsers(in)
	if len(got) != 3 {
		t.Fatalf("want 3 messages, got %d", len(got))
	}
}

func TestMergeAdjacentUsers_PreservesToolSequence(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleUser, Content: "q"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "x"}}},
		{Role: llm.RoleTool, ToolName: "x", Content: "r"},
		{Role: llm.RoleAssistant, Content: "done"},
	}
	got := mergeAdjacentUsers(in)
	if len(got) != 4 {
		t.Fatalf("want 4 messages unchanged, got %d", len(got))
	}
}

func TestMergeAdjacentUsers_ShortInput(t *testing.T) {
	if got := mergeAdjacentUsers(nil); got != nil {
		t.Errorf("nil input should stay nil, got %v", got)
	}
	one := []llm.Message{{Role: llm.RoleUser, Content: "q"}}
	if got := mergeAdjacentUsers(one); len(got) != 1 {
		t.Errorf("single message should pass through, got %d", len(got))
	}
}

// TestRenderWorkflowSummaryIncludesDownloadURL 工作流摘要的产物清单要带下载链接。
//
// 根因回归：追问「产物在哪 / 给个链接」时，模型看不到下载 URL，只能自己编
// sandbox:/mnt/data 之类的假路径。摘要产物清单带上 f.URL 后，模型直接引用即可。
func TestRenderWorkflowSummaryIncludesDownloadURL(t *testing.T) {
	wf := &model.WorkflowOutputData{
		PlanSummary: "PDF 转 Word",
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n1", Name: "转换", Status: model.TaskStatusCompleted},
		},
		OutputFiles: []model.FileInfo{
			{Name: "report.docx", Path: "report.docx", Size: 43000,
				URL: "/api/v1/files/download?chat_id=c1&path=report.docx"},
		},
	}
	got := renderWorkflowSummary(wf, "wf-task-1")
	if !strings.Contains(got, "/api/v1/files/download?") {
		t.Fatalf("摘要产物清单缺下载链接：\n%s", got)
	}
	if !strings.Contains(got, "report.docx") {
		t.Fatalf("摘要产物清单缺文件名：\n%s", got)
	}
}
