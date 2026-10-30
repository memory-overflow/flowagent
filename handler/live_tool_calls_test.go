package handler

import (
	"encoding/json"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

func TestToolCallRecordsFromTurnsIncludesRunningAndCompletedCalls(t *testing.T) {
	args, _ := json.Marshal(model.TurnToolInput{
		ToolName:   "run_command",
		ServerName: "builtin",
		Arguments:  map[string]interface{}{"cmd": "go test ./..."},
		Summary:    "运行后端测试",
	})
	result, _ := json.Marshal(model.TurnToolOutput{Content: "ok"})
	turns := []*model.TaskTurn{
		{TurnType: model.TurnTypeFunctionalCall, Status: model.TurnStatusSuccess},
		{TurnType: model.TurnTypeToolCall, ToolName: "run_command", ServerName: "builtin", Input: string(args), Status: model.TurnStatusRunning},
		{TurnType: model.TurnTypeToolCall, Input: string(args), Output: string(result), Status: model.TurnStatusSuccess, TimeCostMs: 125},
		{TurnType: model.TurnTypeToolCall, Input: string(args), Status: model.TurnStatusInterrupted},
	}

	got := toolCallRecordsFromTurns(turns)
	if len(got) != 2 {
		t.Fatalf("tool call count = %d, want 2: %#v", len(got), got)
	}
	if got[0].ToolName != "run_command" || got[0].ServerName != "builtin" || got[0].Status != int(model.TurnStatusRunning) {
		t.Fatalf("running call mismatch: %#v", got[0])
	}
	if got[0].Summary != "运行后端测试" || got[0].Input != `{"cmd":"go test ./..."}` {
		t.Fatalf("running call details mismatch: %#v", got[0])
	}
	if got[1].Status != int(model.TurnStatusSuccess) || got[1].Output != "ok" || got[1].TimeCost != "125ms" {
		t.Fatalf("completed call mismatch: %#v", got[1])
	}
}
