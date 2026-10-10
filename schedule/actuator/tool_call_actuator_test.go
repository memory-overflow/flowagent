package actuator

import (
	"encoding/json"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// parseToolCallsFromTurn 复刻 loadPendingToolCalls 中的解析部分，
// 便于在不接 DB 的情况下验证解析与校验逻辑。
// 若此处逻辑与 loadPendingToolCalls 偏离，说明实现被改动，应同步更新。
func parseToolCallsFromTurn(t *testing.T, turn *model.TaskTurn) (int, bool) {
	t.Helper()
	if turn == nil || turn.Status != model.TurnStatusSuccess || turn.Output == "" {
		return 0, false
	}
	var out model.TurnLLMOutput
	if err := json.Unmarshal([]byte(turn.Output), &out); err != nil {
		return 0, false
	}
	return len(out.ToolCalls), true
}

// TestPendingToolCalls_Valid 正常解析出多个工具调用
func TestPendingToolCalls_Valid(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeFunctionalCall,
		Status:   model.TurnStatusSuccess,
		TurnSeq:  2,
		Output: mustJSON(t, model.TurnLLMOutput{
			ToolCalls: []model.TurnToolCallReq{
				{Name: "srv__read", Arguments: map[string]interface{}{"path": "/a"}},
				{Name: "srv__search", Arguments: map[string]interface{}{"q": "go"}},
			},
		}),
	}
	n, ok := parseToolCallsFromTurn(t, turn)
	if !ok {
		t.Fatal("expected successful parse")
	}
	if n != 2 {
		t.Errorf("tool calls = %d, want 2", n)
	}
}

// TestPendingToolCalls_Rejected 非法 turn 必须被拒绝，
// 避免 tool_call 阶段拿着脏数据空转
func TestPendingToolCalls_Rejected(t *testing.T) {
	cases := []struct {
		name string
		turn *model.TaskTurn
	}{
		{"nil turn", nil},
		{"failed turn", &model.TaskTurn{
			Status: model.TurnStatusFailed,
			Output: mustJSON(t, model.TurnLLMOutput{
				ToolCalls: []model.TurnToolCallReq{{Name: "x"}},
			}),
		}},
		{"running turn", &model.TaskTurn{
			Status: model.TurnStatusRunning,
			Output: mustJSON(t, model.TurnLLMOutput{
				ToolCalls: []model.TurnToolCallReq{{Name: "x"}},
			}),
		}},
		{"empty output", &model.TaskTurn{Status: model.TurnStatusSuccess, Output: ""}},
		{"bad json", &model.TaskTurn{Status: model.TurnStatusSuccess, Output: "{not json"}},
	}
	for _, c := range cases {
		if _, ok := parseToolCallsFromTurn(t, c.turn); ok {
			t.Errorf("%s: expected parse to be rejected", c.name)
		}
	}
}

// TestPendingToolCalls_TextOnly 纯文本回复解析出 0 个工具调用。
// tool_call 阶段据此判失败（不该进到这个阶段）。
func TestPendingToolCalls_TextOnly(t *testing.T) {
	turn := &model.TaskTurn{
		Status: model.TurnStatusSuccess,
		Output: mustJSON(t, model.TurnLLMOutput{Text: "这是最终答案"}),
	}
	n, ok := parseToolCallsFromTurn(t, turn)
	if !ok {
		t.Fatal("expected successful parse")
	}
	if n != 0 {
		t.Errorf("tool calls = %d, want 0", n)
	}
}

// TestBuildToolCallRecord 验证 tool_call turn 转对外记录
func TestBuildToolCallRecord(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType:   model.TurnTypeToolCall,
		Status:     model.TurnStatusSuccess,
		ToolName:   "srv__read",
		ServerName: "srv",
		TimeCostMs: 150,
		Input: mustJSON(t, model.TurnToolInput{
			ToolName:  "srv__read",
			Summary:   "读取 /a 的内容",
			Arguments: map[string]interface{}{"path": "/a"},
		}),
		Output: mustJSON(t, model.TurnToolOutput{Content: "file content"}),
	}
	rec := buildToolCallRecord(turn)

	if rec.ToolName != "srv__read" {
		t.Errorf("tool name = %q", rec.ToolName)
	}
	if rec.ServerName != "srv" {
		t.Errorf("server name = %q", rec.ServerName)
	}
	if rec.Output != "file content" {
		t.Errorf("output = %q", rec.Output)
	}
	if rec.Input == "" {
		t.Error("input should carry serialized arguments")
	}
	if rec.TimeCost != "150ms" {
		t.Errorf("time cost = %q, want 150ms", rec.TimeCost)
	}
	if rec.Summary != "读取 /a 的内容" {
		t.Errorf("summary = %q, want model summary", rec.Summary)
	}
}

// TestBuildToolCallRecord_NoSummary 验证 TurnToolInput 没填 summary 时
// rec.Summary 为空（前端降级为 server/tool_name 拼接），不会误传脏值。
func TestBuildToolCallRecord_NoSummary(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType:   model.TurnTypeToolCall,
		Status:     model.TurnStatusSuccess,
		ToolName:   "srv__read",
		ServerName: "srv",
		Input: mustJSON(t, model.TurnToolInput{
			ToolName:  "srv__read",
			Arguments: map[string]interface{}{"path": "/a"},
		}),
		Output: mustJSON(t, model.TurnToolOutput{Content: "ok"}),
	}
	rec := buildToolCallRecord(turn)
	if rec.Summary != "" {
		t.Errorf("summary = %q, want empty (frontend will fallback to server/tool name)", rec.Summary)
	}
}

// TestBuildToolCallRecord_SummaryTrimmed 验证 summary 两端空白会被 trim，
// 避免前端把空白当有效说明展示。
func TestBuildToolCallRecord_SummaryTrimmed(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusSuccess,
		ToolName: "srv__read",
		Input: mustJSON(t, model.TurnToolInput{
			ToolName:  "srv__read",
			Summary:   "   读取文件   ",
			Arguments: map[string]interface{}{"path": "/a"},
		}),
		Output: mustJSON(t, model.TurnToolOutput{Content: "ok"}),
	}
	rec := buildToolCallRecord(turn)
	if rec.Summary != "读取文件" {
		t.Errorf("summary = %q, want trimmed 读取文件", rec.Summary)
	}
}

// TestBuildToolCallRecord_FailedUsesErrorMsg 失败且无输出时用 error_msg 填充，
// 保证前端能看到失败原因
func TestBuildToolCallRecord_FailedUsesErrorMsg(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeToolCall,
		Status:   model.TurnStatusFailed,
		ToolName: "srv__read",
		ErrorMsg: "connection refused",
	}
	rec := buildToolCallRecord(turn)
	if rec.Output != "connection refused" {
		t.Errorf("output = %q, want error msg", rec.Output)
	}
}

// TestExtractToolSummary 验证从工具入参提取并剥离 summary 元参数。
func TestExtractToolSummary(t *testing.T) {
	// nil args
	if s, clean := extractToolSummary(nil); s != "" || clean != nil {
		t.Errorf("nil args: summary=%q clean=%v, want empty/nil", s, clean)
	}

	// 无 summary：返回原引用，summary 为空
	plain := map[string]interface{}{"path": "/a"}
	if s, clean := extractToolSummary(plain); s != "" || clean == nil || clean["path"] != "/a" {
		t.Errorf("no summary: summary=%q clean=%v", s, clean)
	}

	// 有 summary：提取并剥离，原 map 不变
	withSum := map[string]interface{}{"path": "/a", summaryParamKey: "  读取该文件  "}
	s, clean := extractToolSummary(withSum)
	if s != "读取该文件" {
		t.Errorf("summary = %q, want trimmed %q", s, "读取该文件")
	}
	if _, ok := clean[summaryParamKey]; ok {
		t.Errorf("clean args should not contain summary, got %v", clean)
	}
	if clean["path"] != "/a" {
		t.Errorf("clean args lost path: %v", clean)
	}
	if _, still := withSum[summaryParamKey]; !still {
		t.Error("original args must not be mutated")
	}

	// summary 非 string：剥离但不作为 summary
	bad := map[string]interface{}{"cmd": "ls", summaryParamKey: 123}
	s, clean = extractToolSummary(bad)
	if s != "" {
		t.Errorf("non-string summary = %q, want empty", s)
	}
	if _, ok := clean[summaryParamKey]; ok {
		t.Errorf("clean args should not contain summary, got %v", clean)
	}
}

// TestToolCallsFingerprint_ExcludesSummary 死循环指纹必须排除 summary，
// 否则模型每轮重写的说明文本会让「重复调用」永远无法被识别。
func TestToolCallsFingerprint_ExcludesSummary(t *testing.T) {
	a := toolCallsFingerprint([]llm.ToolCall{
		{Name: "run_command", Arguments: map[string]interface{}{"cmd": "go test ./...", summaryParamKey: "跑一遍测试"}},
	})
	b := toolCallsFingerprint([]llm.ToolCall{
		{Name: "run_command", Arguments: map[string]interface{}{"cmd": "go test ./...", summaryParamKey: "验证编译通过"}},
	})
	if a != b {
		t.Errorf("fingerprint should ignore summary: a=%q b=%q", a, b)
	}
	if a == "" {
		t.Error("fingerprint should not be empty")
	}
}
