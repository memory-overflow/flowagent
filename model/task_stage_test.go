package model

import "testing"

// TestNextStage_ExecutingDirectAnswer 执行期一问一答：LLM 不调工具，直接收口。
func TestNextStage_ExecutingDirectAnswer(t *testing.T) {
	hint := &StageTransitionHint{MaxToolCallRound: 3}

	// functional_call_wait → functional_call_doing
	if got := StageFunctionalCallWait.NextStage(hint); got != StageFunctionalCallDoing {
		t.Errorf("functional_call_wait → got %v, want functional_call_doing", got)
	}
	// functional_call_doing → finished（无需工具调用）
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageFinished {
		t.Errorf("functional_call_doing (no tool) → got %v, want finished", got)
	}
}

// TestNextStage_ToolCallLoop 验证 tool_call loop 流转和轮次上限
func TestNextStage_ToolCallLoop(t *testing.T) {
	hint := &StageTransitionHint{
		NeedToolCall:     true,
		ToolCallRound:    0,
		MaxToolCallRound: 3,
	}

	// functional_call_doing → tool_call_wait（需要工具调用，轮次未超）
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageToolCallWait {
		t.Errorf("functional_call_doing (need tool, round 0/3) → got %v, want tool_call_wait", got)
	}
	// tool_call_wait → tool_call_doing
	if got := StageToolCallWait.NextStage(hint); got != StageToolCallDoing {
		t.Errorf("tool_call_wait → got %v, want tool_call_doing", got)
	}
	// tool_call_doing → functional_call_wait（loop 回 LLM）
	if got := StageToolCallDoing.NextStage(hint); got != StageFunctionalCallWait {
		t.Errorf("tool_call_doing → got %v, want functional_call_wait", got)
	}

	// 轮次达到上限 → 强制结束
	hint.ToolCallRound = 3
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageFinished {
		t.Errorf("functional_call_doing (round 3/3, exhausted) → got %v, want finished", got)
	}
}

// TestNextStage_PlanSubmitted 规划期提交规划后必须回到 functional_call，
// 而不是收口 finished —— 此时 phase 已切执行期，真正的活还没干。
func TestNextStage_PlanSubmitted(t *testing.T) {
	hint := &StageTransitionHint{
		PlanSubmitted:    true,
		MaxToolCallRound: 3,
	}
	got := StageFunctionalCallDoing.Transit(ActionSuccess, hint)
	if got.Stage != StageFunctionalCallWait {
		t.Errorf("plan submitted → got stage %v, want functional_call_wait", got.Stage)
	}
	if got.Status != TaskStatusRunning {
		t.Errorf("plan submitted → got status %v, want running", got.Status)
	}
	if got.IsTerminal {
		t.Error("plan submitted should not be terminal")
	}
}

// TestNextStage_PlanSubmittedBeatsToolCall PlanSubmitted 优先于 NeedToolCall。
// 模型同时给出 submit_plan 与真实工具调用时，执行器以提交规划为准，
// 状态机的判定顺序必须与之一致，否则会漏掉 phase 已切换的事实。
func TestNextStage_PlanSubmittedBeatsToolCall(t *testing.T) {
	hint := &StageTransitionHint{
		PlanSubmitted:    true,
		NeedToolCall:     true,
		ToolCallRound:    0,
		MaxToolCallRound: 3,
	}
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageFunctionalCallWait {
		t.Errorf("plan submitted + need tool → got %v, want functional_call_wait", got)
	}
}

// TestNextStage_PlanningResearch 规划期调研：模型只调真实工具时走 tool_call，
// 回来后仍是 functional_call（phase 仍为规划期，由执行器判定）。
func TestNextStage_PlanningResearch(t *testing.T) {
	hint := &StageTransitionHint{
		NeedToolCall:     true,
		ToolCallRound:    0,
		MaxToolCallRound: 5,
	}
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageToolCallWait {
		t.Errorf("planning research → got %v, want tool_call_wait", got)
	}
	if got := StageToolCallDoing.NextStage(hint); got != StageFunctionalCallWait {
		t.Errorf("after research → got %v, want functional_call_wait", got)
	}
}

// TestNextStage_ZeroMaxRoundFallback MaxToolCallRound 未设置（0）时
// 应回落到默认上限，而不是当成"上限为 0 立即收口"。
func TestNextStage_ZeroMaxRoundFallback(t *testing.T) {
	hint := &StageTransitionHint{
		NeedToolCall:  true,
		ToolCallRound: 0,
		// MaxToolCallRound 故意留 0
	}
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageToolCallWait {
		t.Errorf("zero max round → got %v, want tool_call_wait (fallback to default)", got)
	}

	hint.ToolCallRound = DefaultMaxToolCallRound
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageFinished {
		t.Errorf("zero max round at default limit → got %v, want finished", got)
	}
}

// TestNextStage_NilHint 验证 hint 为 nil 时的默认行为
func TestNextStage_NilHint(t *testing.T) {
	// nil hint → 默认不需要工具调用，直接完成
	if got := StageFunctionalCallDoing.NextStage(nil); got != StageFinished {
		t.Errorf("functional_call_doing (nil hint) → got %v, want finished", got)
	}
	// tool_call_doing 与 hint 无关，恒回 functional_call
	if got := StageToolCallDoing.NextStage(nil); got != StageFunctionalCallWait {
		t.Errorf("tool_call_doing (nil hint) → got %v, want functional_call_wait", got)
	}
	// 终态保持
	if got := StageFinished.NextStage(nil); got != StageFinished {
		t.Errorf("finished → got %v, want finished", got)
	}
}

// TestTransit_FailedAndStop 失败与停止两个动作的流转
func TestTransit_FailedAndStop(t *testing.T) {
	// 失败：stage 保留在失败发生处，便于定位
	got := StageToolCallDoing.Transit(ActionFailed, nil)
	if got.Stage != StageToolCallDoing || got.Status != TaskStatusFailed || !got.IsTerminal {
		t.Errorf("failed → got %+v, want {tool_call_doing failed terminal}", got)
	}

	// 停止：doing 回退到对应 wait
	got = StageFunctionalCallDoing.Transit(ActionStop, nil)
	if got.Stage != StageFunctionalCallWait || got.Status != TaskStatusStopped || !got.IsTerminal {
		t.Errorf("stop → got %+v, want {functional_call_wait stopped terminal}", got)
	}
}

// TestStageHelpers 验证辅助方法
func TestStageHelpers(t *testing.T) {
	waitStages := AllWaitStages()
	if len(waitStages) != 3 {
		t.Errorf("wait stages count = %d, want 3 (functional_call + tool_call + workflow)", len(waitStages))
	}
	for _, s := range waitStages {
		if !s.IsWaitStage() {
			t.Errorf("%v should be wait stage", s)
		}
		if s.IsDoingStage() {
			t.Errorf("%v should not be doing stage", s)
		}
		// wait → doing → wait 往返一致
		if s.ToDoingStage().ToWaitStage() != s {
			t.Errorf("%v roundtrip failed", s)
		}
	}

	doingStages := AllDoingStages()
	if len(doingStages) != 3 {
		t.Errorf("doing stages count = %d, want 3 (functional_call + tool_call + workflow)", len(doingStages))
	}
	for _, s := range doingStages {
		if !s.IsDoingStage() {
			t.Errorf("%v should be doing stage", s)
		}
		if s.IsWaitStage() {
			t.Errorf("%v should not be wait stage", s)
		}
		if s.ToWaitStage().ToDoingStage() != s {
			t.Errorf("%v roundtrip failed", s)
		}
	}

	if !StageFinished.IsTerminalStage() {
		t.Error("finished should be terminal")
	}
	if StageFunctionalCallWait.IsTerminalStage() {
		t.Error("functional_call_wait should not be terminal")
	}
}

// TestNextStage_PlanDelegated 判定 workflow 后当前对话任务必须直接收口。
//
// 活已移交给对话绑定的工作流任务，若还回到 functional_call 继续跑，
// 同一件事会被做两遍，用户还会看到两份互相矛盾的回复。
func TestNextStage_PlanDelegated(t *testing.T) {
	hint := &StageTransitionHint{
		PlanDelegated:    true,
		MaxToolCallRound: 3,
	}
	got := StageFunctionalCallDoing.Transit(ActionSuccess, hint)
	if got.Stage != StageFinished {
		t.Errorf("plan delegated → got stage %v, want finished", got.Stage)
	}
	if got.Status != TaskStatusCompleted {
		t.Errorf("plan delegated → got status %v, want completed", got.Status)
	}
	if !got.IsTerminal {
		t.Error("plan delegated should be terminal")
	}
}

// TestNextStage_PlanDelegatedBeatsSubmitted PlanDelegated 优先于 PlanSubmitted。
//
// 移交工作流时同样会切 phase，两个标志可能同时为真（尤其是从 DB 恢复 hint 时）。
// 若被 PlanSubmitted 分支截住，任务会回到 functional_call 再跑一遍单 Agent。
func TestNextStage_PlanDelegatedBeatsSubmitted(t *testing.T) {
	hint := &StageTransitionHint{
		PlanDelegated:    true,
		PlanSubmitted:    true,
		NeedToolCall:     true,
		MaxToolCallRound: 3,
	}
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageFinished {
		t.Errorf("delegated + submitted + need tool → got %v, want finished", got)
	}
}

// TestNextWorkflowStatus 追加节点后工作流状态的调整规则：
// 执行中不动（新节点并入正在跑的 DAG），其余一律回到排队等待执行。
func TestNextWorkflowStatus(t *testing.T) {
	cases := []struct {
		current     TaskStatus
		wantStatus  TaskStatus
		wantChanged bool
	}{
		// 执行中：新节点直接并入，不打断当前执行
		{TaskStatusRunning, TaskStatusRunning, false},
		// 已跑完 / 失败 / 被终止：有新活要干，重新排队
		{TaskStatusCompleted, TaskStatusQueued, true},
		{TaskStatusFailed, TaskStatusQueued, true},
		{TaskStatusStopped, TaskStatusQueued, true},
		{TaskStatusPartialFailed, TaskStatusQueued, true},
		// 本来就在排队：状态值不变，但仍按 changed 处理，
		// 让调用方一并把 stage 拨回 workflow_wait（stage 可能已漂移）
		{TaskStatusQueued, TaskStatusQueued, true},
	}

	for _, c := range cases {
		gotStatus, gotChanged := NextWorkflowStatus(c.current)
		if gotStatus != c.wantStatus || gotChanged != c.wantChanged {
			t.Errorf("NextWorkflowStatus(%d) = (%d, %v), want (%d, %v)",
				c.current, gotStatus, gotChanged, c.wantStatus, c.wantChanged)
		}
	}
}

// TestWorkflowStageScheduled 工作流阶段已接入调度器，应出现在扫描列表里，
// 且能正确完成 wait/doing 往返与终态流转。
func TestWorkflowStageScheduled(t *testing.T) {
	foundWait := false
	for _, s := range AllWaitStages() {
		if s == StageWorkflowWait {
			foundWait = true
		}
	}
	if !foundWait {
		t.Error("StageWorkflowWait must be in AllWaitStages (workflow actuator exists)")
	}

	foundDoing := false
	for _, s := range AllDoingStages() {
		if s == StageWorkflowDoing {
			foundDoing = true
		}
	}
	if !foundDoing {
		t.Error("StageWorkflowDoing must be in AllDoingStages (workflow actuator exists)")
	}

	// 必须是合法的 wait/doing 阶段，且能正确往返
	if !StageWorkflowWait.IsWaitStage() || !StageWorkflowDoing.IsDoingStage() {
		t.Error("workflow stages should be recognized as wait/doing")
	}
	if StageWorkflowWait.ToDoingStage() != StageWorkflowDoing {
		t.Error("workflow_wait → doing mapping broken")
	}
	if StageWorkflowDoing.ToWaitStage() != StageWorkflowWait {
		t.Error("workflow_doing → wait mapping broken")
	}

	// 编排成功应流转到 finished 终态
	tr := StageWorkflowDoing.Transit(ActionSuccess, nil)
	if tr.Stage != StageFinished || tr.Status != TaskStatusCompleted || !tr.IsTerminal {
		t.Errorf("workflow_doing success → got %+v, want {finished completed terminal}", tr)
	}
}

// TestTaskType_String 任务类型名称映射
func TestTaskType_String(t *testing.T) {
	cases := []struct {
		tt   TaskType
		want string
	}{
		{TaskTypeConversation, "conversation"},
		{TaskTypeWorkflow, "workflow"},
		{TaskTypeWorkflowSubTask, "workflow_subtask"},
		{TaskType(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.tt.String(); got != c.want {
			t.Errorf("TaskType(%d).String() = %q, want %q", c.tt, got, c.want)
		}
	}
}

// TestTaskPhase_String phase 名称映射
func TestTaskPhase_String(t *testing.T) {
	cases := []struct {
		phase TaskPhase
		want  string
	}{
		{PhasePlanning, "planning"},
		{PhaseExecuting, "executing"},
		{TaskPhase(0), "unknown"},
	}
	for _, c := range cases {
		if got := c.phase.String(); got != c.want {
			t.Errorf("TaskPhase(%d).String() = %q, want %q", c.phase, got, c.want)
		}
	}
}
