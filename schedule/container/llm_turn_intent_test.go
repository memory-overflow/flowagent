package container

import (
	"encoding/json"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// TestLLMTurnIntent_DirectReplyFinishes 进程重启后从 DB 恢复 hint 时，
// direct_reply 必须和 workflow 一样判为收口（PlanDelegated），
// 否则任务会被拉回执行期再答一遍，用户看到两份回复。
func TestLLMTurnIntent_DirectReplyFinishes(t *testing.T) {
	mk := func(mode string) *model.TaskTurn {
		out, _ := json.Marshal(model.TurnLLMOutput{
			ToolCalls: []model.TurnToolCallReq{{
				Name:      model.SubmitPlanToolName,
				Arguments: map[string]interface{}{"mode": mode, "reply": "hi"},
			}},
		})
		return &model.TaskTurn{Output: string(out)}
	}

	cases := []struct {
		mode                       string
		delegated, submitted, tool bool
	}{
		{model.PlanModeDirectReply, true, false, false},
		{model.PlanModeWorkflow, true, false, false},
		{model.PlanModeSingleAgent, false, true, false},
	}
	for _, c := range cases {
		d, s, n := llmTurnIntent(mk(c.mode))
		if d != c.delegated || s != c.submitted || n != c.tool {
			t.Errorf("mode=%s: got (%v,%v,%v) want (%v,%v,%v)",
				c.mode, d, s, n, c.delegated, c.submitted, c.tool)
		}
	}
}
