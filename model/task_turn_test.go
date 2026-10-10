package model

import "testing"

// TestTurnTypeFromStage 验证 stage → turn 类型的映射。
//
// phase 不参与推导：规划期与执行期共享 functional_call 调度阶段，
// 两者的 turn_type 都是 functional_call，区别记在 phase 列上。
func TestTurnTypeFromStage(t *testing.T) {
	cases := []struct {
		stage TaskStage
		want  TurnType
	}{
		{StageFunctionalCallWait, TurnTypeFunctionalCall},
		{StageFunctionalCallDoing, TurnTypeFunctionalCall},
		{StageToolCallWait, TurnTypeToolCall},
		{StageToolCallDoing, TurnTypeToolCall},
		{StageWorkflowWait, TurnTypeWorkflow},
		{StageWorkflowDoing, TurnTypeWorkflow},
		{StageFinished, 0},
		{StageNone, 0},
	}
	for _, c := range cases {
		if got := TurnTypeFromStage(c.stage); got != c.want {
			t.Errorf("TurnTypeFromStage(%v) = %v, want %v", c.stage, got, c.want)
		}
	}
}

// TestTurnTypeIsLLMCall 验证 LLM 调用类型判定
func TestTurnTypeIsLLMCall(t *testing.T) {
	if !TurnTypeFunctionalCall.IsLLMCall() {
		t.Error("functional_call should be an LLM call")
	}
	// 存量的 legacy planning 记录本质就是一次 LLM 调用
	if !TurnTypePlanningLegacy.IsLLMCall() {
		t.Error("legacy planning should be an LLM call")
	}
	if TurnTypeToolCall.IsLLMCall() {
		t.Error("tool_call should not be an LLM call")
	}
	if TurnTypeWorkflow.IsLLMCall() {
		t.Error("workflow should not be an LLM call")
	}
}

// TestTurnTypeString 验证类型名称，保证日志可读。
//
// legacy planning 也输出 functional_call —— 它不是一种独立的调用类型，
// 「在规划」这件事由 phase 表达。
func TestTurnTypeString(t *testing.T) {
	cases := map[TurnType]string{
		TurnTypePlanningLegacy: "functional_call",
		TurnTypeFunctionalCall: "functional_call",
		TurnTypeToolCall:       "tool_call",
		TurnTypeWorkflow:       "workflow",
		TurnType(99):           "unknown",
	}
	for tt, want := range cases {
		if got := tt.String(); got != want {
			t.Errorf("TurnType(%d).String() = %q, want %q", tt, got, want)
		}
	}
}

// ============================================================
// NormalizeTurn —— 存量数据归一到「双维度」表示
// ============================================================

// TestNormalizeTurn_LegacyPlanning 存量的 turn_type=1 要拆成
// turn_type=functional_call + phase=planning，两个维度都补齐。
func TestNormalizeTurn_LegacyPlanning(t *testing.T) {
	turn := &TaskTurn{TurnType: TurnTypePlanningLegacy}
	NormalizeTurn(turn)

	if turn.TurnType != TurnTypeFunctionalCall {
		t.Errorf("turn_type = %v, want functional_call", turn.TurnType)
	}
	if turn.TurnPhase() != PhasePlanning {
		t.Errorf("phase = %v, want planning", turn.TurnPhase())
	}
	if !turn.IsPlanning() {
		t.Error("IsPlanning() should be true")
	}
}

// TestNormalizeTurn_LegacyPhaseZero phase 列上线前的 functional_call / tool_call
// 记录都属于执行期（那时规划期一律写成 turn_type=1）。
func TestNormalizeTurn_LegacyPhaseZero(t *testing.T) {
	for _, tt := range []TurnType{TurnTypeFunctionalCall, TurnTypeToolCall, TurnTypeWorkflow} {
		turn := &TaskTurn{TurnType: tt, Phase: 0}
		NormalizeTurn(turn)
		if turn.TurnPhase() != PhaseExecuting {
			t.Errorf("turn_type=%v phase=0 → got %v, want executing", tt, turn.TurnPhase())
		}
		if turn.TurnType != tt {
			t.Errorf("turn_type must not change: got %v, want %v", turn.TurnType, tt)
		}
	}
}

// TestNormalizeTurn_Idempotent 新写入的记录两个字段本已正确，归一不应改变它们。
// 幂等很重要 —— BuildThinkProcess 会对同一批 turn 反复调用。
func TestNormalizeTurn_Idempotent(t *testing.T) {
	cases := []*TaskTurn{
		{TurnType: TurnTypeFunctionalCall, Phase: int(PhasePlanning)},
		{TurnType: TurnTypeFunctionalCall, Phase: int(PhaseExecuting)},
		{TurnType: TurnTypeToolCall, Phase: int(PhasePlanning)},
		{TurnType: TurnTypeToolCall, Phase: int(PhaseExecuting)},
	}
	for _, turn := range cases {
		wantType, wantPhase := turn.TurnType, turn.Phase
		NormalizeTurn(turn)
		NormalizeTurn(turn)
		if turn.TurnType != wantType || turn.Phase != wantPhase {
			t.Errorf("normalize changed (%v,%d) → (%v,%d)",
				wantType, wantPhase, turn.TurnType, turn.Phase)
		}
	}
}

// TestNormalizeTurn_PlanningToolCall 规划期调工具做调研是合法组合，
// 归一后必须原样保留 —— 这正是两个维度分开的意义。
func TestNormalizeTurn_PlanningToolCall(t *testing.T) {
	turn := &TaskTurn{TurnType: TurnTypeToolCall, Phase: int(PhasePlanning)}
	NormalizeTurn(turn)
	if turn.TurnType != TurnTypeToolCall || !turn.IsPlanning() {
		t.Errorf("planning tool_call broken: type=%v phase=%v", turn.TurnType, turn.TurnPhase())
	}
}

// TestNormalizeTurn_Nil 不能因为 nil 就 panic —— 调用点分散在多个读路径上
func TestNormalizeTurn_Nil(t *testing.T) {
	if got := NormalizeTurn(nil); got != nil {
		t.Errorf("NormalizeTurn(nil) = %v, want nil", got)
	}
	if got := NormalizeTurns(nil); got != nil {
		t.Errorf("NormalizeTurns(nil) = %v, want nil", got)
	}
}

// TestNormalizeTurns_Batch 批量归一要就地修改并原样返回切片
func TestNormalizeTurns_Batch(t *testing.T) {
	turns := []*TaskTurn{
		{TurnType: TurnTypePlanningLegacy},
		{TurnType: TurnTypeToolCall},
	}
	got := NormalizeTurns(turns)
	if len(got) != 2 {
		t.Fatalf("got %d turns, want 2", len(got))
	}
	if !turns[0].IsPlanning() || turns[0].TurnType != TurnTypeFunctionalCall {
		t.Error("legacy planning not normalized in batch")
	}
	if turns[1].TurnPhase() != PhaseExecuting {
		t.Error("tool_call should default to executing phase")
	}
}

// TestTurnStatusString 验证状态名称
func TestTurnStatusString(t *testing.T) {
	cases := map[TurnStatus]string{
		TurnStatusRunning: "running",
		TurnStatusSuccess: "success",
		TurnStatusFailed:  "failed",
		TurnStatus(99):    "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("TurnStatus(%d).String() = %q, want %q", s, got, want)
		}
	}
}

// TestTaskTurnIsTerminal 验证 turn 终态判定
func TestTaskTurnIsTerminal(t *testing.T) {
	running := &TaskTurn{Status: TurnStatusRunning}
	if running.IsTerminal() {
		t.Error("running turn should not be terminal")
	}
	success := &TaskTurn{Status: TurnStatusSuccess}
	if !success.IsTerminal() {
		t.Error("success turn should be terminal")
	}
	failed := &TaskTurn{Status: TurnStatusFailed}
	if !failed.IsTerminal() {
		t.Error("failed turn should be terminal")
	}
	interrupted := &TaskTurn{Status: TurnStatusInterrupted}
	if !interrupted.IsTerminal() {
		t.Error("interrupted turn should be terminal")
	}
}
