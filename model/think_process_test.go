package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustJSONStr(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestBuildThinkProcess_FullFlow 完整 agent loop 的过程描述：
// planning → LLM 请求工具 → 工具执行 → LLM 给出答案
func TestBuildThinkProcess_FullFlow(t *testing.T) {
	turns := []*TaskTurn{
		{
			TurnSeq: 1, TurnType: TurnTypeFunctionalCall, Phase: int(PhasePlanning), Status: TurnStatusSuccess, TimeCostMs: 800,
			Output: mustJSONStr(t, TurnLLMOutput{
				ToolCalls: []TurnToolCallReq{{
					Name: "submit_plan",
					Arguments: map[string]interface{}{
						"mode":   "single_agent",
						"reason": "问题简单，单轮即可",
					},
				}},
			}),
		},
		{
			TurnSeq: 2, TurnType: TurnTypeFunctionalCall, Status: TurnStatusSuccess, TimeCostMs: 1500,
			Output: mustJSONStr(t, TurnLLMOutput{
				ToolCalls: []TurnToolCallReq{{Name: "fs__read_file"}},
			}),
		},
		{
			TurnSeq: 3, TurnType: TurnTypeToolCall, Status: TurnStatusSuccess, TimeCostMs: 120,
			ToolName: "fs__read_file", ServerName: "Filesystem",
			Input:  mustJSONStr(t, TurnToolInput{ToolName: "fs__read_file", Arguments: map[string]interface{}{"path": "/a.txt"}}),
			Output: mustJSONStr(t, TurnToolOutput{Content: "hello world"}),
		},
		{
			TurnSeq: 4, TurnType: TurnTypeFunctionalCall, Status: TurnStatusSuccess, TimeCostMs: 900,
			Output: mustJSONStr(t, TurnLLMOutput{Text: "文件内容是 hello world"}),
		},
	}

	steps := BuildThinkProcess(turns)
	if len(steps) != 4 {
		t.Fatalf("steps = %d, want 4:\n%s", len(steps), strings.Join(steps, "\n"))
	}

	// 顺序必须与 turn_seq 一致
	for i, prefix := range []string{"#1", "#2", "#3", "#4"} {
		if !strings.HasPrefix(steps[i], prefix) {
			t.Errorf("step %d = %q, want prefix %s", i, steps[i], prefix)
		}
	}
	// 各步关键信息
	if !strings.Contains(steps[0], "单 Agent") || !strings.Contains(steps[0], "问题简单") {
		t.Errorf("planning step = %q", steps[0])
	}
	if !strings.Contains(steps[1], "fs__read_file") {
		t.Errorf("llm tool-call step = %q", steps[1])
	}
	if !strings.Contains(steps[2], "Filesystem") || !strings.Contains(steps[2], "hello world") {
		t.Errorf("tool step = %q", steps[2])
	}
	if !strings.Contains(steps[3], "hello world") {
		t.Errorf("final answer step = %q", steps[3])
	}
	// 耗时格式：>=1000ms 转秒
	if !strings.Contains(steps[1], "1.5s") {
		t.Errorf("step %q should show cost as 1.5s", steps[1])
	}
	if !strings.Contains(steps[2], "120ms") {
		t.Errorf("step %q should show cost as 120ms", steps[2])
	}
}

// TestBuildThinkProcess_WorkflowMode workflow 模式带出计划摘要
func TestBuildThinkProcess_WorkflowMode(t *testing.T) {
	turns := []*TaskTurn{{
		TurnSeq: 1, TurnType: TurnTypeFunctionalCall, Phase: int(PhasePlanning), Status: TurnStatusSuccess,
		Output: mustJSONStr(t, TurnLLMOutput{
			ToolCalls: []TurnToolCallReq{{
				Name: "submit_plan",
				Arguments: map[string]interface{}{
					"mode":         "workflow",
					"reason":       "任务复杂",
					"plan_summary": "检索→归纳→撰写",
				},
			}},
		}),
	}}
	steps := BuildThinkProcess(turns)
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if !strings.Contains(steps[0], "工作流") {
		t.Errorf("step = %q, want workflow mode", steps[0])
	}
	if !strings.Contains(steps[0], "检索→归纳→撰写") {
		t.Errorf("step = %q, want plan summary", steps[0])
	}
}

// TestBuildThinkProcess_FailedTurns 失败的 turn 必须展示，
// 否则用户不知道中间哪一步断了
func TestBuildThinkProcess_FailedTurns(t *testing.T) {
	turns := []*TaskTurn{
		{
			TurnSeq: 1, TurnType: TurnTypeToolCall, Status: TurnStatusFailed,
			ToolName: "fs__read_file", ErrorMsg: "permission denied",
		},
		{
			TurnSeq: 2, TurnType: TurnTypeFunctionalCall, Status: TurnStatusFailed,
			ErrorMsg: "llm timeout",
		},
		{
			TurnSeq: 3, TurnType: TurnTypeFunctionalCall, Phase: int(PhasePlanning), Status: TurnStatusFailed,
			ErrorMsg: "invalid plan",
		},
	}
	steps := BuildThinkProcess(turns)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	for i, want := range []string{"permission denied", "llm timeout", "invalid plan"} {
		if !strings.Contains(steps[i], want) {
			t.Errorf("step %d = %q, want to contain %q", i, steps[i], want)
		}
		if !strings.Contains(steps[i], "失败") {
			t.Errorf("step %d = %q, should be marked as failure", i, steps[i])
		}
	}
}

// TestBuildThinkProcess_KeepRunning 执行中的 turn 保留在过程描述里并带「已耗时」。
//
// 早期口径是跳过 running turn，结果用户只能看到「已完成的部分 + 一个光秃秃的
// 进行中标题」，无从判断当前卡在哪一步。现在改为保留，只是描述换成
// 「正在调用工具 x」，耗时用 StartTime 现算（TimeCostMs 要收口才回填，此刻恒为 0）。
func TestBuildThinkProcess_KeepRunning(t *testing.T) {
	turns := []*TaskTurn{
		{TurnSeq: 1, TurnType: TurnTypeFunctionalCall, Status: TurnStatusSuccess,
			Output: mustJSONStr(t, TurnLLMOutput{Text: "done"})},
		{TurnSeq: 2, TurnType: TurnTypeToolCall, Status: TurnStatusRunning, ToolName: "x"},
	}
	steps := BuildThinkProcess(turns)
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2 (running turn should be kept)", len(steps))
	}
	if !strings.Contains(steps[1], "x") {
		t.Errorf("running step = %q, want to mention tool name", steps[1])
	}
	// 不能按「已完成」模板写 —— 那会写出与事实相反的「成功，无返回内容」
	if strings.Contains(steps[1], "成功") || strings.Contains(steps[1], "失败") {
		t.Errorf("running step = %q, should not claim success or failure", steps[1])
	}
}

// TestBuildThinkProcessLayered_RunningFlag 分层输出时 running 标记要透传出去
func TestBuildThinkProcessLayered_RunningFlag(t *testing.T) {
	turns := []*TaskTurn{
		{TurnSeq: 1, TurnType: TurnTypeToolCall, Status: TurnStatusSuccess, ToolName: "a"},
		{TurnSeq: 2, TurnType: TurnTypeToolCall, Status: TurnStatusRunning, ToolName: "b"},
	}
	layers := BuildThinkProcessLayered(turns)
	if len(layers.Execution) != 2 {
		t.Fatalf("execution steps = %d, want 2", len(layers.Execution))
	}
	if layers.Execution[0].Running {
		t.Error("finished turn should not be marked running")
	}
	if !layers.Execution[1].Running {
		t.Error("running turn should be marked running")
	}
}

// TestBuildThinkProcess_SkipInterrupted 被中断的 turn 仍然跳过
//
// 中断 = 进程重启等原因收口，没有完整结果，且通常会被重试后的新 turn 取代，
// 展示出来只会造成重复与误解。这与「保留 running」是两条不同的规则。
func TestBuildThinkProcess_SkipInterrupted(t *testing.T) {
	turns := []*TaskTurn{
		{TurnSeq: 1, TurnType: TurnTypeFunctionalCall, Status: TurnStatusSuccess,
			Output: mustJSONStr(t, TurnLLMOutput{Text: "done"})},
		{TurnSeq: 2, TurnType: TurnTypeToolCall, Status: TurnStatusInterrupted, ToolName: "x"},
	}
	steps := BuildThinkProcess(turns)
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1 (interrupted turn should be skipped)", len(steps))
	}
}

// TestBuildThinkSteps_RunningElapsed 执行中的 turn 用 StartTime 现算已耗时。
//
// TimeCostMs 要等 turn 收口才回填，执行中恒为 0；直接显示 0ms 会让人误以为
// 刚起步，而「已 3.0s」「已 2m10s」才是用户判断有没有卡住的依据。
func TestBuildThinkSteps_RunningElapsed(t *testing.T) {
	turns := []*TaskTurn{{
		TurnSeq:   1,
		TurnType:  TurnTypeToolCall,
		Status:    TurnStatusRunning,
		ToolName:  "deep_search",
		StartTime: time.Now().Add(-3 * time.Second),
	}}
	steps := BuildThinkProcess(turns)
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if strings.Contains(steps[0], "0ms") {
		t.Errorf("step = %q, running turn must not show 0ms cost", steps[0])
	}
	if !strings.Contains(steps[0], "已 3.0s") {
		t.Errorf("step = %q, want elapsed time '已 3.0s'", steps[0])
	}

	// StartTime 缺失（手工构造 / 历史数据）时退化为定性文案，不出现负数
	noStart := BuildThinkProcess([]*TaskTurn{{
		TurnSeq: 1, TurnType: TurnTypeToolCall, Status: TurnStatusRunning, ToolName: "x",
	}})
	if len(noStart) != 1 || !strings.Contains(noStart[0], "进行中") {
		t.Errorf("step = %q, want fallback '进行中'", noStart)
	}
}

// TestThinkStep_RunningBackwardCompat 老数据没有 running 字段，
// 反序列化后必须是 false（视为终态），不能让历史步骤亮起「进行中」。
func TestThinkStep_RunningBackwardCompat(t *testing.T) {
	var s ThinkStep
	if err := json.Unmarshal([]byte(`{"summary":"#1 调用工具 x（1.0s）","detail":"d"}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Running {
		t.Error("legacy step without running should default to false")
	}
	// 旧版纯字符串形态同样为 false
	var s2 ThinkStep
	if err := json.Unmarshal([]byte(`"#1 规划"`), &s2); err != nil {
		t.Fatalf("unmarshal legacy string: %v", err)
	}
	if s2.Running {
		t.Error("legacy string step should default to false")
	}
	// 新数据能正常序列化出该字段
	b, _ := json.Marshal(ThinkStep{Summary: "s", Running: true})
	if !strings.Contains(string(b), `"running":true`) {
		t.Errorf("marshal = %s, want running field", b)
	}
	// 终态 step 不该带出该字段（omitempty）
	b2, _ := json.Marshal(ThinkStep{Summary: "s"})
	if strings.Contains(string(b2), "running") {
		t.Errorf("marshal = %s, finished step should omit running", b2)
	}
}

// TestBuildThinkProcess_Empty 空输入
func TestBuildThinkProcess_Empty(t *testing.T) {
	if got := BuildThinkProcess(nil); got != nil {
		t.Errorf("nil turns → %v, want nil", got)
	}
	if got := BuildThinkProcess([]*TaskTurn{}); got != nil {
		t.Errorf("empty turns → %v, want nil", got)
	}
}

// TestDescribePlanningTurn_DirectReply 思考过程里要能看出这轮是「直接回复」，
// 而不是落到兜底的「任务规划完成」。
func TestDescribePlanningTurn_DirectReply(t *testing.T) {
	turn := &TaskTurn{
		TurnType: TurnTypeFunctionalCall, Phase: int(PhasePlanning), Status: TurnStatusSuccess,
		Output: mustJSONStr(t, TurnLLMOutput{
			ToolCalls: []TurnToolCallReq{{Name: SubmitPlanToolName, Arguments: map[string]interface{}{
				"mode": PlanModeDirectReply, "reason": "常识问答", "reply": "……",
			}}},
		}),
	}
	line, _ := describePlanningTurn(turn)
	if line != "任务规划：无需执行动作，直接回复；理由：常识问答" {
		t.Errorf("describePlanningTurn = %q", line)
	}
}

// TestBuildThinkProcessLayered 分层：规划期进 planning、执行器进 execution，
// 且平铺 think_process 仍保持 turn_seq 顺序。
func TestBuildThinkProcessLayered(t *testing.T) {
	turns := []*TaskTurn{
		{
			TurnSeq: 1, TurnType: TurnTypeFunctionalCall, Phase: int(PhasePlanning), Status: TurnStatusSuccess,
			Output: mustJSONStr(t, TurnLLMOutput{
				ToolCalls: []TurnToolCallReq{{Name: "submit_plan", Arguments: map[string]interface{}{"mode": "single_agent", "reason": "简单"}}},
			}),
		},
		{
			TurnSeq: 2, TurnType: TurnTypeFunctionalCall, Status: TurnStatusSuccess,
			Output: mustJSONStr(t, TurnLLMOutput{ToolCalls: []TurnToolCallReq{{Name: "fs__read"}}}),
		},
		{
			TurnSeq: 3, TurnType: TurnTypeToolCall, Status: TurnStatusSuccess,
			ToolName: "fs__read", ServerName: "Filesystem",
		},
	}

	layers := BuildThinkProcessLayered(turns)
	if len(layers.Planning) != 1 {
		t.Fatalf("planning = %d, want 1:\n%v", len(layers.Planning), layers.Planning)
	}
	if len(layers.Execution) != 2 {
		t.Fatalf("execution = %d, want 2:\n%v", len(layers.Execution), layers.Execution)
	}
	if !strings.Contains(layers.Planning[0].Summary, "任务规划") {
		t.Errorf("planning[0] = %q, want planning desc", layers.Planning[0].Summary)
	}

	// FillThinkProcess 同时填充两层，平铺仍按 turn_seq 顺序
	var out AgentOutputData
	if !out.FillThinkProcess(turns) {
		t.Fatal("FillThinkProcess returned false, want true")
	}
	if out.ThinkLayers == nil {
		t.Fatal("ThinkLayers should be non-nil")
	}
	if len(out.ThinkLayers.Planning) != 1 || len(out.ThinkLayers.Execution) != 2 {
		t.Fatalf("layers = %+v, want 1 planning + 2 execution", out.ThinkLayers)
	}
	if len(out.ThinkProcess) != 3 {
		t.Fatalf("think_process = %d, want 3", len(out.ThinkProcess))
	}
	for i, prefix := range []string{"#1", "#2", "#3"} {
		if !strings.HasPrefix(out.ThinkProcess[i], prefix) {
			t.Errorf("think_process[%d] = %q, want prefix %s", i, out.ThinkProcess[i], prefix)
		}
	}

	// 工具调用 turn 应是结构化 summary（无模型说明时退化为「调用工具 X」），
	// 且 detail 带完整工具执行信息。
	exec := out.ThinkLayers.Execution
	if !strings.Contains(exec[1].Summary, "调用工具") {
		t.Errorf("tool step summary = %q, want fallback '调用工具 X'", exec[1].Summary)
	}
	if exec[1].Detail == "" {
		t.Errorf("tool step detail should be non-empty")
	}

	// JSON 序列化字段名与 omitempty 行为
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"think_layers"`) || !strings.Contains(s, `"planning"`) || !strings.Contains(s, `"execution"`) {
		t.Errorf("json missing layered fields: %s", s)
	}
	if !strings.Contains(s, `"summary"`) {
		t.Errorf("json missing structured summary field: %s", s)
	}
}

// TestFillThinkProcess_Empty 无有效条目时不产出分层、返回 false
func TestFillThinkProcess_Empty(t *testing.T) {
	var out AgentOutputData
	if out.FillThinkProcess(nil) {
		t.Error("FillThinkProcess(nil) = true, want false")
	}
	if out.ThinkLayers != nil {
		t.Error("ThinkLayers should stay nil for empty input")
	}
}

// TestBuildThinkProcess_ToolNoOutput 工具无返回内容也要有可读描述
func TestBuildThinkProcess_ToolNoOutput(t *testing.T) {
	turns := []*TaskTurn{{
		TurnSeq: 1, TurnType: TurnTypeToolCall, Status: TurnStatusSuccess,
		ToolName: "fs__noop",
		Output:   mustJSONStr(t, TurnToolOutput{Content: ""}),
	}}
	steps := BuildThinkProcess(turns)
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if !strings.Contains(steps[0], "无返回内容") {
		t.Errorf("step = %q, want hint for empty output", steps[0])
	}
}

// TestBuildThinkProcess_ToolSummary 工具调用带模型说明时，默认展示说明、点击展开完整执行信息。
func TestBuildThinkProcess_ToolSummary(t *testing.T) {
	turns := []*TaskTurn{{
		TurnSeq: 1, TurnType: TurnTypeToolCall, Status: TurnStatusSuccess, TimeCostMs: 120,
		ToolName: "run_command", ServerName: "builtin",
		Input: mustJSONStr(t, TurnToolInput{
			ToolName:   "run_command",
			ServerName: "builtin",
			Summary:    "我先查看当前目录下的文件列表",
			Arguments:  map[string]interface{}{"command": "ls -la"},
		}),
		Output: mustJSONStr(t, TurnToolOutput{Content: "total 4\n-rw-r--r-- 1 user staff 0 ..."}),
	}}

	layers := BuildThinkProcessLayered(turns)
	if len(layers.Execution) != 1 {
		t.Fatalf("execution = %d, want 1", len(layers.Execution))
	}
	step := layers.Execution[0]

	// 默认展示模型说明，而不是「调用工具 run_command」
	if !strings.Contains(step.Summary, "我先查看当前目录下的文件列表") {
		t.Errorf("summary = %q, want model summary", step.Summary)
	}
	// 点击展开的 detail 带完整工具名、server、参数与返回
	if !strings.Contains(step.Detail, "run_command") ||
		!strings.Contains(step.Detail, "builtin") ||
		!strings.Contains(step.Detail, "ls -la") {
		t.Errorf("detail = %q, want full tool info", step.Detail)
	}

	// 平铺 think_process 仍保留完整文本（兼容旧前端 / 降级）
	flat := BuildThinkProcess(turns)
	if len(flat) != 1 || !strings.Contains(flat[0], "run_command") {
		t.Errorf("flat = %v, want full text with tool name", flat)
	}
}

// TestAgentOutput_BackwardCompatThinkLayers 老任务落库的 think_layers 还是
// 字符串数组（[]string），升级后反序列化不能因类型不匹配报错，否则 reply_text
// 等字段会被整体丢弃。
func TestAgentOutput_BackwardCompatThinkLayers(t *testing.T) {
	raw := `{
		"reply_text": "这是最终答案",
		"think_process": ["#1 规划", "#2 调用工具 run_command"],
		"think_layers": {
			"planning": ["#1 规划前调研，调用工具：run_command（5.3s）"],
			"execution": ["#2 调用工具 run_command（来自 builtin），参数：{\"cmd\":\"ls\"} → 返回：...（596ms）"]
		}
	}`
	var out AgentOutputData
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal legacy think_layers failed: %v", err)
	}
	if out.ReplyText != "这是最终答案" {
		t.Errorf("reply_text = %q, want 这是最终答案", out.ReplyText)
	}
	if out.ThinkLayers == nil {
		t.Fatal("think_layers should be parsed")
	}
	if len(out.ThinkLayers.Planning) != 1 || len(out.ThinkLayers.Execution) != 1 {
		t.Fatalf("layers = %+v, want 1 planning + 1 execution", out.ThinkLayers)
	}
	// 旧字符串被当作 summary 全文，detail 为空
	if out.ThinkLayers.Planning[0].Summary == "" || out.ThinkLayers.Planning[0].Detail != "" {
		t.Errorf("planning[0] = %+v, want summary from string + empty detail", out.ThinkLayers.Planning[0])
	}
	if !strings.Contains(out.ThinkLayers.Execution[0].Summary, "run_command") {
		t.Errorf("execution[0].summary = %q, want contains run_command", out.ThinkLayers.Execution[0].Summary)
	}

	// 新对象数组形态也应能正常解析
	mixed := `{"reply_text":"x","think_layers":{"execution":[{"summary":"s","detail":"d"}]}}`
	var out2 AgentOutputData
	if err := json.Unmarshal([]byte(mixed), &out2); err != nil {
		t.Fatalf("unmarshal new think_layers failed: %v", err)
	}
	if out2.ThinkLayers.Execution[0].Summary != "s" || out2.ThinkLayers.Execution[0].Detail != "d" {
		t.Errorf("new shape parsed wrong: %+v", out2.ThinkLayers.Execution[0])
	}
}

// TestTruncateThink 必须按字符截断，避免把中文切成乱码
func TestTruncateThink(t *testing.T) {
	if got := truncateThink("短文本", 100); got != "短文本" {
		t.Errorf("short text should be unchanged, got %q", got)
	}

	long := strings.Repeat("中", 50)
	got := truncateThink(long, 10)
	trimmed := strings.TrimSuffix(got, "...")
	if len([]rune(trimmed)) != 10 {
		t.Errorf("truncated to %d runes, want 10", len([]rune(trimmed)))
	}
	// 截断结果必须是合法 UTF-8（不含替换字符）
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
}

// TestBuildThinkProcess_LongContentTruncated 长内容被截断，
// 避免把大段 JSON 灌到前端
func TestBuildThinkProcess_LongContentTruncated(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	turns := []*TaskTurn{{
		TurnSeq: 1, TurnType: TurnTypeToolCall, Status: TurnStatusSuccess,
		ToolName: "fs__read",
		Output:   mustJSONStr(t, TurnToolOutput{Content: huge}),
	}}
	steps := BuildThinkProcess(turns)
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if len(steps[0]) > 600 {
		t.Errorf("step length = %d, should be truncated", len(steps[0]))
	}
	if !strings.HasSuffix(steps[0], "...（0ms）") && !strings.Contains(steps[0], "...") {
		t.Errorf("step = %q, should contain truncation marker", steps[0][:80])
	}
}
