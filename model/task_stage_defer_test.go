package model

import "testing"

// ============================================================
// 延后重排（Deferred）的状态机流转
// ============================================================

// TestNextStage_Deferred 延后重排必须回到本阶段的 wait，而不是推进或收口。
//
// 这是「压缩要等前置任务」能不能成立的关键：本轮什么都没执行（没调 LLM、
// 没写 turn），若按正常成功路径流转，规划期会被拨到 finished 或 tool_call，
// 用户的问题就此消失；若判失败，又会消耗重试次数最终把任务判死。
func TestNextStage_Deferred(t *testing.T) {
	cases := []struct {
		name  string
		stage TaskStage
		want  TaskStage
	}{
		{"functional_call", StageFunctionalCallDoing, StageFunctionalCallWait},
		{"tool_call", StageToolCallDoing, StageToolCallWait},
		{"workflow", StageWorkflowDoing, StageWorkflowWait},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.stage.NextStage(&StageTransitionHint{Deferred: true})
			if got != c.want {
				t.Fatalf("stage %s with Deferred → %s, want %s", c.stage, got, c.want)
			}
		})
	}

	// 延后期间对外状态仍是 queued（前端看到的是「执行中」，不是完成也不是失败）
	tr := StageFunctionalCallDoing.Transit(ActionSuccess, &StageTransitionHint{Deferred: true})
	if tr.Status != TaskStatusQueued || tr.IsTerminal {
		t.Fatalf("Deferred 流转 status=%v terminal=%v, want queued/false", tr.Status, tr.IsTerminal)
	}
}

// TestNextStage_DeferredBeatsOtherHints 延后优先于其它流转决策。
//
// 同一轮里 hint 可能同时带着 NeedToolCall / PlanSubmitted（它们在内存 hint
// 里跨轮保留），若被那些分支先截住，任务会带着「本轮没执行」的事实被推进。
func TestNextStage_DeferredBeatsOtherHints(t *testing.T) {
	hint := &StageTransitionHint{
		Deferred:      true,
		NeedToolCall:  true,
		PlanSubmitted: true,
	}
	if got := StageFunctionalCallDoing.NextStage(hint); got != StageFunctionalCallWait {
		t.Fatalf("Deferred + NeedToolCall → %s, want functional_call_wait", got)
	}
}

// TestNextStage_WithoutDeferred 未延后时行为不变（回归保护）。
func TestNextStage_WithoutDeferred(t *testing.T) {
	if got := StageFunctionalCallDoing.NextStage(&StageTransitionHint{NeedToolCall: true}); got != StageToolCallWait {
		t.Fatalf("NeedToolCall → %s, want tool_call_wait", got)
	}
	if got := StageToolCallDoing.NextStage(nil); got != StageFunctionalCallWait {
		t.Fatalf("tool_call_doing → %s, want functional_call_wait", got)
	}
}
