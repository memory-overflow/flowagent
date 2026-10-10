package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestDirectReply_DecodeAndValidate direct_reply 的 reply 要能被自定义 UnmarshalJSON 解出，
// 空 reply 判为普通协议违规（非 replanError）→ 调用方降级 single_agent。
func TestDirectReply_DecodeAndValidate(t *testing.T) {
	ok := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":   model.PlanModeDirectReply,
		"reason": "寒暄，无需执行",
		"reply":  "你好！有什么可以帮你？",
	}}
	resp, target, err := decodeAndValidatePlan(ok, &workflowContext{}, false)
	if err != nil {
		t.Fatalf("valid direct_reply rejected: %v", err)
	}
	if resp.Reply != "你好！有什么可以帮你？" {
		t.Errorf("reply lost in decode: %q", resp.Reply)
	}
	if target != nil {
		t.Errorf("direct_reply should not resolve a workflow target, got %+v", target)
	}

	empty := &llm.ToolCall{Name: model.SubmitPlanToolName, Arguments: map[string]interface{}{
		"mode":   model.PlanModeDirectReply,
		"reason": "x",
		"reply":  "   ",
	}}
	_, _, err = decodeAndValidatePlan(empty, &workflowContext{}, false)
	if err == nil {
		t.Fatal("empty reply should be rejected")
	}
	if _, isReplan := err.(*replanError); isReplan {
		t.Error("empty reply should degrade to single_agent, not trigger replan")
	}
}

// TestPlanModeFinishesTask 只有 single_agent 要进执行期。
func TestPlanModeFinishesTask(t *testing.T) {
	cases := map[string]bool{
		model.PlanModeDirectReply: true,
		model.PlanModeWorkflow:    true,
		model.PlanModeSingleAgent: false,
		"":                        false,
	}
	for mode, want := range cases {
		if got := model.PlanModeFinishesTask(mode); got != want {
			t.Errorf("PlanModeFinishesTask(%q) = %v, want %v", mode, got, want)
		}
	}
}

// TestPlanningTurnToMessage_DirectReplyEntersHistory direct_reply 的答案只在 submit_plan
// 参数里；入历史时必须还原成 assistant 文本，否则下一轮模型看不到自己的回答。
// submit_plan 调用本身仍要剔除（执行期没有该工具，回放会 400）。
func TestPlanningTurnToMessage_DirectReplyEntersHistory(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeFunctionalCall,
		Phase:    int(model.PhasePlanning),
		Status:   model.TurnStatusSuccess,
		Output: mustJSON(t, model.TurnLLMOutput{
			ToolCalls: []model.TurnToolCallReq{{
				Name: model.SubmitPlanToolName,
				Arguments: map[string]interface{}{
					"mode":  model.PlanModeDirectReply,
					"reply": "TCP 三次握手是……",
				},
			}},
		}),
	}
	msg, ok := turnToMessage(turn)
	if !ok {
		t.Fatal("direct_reply planning turn should enter history")
	}
	if msg.Role != llm.RoleAssistant || !strings.Contains(msg.Content, "三次握手") {
		t.Errorf("want assistant message carrying the reply, got %+v", msg)
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("submit_plan must be stripped from history, got %+v", msg.ToolCalls)
	}
}

// TestRenderPlanContext_DirectReplyNotInjected direct_reply 任务不会进执行期，
// 即便状态异常走到执行期，也不应把它当作单 Agent 计划注入。
func TestRenderPlanContext_DirectReplyNotInjected(t *testing.T) {
	planJSON := mustJSON(t, planningLLMResp{Mode: model.PlanModeDirectReply, Reason: "闲聊"})
	if got := renderPlanContext(planJSON); got != "" {
		t.Errorf("direct_reply plan should not render execution context, got %q", got)
	}
}
