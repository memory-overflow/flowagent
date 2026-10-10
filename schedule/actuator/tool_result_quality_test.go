package actuator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

func TestIsRetrievalTool(t *testing.T) {
	cases := map[string]bool{
		"web_search":     true,
		"fetch__fetch":   true,
		"deep_search":    true,
		"serper__search": true, // 用户自绑的检索 MCP 也适用
		"write_file":     false,
		"run_command":    false,
		"read_file":      false,
	}
	for name, want := range cases {
		if got := isRetrievalTool(name); got != want {
			t.Errorf("isRetrievalTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestNormalizeLowValueToolResult_Empty 空结果必须变成带下一步建议的提示，
// 否则模型只会原样重试（实测 25.7% 的搜索返回 <100B）。
func TestNormalizeLowValueToolResult_Empty(t *testing.T) {
	got := normalizeLowValueToolResult("web_search", "")
	if !strings.Contains(got, "无有效内容") {
		t.Errorf("empty result must be normalized, got %q", got)
	}
	for _, hint := range []string{"更具体", "换一个信息源", "deep_search"} {
		if !strings.Contains(got, hint) {
			t.Errorf("hint must mention %q, got %q", hint, got)
		}
	}

	// 过短但非空：保留原文 + 追加引导（原文要留着，调试页与模型都要看）
	got = normalizeLowValueToolResult("fetch__fetch", "  没有结果  ")
	if !strings.Contains(got, "没有结果") || !strings.Contains(got, "无有效内容") {
		t.Errorf("short result must keep the original and append the hint, got %q", got)
	}
}

func TestNormalizeLowValueToolResult_ValidUntouched(t *testing.T) {
	valid := strings.Repeat("有效内容", 100)
	if got := normalizeLowValueToolResult("web_search", valid); got != valid {
		t.Error("valid result must be returned untouched")
	}
	// 非检索类工具的短回执不能误伤（写入成功这类回执本就只有几十字符）
	short := "写入成功"
	if got := normalizeLowValueToolResult("write_file", short); got != short {
		t.Errorf("non-retrieval short receipt must be untouched, got %q", got)
	}
}

// mkToolTurn 造一条 tool_call turn（content 为空串表示无 output）。
func mkToolTurn(seq int32, tool string, status model.TurnStatus, content string) *model.TaskTurn {
	out := ""
	if content != "" {
		b, err := json.Marshal(model.TurnToolOutput{Content: content})
		if err != nil {
			panic(err)
		}
		out = string(b)
	}
	return &model.TaskTurn{
		TurnSeq:  seq,
		TurnType: model.TurnTypeToolCall,
		ToolName: tool,
		Status:   status,
		Output:   out,
	}
}

func TestLowValueStreakFromTurns(t *testing.T) {
	valid := strings.Repeat("内容", 100)
	cases := []struct {
		name  string
		turns []*model.TaskTurn
		want  int
	}{
		{"全有效", []*model.TaskTurn{
			mkToolTurn(1, "web_search", model.TurnStatusSuccess, valid),
			mkToolTurn(2, "web_search", model.TurnStatusSuccess, valid),
		}, 0},
		{"末尾两条空", []*model.TaskTurn{
			mkToolTurn(1, "web_search", model.TurnStatusSuccess, valid),
			mkToolTurn(2, "web_search", model.TurnStatusSuccess, ""),
			mkToolTurn(3, "fetch__fetch", model.TurnStatusSuccess, ""),
		}, 2},
		{"失败计入", []*model.TaskTurn{
			mkToolTurn(1, "web_search", model.TurnStatusSuccess, valid),
			mkToolTurn(2, "web_search", model.TurnStatusFailed, ""),
			mkToolTurn(3, "web_search", model.TurnStatusSuccess, ""),
		}, 2},
		{"中间有效则重置", []*model.TaskTurn{
			mkToolTurn(1, "web_search", model.TurnStatusSuccess, ""),
			mkToolTurn(2, "web_search", model.TurnStatusSuccess, valid),
			mkToolTurn(3, "web_search", model.TurnStatusSuccess, ""),
		}, 1},
		{"非检索工具的短结果不计", []*model.TaskTurn{
			mkToolTurn(1, "write_file", model.TurnStatusSuccess, "ok"),
			mkToolTurn(2, "run_command", model.TurnStatusSuccess, ""),
		}, 0},
		{"assistant turn 不打断计数", []*model.TaskTurn{
			mkToolTurn(1, "web_search", model.TurnStatusSuccess, ""),
			{TurnSeq: 2, TurnType: model.TurnTypeFunctionalCall, Status: model.TurnStatusSuccess},
			mkToolTurn(3, "web_search", model.TurnStatusSuccess, ""),
		}, 2},
	}
	for _, c := range cases {
		if got := lowValueStreakFromTurns(c.turns); got != c.want {
			t.Errorf("%s: streak = %d, want %d", c.name, got, c.want)
		}
	}
}
