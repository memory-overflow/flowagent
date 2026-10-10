// Package model — think_process 构建
//
// 把任务执行过程中的每个 turn（planning / functional_call / tool_call）
// 转成人类可读的过程描述，汇总进 AgentOutputData.ThinkProcess，
// 供前端「思考过程」折叠面板展示。
//
// 放在 model 包而非 actuator：任务成功时由 actuator 写入，
// 任务失败时由 container 补写，两者都要用，放这里避免循环依赖。
//
// 设计取向：
//   - 面向人阅读，不是给机器解析 —— 每条一句话说清「做了什么、结果如何」
//   - 长内容一律截断。think_process 是辅助信息，不该把详细 JSON 灌进前端
//   - 失败的 turn 必须展示，用户需要知道中间哪一步出了问题
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// think_process 各字段的截断长度
const (
	thinkMaxPlanReason = 200 // planning 决策理由
	thinkMaxLLMText    = 300 // LLM 回复文本
	thinkMaxToolArgs   = 200 // 工具入参
	thinkMaxToolOutput = 300 // 工具返回
	thinkMaxErrorMsg   = 200 // 错误信息
)

// ThinkStep 一条思考过程的展示单元。
//
// Summary 是默认展示的一句话说明；Detail 是点击展开后的完整信息
// （工具名、入参、返回等）。只有工具调用类 turn 才有独立 Detail，
// 其余 turn 的 Detail 为空，前端不渲染展开交互。
type ThinkStep struct {
	// Summary 默认展示的一句话说明（带 #序号 + 耗时前缀）
	Summary string `json:"summary"`
	// Detail 点击展开后的完整描述；为空表示该条无需展开
	Detail string `json:"detail,omitempty"`
	// Running 该步是否仍在执行中（对应 turn 状态 running）。
	//
	// 任务跑起来后前端每轮都会实时重建 think_layers，若把执行中的 turn
	// 整条丢掉，用户就只能看到「已完成的部分 + 一个光秃秃的进行中标题」，
	// 不知道当前卡在哪一步。这里保留该条并置位，由前端加「进行中」标记。
	// 老数据没有该字段，反序列化后为 false（终态），语义一致。
	Running bool `json:"running,omitempty"`
}

// UnmarshalJSON 兼容两版 think_layers 数据：
//   - 旧版：planning/execution 直接是字符串数组（summary 即全文，无 detail）
//   - 新版：{"summary":"...","detail":"..."} 对象数组
//
// 老任务落库时 ThinkLayers 还是 []string 结构，升级后若不兼容，
// 反序列化整个 agent_output 会因「cannot unmarshal string into Go struct
// field ThinkStep」报错，导致 reply_text 等字段被一并丢弃。
func (s *ThinkStep) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		s.Summary = str
		s.Detail = ""
		return nil
	}
	// 用别名类型规避递归调用本方法
	type thinkStepAlias ThinkStep
	var a thinkStepAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = ThinkStep(a)
	return nil
}

// ThinkLayers 把思考过程按语义阶段拆成两层，供前端分组折叠展示。
//
// 直接平铺一长串会淹没关键信息：规划期就一两条「拆任务」，
// 执行期却可能几十条「调工具」，混在一起用户根本读不动。
type ThinkLayers struct {
	// Planning 规划期：拆解任务、判定单 Agent / 工作流、规划前调研。
	//
	// 注意：这里不能用 omitempty —— 某一层为空时会直接缺字段，前端
	// `layers.execution.length` 会读到 undefined 崩溃。构造时必须用
	// make([]ThinkStep, 0) 保证非 nil，使空层序列化成 [] 而不是 null/缺字段。
	Planning []ThinkStep `json:"planning"`
	// Execution 执行期：LLM 请求工具、工具执行、给出回复、工作流调度。
	Execution []ThinkStep `json:"execution"`
}

// FillThinkProcess 同时填充平铺 ThinkProcess（兼容旧前端）与分层 ThinkLayers（供前端两层展示）。
// 返回是否产出了过程描述。
func (out *AgentOutputData) FillThinkProcess(turns []*TaskTurn) bool {
	steps := buildThinkSteps(turns)
	if len(steps) == 0 {
		return false
	}
	var layers = ThinkLayers{
		Planning:  make([]ThinkStep, 0),
		Execution: make([]ThinkStep, 0),
	}
	flat := make([]string, len(steps))
	for i, s := range steps {
		flat[i] = s.full
		step := ThinkStep{Summary: s.summary, Running: s.running}
		if s.detail != "" {
			step.Detail = s.detail
		}
		if s.planning {
			layers.Planning = append(layers.Planning, step)
		} else {
			layers.Execution = append(layers.Execution, step)
		}
	}
	out.ThinkLayers = &layers
	out.ThinkProcess = flat
	return true
}

// thinkStep 一条已描述好的过程条目，带 planning 标记供分组。
type thinkStep struct {
	planning bool
	// running 该 turn 仍在执行中，前端需标记为「进行中」。
	running bool
	// summary 默认展示的一句话说明（带 #序号 + 耗时前缀）。
	summary string
	// detail 点击展开的完整描述（无前缀）；非工具 turn 为空。
	detail string
	// full 完整文本（带前缀），用于平铺 think_process 的兼容输出。
	full string
}

// buildThinkSteps 把任务的所有 turn 转成有序的过程条目（按 turn_seq 升序）。
//
// turns 需按 turn_seq 升序（ListTurnsByTaskID 已保证）。
//
// 内部对每条 turn 做一次 NormalizeTurn：DAO 读路径本已归一，
// 但本函数也会被单测与失败补写路径直接喂入手工构造的 turn，
// 归一是幂等的，在此兜底可保证 phase 判定始终有效。
func buildThinkSteps(turns []*TaskTurn) []thinkStep {
	if len(turns) == 0 {
		return nil
	}
	steps := make([]thinkStep, 0, len(turns))
	for _, t := range turns {
		NormalizeTurn(t)

		// 被中断（进程重启等原因收口）的 turn 没有完整结果，跳过。
		// 它通常会被重试后的新 turn 取代，展示出来只会造成重复与误解。
		if t.Status == TurnStatusInterrupted {
			continue
		}

		// 执行中的 turn 保留，只是描述口径不同：它还没有结果，
		// 不能按「已完成」的模板生成（那会写出「成功，无返回内容」这类
		// 与事实相反的话），统一走 describeRunningTurn。
		running := t.Status == TurnStatusRunning

		var line, summary string
		switch t.TurnType {
		case TurnTypeFunctionalCall, TurnTypePlanningLegacy:
			// 同一种调用类型，按语义阶段分别描述：
			// 规划期在「拆解任务」，执行期在「干活」，两者对用户的含义完全不同。
			if running {
				line, summary = describeRunningTurn(t)
			} else if t.IsPlanning() {
				line, summary = describePlanningTurn(t)
			} else {
				line, summary = describeLLMTurn(t)
			}
		case TurnTypeToolCall:
			if running {
				line, summary = describeRunningTurn(t)
			} else {
				line, summary = describeToolTurn(t)
			}
		case TurnTypeWorkflow:
			if running {
				line, summary = describeRunningTurn(t)
			} else {
				line, summary = describeWorkflowTurn(t)
			}
		default:
			continue
		}

		if strings.TrimSpace(line) == "" {
			continue
		}

		// 前缀带上序号与耗时，便于定位与看性能。
		// 执行中的 turn 用「已耗时」代替耗时：TimeCostMs 要等结束才回填，
		// 此刻是 0，直接显示会让人误以为刚起步。
		cost := formatTurnCost(t)
		if running {
			cost = formatRunningCost(t)
		}
		full := fmt.Sprintf("#%d %s（%s）", t.TurnSeq, line, cost)
		step := thinkStep{
			planning: t.IsPlanning(),
			running:  running,
			full:     full,
		}
		if summary == "" {
			// 无独立摘要（规划 / LLM / 工作流 turn）：摘要即全文，无展开层。
			step.summary = full
		} else {
			step.summary = fmt.Sprintf("#%d %s（%s）", t.TurnSeq, summary, cost)
			step.detail = line
		}
		steps = append(steps, step)
	}
	return steps
}

// BuildThinkProcessLayered 把任务的所有 turn 转成两层过程描述（规划期 / 执行期）。
// 层内顺序即真实执行顺序。
func BuildThinkProcessLayered(turns []*TaskTurn) ThinkLayers {
	layers := ThinkLayers{
		Planning:  make([]ThinkStep, 0),
		Execution: make([]ThinkStep, 0),
	}
	for _, s := range buildThinkSteps(turns) {
		step := ThinkStep{Summary: s.summary, Running: s.running}
		if s.detail != "" {
			step.Detail = s.detail
		}
		if s.planning {
			layers.Planning = append(layers.Planning, step)
		} else {
			layers.Execution = append(layers.Execution, step)
		}
	}
	return layers
}

// BuildThinkProcess 返回平铺的过程描述列表，顺序与 turn_seq 一致。
//
// 保留给需要线性视图的调用方与存量单测使用；
// 前端对话任务已改用 think_layers 两层展示，此处主要作兼容兜底。
func BuildThinkProcess(turns []*TaskTurn) []string {
	steps := buildThinkSteps(turns)
	if len(steps) == 0 {
		return nil
	}
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.full
	}
	return out
}

// ============================================================
// 各类型 turn 的描述
// ============================================================

// describePlanningTurn 描述规划期的一次 LLM 调用。
//
// 规划期有两种产出：
//   - 调 submit_plan   → 已给出规划（模式 + 理由 / 摘要）
//   - 调其他真实工具   → 只是在做调研，规划尚未成形
func describePlanningTurn(t *TaskTurn) (string, string) {
	if t.Status == TurnStatusFailed {
		return "任务规划失败：" + truncateThink(firstNonEmpty(t.ErrorMsg, "未知原因"), thinkMaxErrorMsg), ""
	}

	mode, reason, summary := parsePlanningOutput(t.Output)
	switch mode {
	case "workflow":
		line := "任务规划：判定为工作流模式，拆解为多步骤执行"
		if summary != "" {
			line += "；计划：" + truncateThink(summary, thinkMaxPlanReason)
		} else if reason != "" {
			line += "；理由：" + truncateThink(reason, thinkMaxPlanReason)
		}
		return line, ""
	case PlanModeDirectReply:
		line := "任务规划：无需执行动作，直接回复"
		if reason != "" {
			line += "；理由：" + truncateThink(reason, thinkMaxPlanReason)
		}
		return line, ""
	case "single_agent":
		line := "任务规划：判定为单 Agent 模式，直接执行"
		if reason != "" {
			line += "；理由：" + truncateThink(reason, thinkMaxPlanReason)
		}
		return line, ""
	}

	// 没有 submit_plan：本轮是调研，把调用的工具列出来，
	// 否则用户只看到一句"任务规划完成"，不知道中间为什么多花了时间。
	if names := planningResearchTools(t.Output); len(names) > 0 {
		return "规划前调研，调用工具：" + strings.Join(names, "、"), ""
	}
	return "任务规划完成", ""
}

// planningResearchTools 取规划期这次调用请求的真实工具名（排除 submit_plan 协议工具）
func planningResearchTools(output string) []string {
	if output == "" {
		return nil
	}
	var out TurnLLMOutput
	if err := json.Unmarshal([]byte(output), &out); err != nil {
		return nil
	}
	names := make([]string, 0, len(out.ToolCalls))
	for _, tc := range out.ToolCalls {
		if tc.Name == SubmitPlanToolName {
			continue
		}
		names = append(names, tc.Name)
	}
	return names
}

// describeLLMTurn 描述 LLM 调用：请求了工具还是给出了答案
func describeLLMTurn(t *TaskTurn) (string, string) {
	if t.Status == TurnStatusFailed {
		return "模型调用失败：" + truncateThink(firstNonEmpty(t.ErrorMsg, "未知原因"), thinkMaxErrorMsg), ""
	}

	var out TurnLLMOutput
	if t.Output != "" {
		_ = json.Unmarshal([]byte(t.Output), &out)
	}

	// 请求工具调用
	if len(out.ToolCalls) > 0 {
		names := make([]string, 0, len(out.ToolCalls))
		for _, tc := range out.ToolCalls {
			names = append(names, tc.Name)
		}
		return fmt.Sprintf("模型思考后决定调用工具：%s", strings.Join(names, "、")), ""
	}

	// 给出文本回复
	if text := strings.TrimSpace(out.Text); text != "" {
		return "模型给出回复：" + truncateThink(text, thinkMaxLLMText), ""
	}
	return "模型调用完成，无明确输出", ""
}

// toolTurnInput 从工具 turn 的 input 快照解析出的展示字段。
//
// 已完成与执行中的工具 turn 都要用（只是拼接口径不同），抽出来避免两处
// 各解一遍 JSON 而在字段缺失时表现不一致。
type toolTurnInput struct {
	toolName     string
	serverName   string
	args         string
	modelSummary string
}

// parseToolTurnInput 解析工具 turn 的输入快照，缺失字段逐级兜底。
func parseToolTurnInput(t *TaskTurn) toolTurnInput {
	var out toolTurnInput
	out.toolName = t.ToolName
	if t.Input != "" {
		var in TurnToolInput
		if json.Unmarshal([]byte(t.Input), &in) == nil {
			if out.toolName == "" {
				out.toolName = in.ToolName
			}
			out.serverName = in.ServerName
			out.modelSummary = in.Summary
			if len(in.Arguments) > 0 {
				if b, err := json.Marshal(in.Arguments); err == nil {
					out.args = truncateThink(string(b), thinkMaxToolArgs)
				}
			}
		}
	}
	if out.toolName == "" {
		out.toolName = "未知工具"
	}
	return out
}

// describeToolTurn 描述工具调用：调了什么、传了什么、返回如何。
//
// 返回 (line, summary)：line 是完整描述（工具名 + server + 参数 + 返回），
// summary 是一句话说明 —— 优先模型在 functional_call 阶段生成的说明，
// 空则退化为「调用工具 X」。前端默认只展示 summary，点击展开 line。
func describeToolTurn(t *TaskTurn) (string, string) {
	in := parseToolTurnInput(t)

	// 头部：工具名（+ server）+ 参数。
	// input 快照里的 server 名优先 —— 它才是工具实际来源；turn 列上的
	// ServerName 在同一 turn 内与之一致，仅在快照缺失时兜底。
	head := "调用工具 " + in.toolName
	if server := firstNonEmpty(in.serverName, t.ServerName); server != "" {
		head = fmt.Sprintf("调用工具 %s（来自 %s）", in.toolName, server)
	}
	if in.args != "" {
		head += "，参数：" + in.args
	}

	// 摘要：优先模型说明，空则退化
	summary := strings.TrimSpace(in.modelSummary)
	if summary == "" {
		summary = "调用工具 " + in.toolName
	}

	// 失败：把原因带出来，用户需要知道中间哪步断了
	if t.Status == TurnStatusFailed {
		reason := t.ErrorMsg
		if reason == "" {
			var o TurnToolOutput
			if t.Output != "" && json.Unmarshal([]byte(t.Output), &o) == nil {
				reason = o.Content
			}
		}
		return head + " → 失败：" + truncateThink(firstNonEmpty(reason, "未知原因"), thinkMaxErrorMsg), summary
	}

	// 成功：带上返回摘要
	var o TurnToolOutput
	if t.Output != "" {
		_ = json.Unmarshal([]byte(t.Output), &o)
	}
	content := strings.TrimSpace(o.Content)
	if content == "" {
		return head + " → 成功，无返回内容", summary
	}
	return head + " → 返回：" + truncateThink(content, thinkMaxToolOutput), summary
}

// describeWorkflowTurn 描述工作流调度
func describeWorkflowTurn(t *TaskTurn) (string, string) {
	if t.Status == TurnStatusFailed {
		return "工作流调度失败：" + truncateThink(firstNonEmpty(t.ErrorMsg, "未知原因"), thinkMaxErrorMsg), ""
	}
	return "工作流调度完成", ""
}

// describeRunningTurn 描述仍在执行的 turn。
//
// 与终态描述刻意分开：执行中的 turn 只有入参没有结果，套用终态模板会写出
// 「→ 成功，无返回内容」这类与事实相反的话。这里只说「在做什么」。
//
// 能拿到的信息只有入参：工具 turn 创建时就写了 input 快照，所以工具名与
// 参数是可读的；LLM turn 在请求返回前 output 为空，只能给阶段级文案。
func describeRunningTurn(t *TaskTurn) (string, string) {
	switch t.TurnType {
	case TurnTypeToolCall:
		in := parseToolTurnInput(t)
		head := "正在调用工具 " + in.toolName
		if server := firstNonEmpty(in.serverName, t.ServerName); server != "" {
			head = fmt.Sprintf("正在调用工具 %s（来自 %s）", in.toolName, server)
		}
		if in.args != "" {
			head += "，参数：" + in.args
		}
		// 模型在 functional_call 阶段给的说明此时已落库，优先展示；
		// 否则退化成工具名。
		if s := strings.TrimSpace(in.modelSummary); s != "" {
			return head, s
		}
		return head, "正在调用工具 " + in.toolName
	case TurnTypeFunctionalCall:
		// 规划期的 LLM 调用如果已经吐出调研工具（output 已落库的临界情况），
		// 把工具名带出来，比干巴巴一句「正在规划」有用得多。
		if t.IsPlanning() {
			if names := planningResearchTools(t.Output); len(names) > 0 {
				line := "规划前调研，正在调用工具：" + strings.Join(names, "、")
				return line, line
			}
			return "正在规划：拆解任务", "正在规划"
		}
		return "正在思考", "正在思考"
	case TurnTypeWorkflow:
		return "正在调度工作流", "正在调度工作流"
	}
	return "", ""
}

// ============================================================
// 辅助
// ============================================================

// parsePlanningOutput 从 planning turn 的输出里取出 mode / reason / plan_summary。
//
// 规划结果是通过 tool_call(submit_plan) 提交的，所以要从 ToolCalls 的参数里读。
// 只认 submit_plan 这一个工具 —— 规划期也可能调真实工具做调研，
// 它们的入参里若恰好有 mode 字段会被误读成规划模式。
func parsePlanningOutput(output string) (mode, reason, summary string) {
	if output == "" {
		return "", "", ""
	}
	var out TurnLLMOutput
	if err := json.Unmarshal([]byte(output), &out); err != nil {
		return "", "", ""
	}
	for _, tc := range out.ToolCalls {
		if tc.Name != SubmitPlanToolName {
			continue
		}
		if m, ok := tc.Arguments["mode"].(string); ok {
			mode = m
		}
		if r, ok := tc.Arguments["reason"].(string); ok {
			reason = r
		}
		if s, ok := tc.Arguments["plan_summary"].(string); ok {
			summary = s
		}
		if mode != "" {
			break
		}
	}
	return mode, reason, summary
}

// formatTurnCost 格式化耗时，毫秒级过大时转成秒便于阅读
func formatTurnCost(t *TaskTurn) string {
	return formatCostMs(t.TimeCostMs)
}

// formatRunningCost 执行中 turn 的耗时文案。
//
// TimeCostMs 要等 turn 收口才回填，执行中恒为 0，直接显示成「0ms」会让人
// 误以为刚起步。这里用 StartTime 现算已耗时 —— 用户盯着「已 2m30s」才能
// 判断是不是卡住了。StartTime 缺失（手工构造的 turn）时退化为「进行中」。
func formatRunningCost(t *TaskTurn) string {
	if t.StartTime.IsZero() {
		return "进行中"
	}
	elapsed := time.Since(t.StartTime)
	if elapsed < 0 {
		// 时钟回拨 / turn 来自其他机器，宁可显示定性文案也不要负数
		return "进行中"
	}
	return "已 " + formatCostMs(elapsed.Milliseconds())
}

// formatCostMs 毫秒转可读耗时，>=1000ms 用秒
func formatCostMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// firstNonEmpty 返回第一个非空字符串
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// truncateThink 按字符（而非字节）截断，避免把中文截成乱码
func truncateThink(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
