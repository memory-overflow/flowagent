package actuator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// 检索类工具的「无效结果」治理
// ============================================================
//
// 背景（实测一次 Agent 选型调研）：1011 次 web_search 里有 260 次（25.7%）
// 返回 <100B —— 空结果或抓取失败。这类调用在既有护栏下是**成功**的：
//   - 连续失败熔断（ConsecutiveFailures）数的是报错，数不到它；
//   - 死循环检测（RepeatedToolCall）要求「工具名 + 参数完全一致」，
//     而模型每次换的词都不同，也抓不到；
//   - 工具调用总次数预算只能限总量，不能识别「一直在白跑」。
//
// 于是模型在一条空转轨道上持续消耗预算：每次看到空内容，就换个词再搜一次。
//
// 治理分两步，都不需要 LLM：
//  1. 标准化：把「成功但空/过短」的返回改写成带明确下一步建议的结构化提示，
//     让模型知道该换关键词 / 换来源，而不是再试一次；
//  2. 连续无效熔断：连续 N 次检索类调用都没拿到有效内容就强制收口，
//     它是「换词也换不出东西」这个信号的唯一判据。

// minUsefulToolResultChars 检索类工具返回低于该字符数即视为「无效」。
//
// 100 字符约等于一行摘要：真正的搜索结果至少会带回标题 + 链接 + 一句话摘要，
// 短于此基本等于没搜到。刻意取小值 —— 宁可放过，不可误伤（例如某些来源
// 只返回一句结论但确实有用）。
const minUsefulToolResultChars = 100

// retrievalToolNameHints 判定「检索类工具」的名称特征。
//
// 按名称特征而非精确白名单：MCP 工具的暴露名带 server 前缀（如 fetch__fetch），
// 用户还可自行绑定别的检索 server（serper__search 等），穷举枚举维护不动。
// 只要名字里含 search / fetch，语义上就是「去外面取信息」，适用同一套治理。
var retrievalToolNameHints = []string{"search", "fetch"}

// isRetrievalTool 判断工具名是否属于检索类（搜索 / 抓取）。
func isRetrievalTool(toolName string) bool {
	lower := strings.ToLower(toolName)
	for _, hint := range retrievalToolNameHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// isLowValueToolResult 判断一条检索类工具的返回是否「无效」（空或过短）。
//
// 只对检索类工具判定：write_file 这类工具的成功返回本就只有几十个字符
// （实测「写入成功」类回执 93B），按长度判会误伤。
func isLowValueToolResult(toolName, content string) bool {
	if !isRetrievalTool(toolName) {
		return false
	}
	return len([]rune(strings.TrimSpace(content))) < minUsefulToolResultChars
}

// normalizeLowValueToolResult 把无效的检索结果改写成结构化提示。
//
// 有效内容一律原样返回（不给正常结果加噪声）；无效内容在保留原文的前提下
// 追加引导 —— 原文仍要留着，否则模型（与调试页）看不出这次到底返回了什么。
func normalizeLowValueToolResult(toolName, content string) string {
	if !isLowValueToolResult(toolName, content) {
		return content
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return lowValueToolResultHint(toolName)
	}
	return trimmed + "\n\n" + lowValueToolResultHint(toolName)
}

// lowValueToolResultHint 无效结果的引导话术。
//
// 必须给出可执行的替代路径，否则模型只会原地重试同一个工具：
// 换更具体的词 → 换来源 → 交给 deep_search（它内部多引擎交叉 + 自动抓取，
// 比单条 web_search 更容易出东西）→ 基于已有信息收口。
func lowValueToolResultHint(toolName string) string {
	return fmt.Sprintf(
		"[无有效内容] 本次 %s 没有拿到可用信息（返回为空或过短，可能是来源无结果 / 抓取被拦 / 关键词过宽）。"+
			"请不要原样重试同一个查询，按序尝试："+
			"1) 换成更具体、更短的关键词（去掉修饰语与多对象并列）；"+
			"2) 换一个信息源或换一种问法（用该领域的常用说法）；"+
			"3) 若需大范围取证改用 deep_search（多引擎交叉 + 自动抓取正文）；"+
			"4) 若已多次无果，基于已有信息给出结论并说明未覆盖的部分。",
		toolName)
}

// lowValueStreakFromTurns 统计「末尾连续无效的工具调用」次数。
//
// 判定口径（按 turn_seq 倒序，只看 tool_call turn）：
//   - 失败的 turn（status=failed）计入：报错同样是「没拿到东西」；
//   - 检索类工具返回过短计入（见 isLowValueToolResult）；
//   - 遇到第一条有效结果即停止；
//   - 非 tool_call 的 turn 跳过不计也不打断 —— assistant 的思考消息夹在
//     工具调用之间是常态，用它打断会让计数永远归零。
//
// 纯函数（不碰 registry / 不落库），便于单测。
func lowValueStreakFromTurns(turns []*model.TaskTurn) int {
	streak := 0
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t == nil || t.TurnType != model.TurnTypeToolCall {
			continue
		}
		if t.Status == model.TurnStatusFailed {
			streak++
			continue
		}
		content := ""
		if t.Output != "" {
			var out model.TurnToolOutput
			if err := json.Unmarshal([]byte(t.Output), &out); err == nil {
				content = out.Content
			}
		}
		if isLowValueToolResult(t.ToolName, content) {
			streak++
			continue
		}
		break
	}
	return streak
}

// resolveLowValueStreak 取当前任务「连续无效工具调用」的次数。
//
// 与 resolveToolCallBudget 同构：以 DB 为准而不是内存 hint —— 进程重启 /
// 多副本接管时内存态会丢，而漏判的代价是模型继续在空转轨道上烧预算。
// 失败时返回 0（保守：不因统计出错就把任务熔断）。
func resolveLowValueStreak(ctx context.Context, taskID string) int {
	// 廉价前置过滤：工具调用总数都不到阈值时不可能是「连续 N 次无效」，
	// 省掉每轮一次带长字段的 turn 全量拉取（agent loop 每轮都会走这里）。
	if n, err := funcCallRegistry.turnDAO.CountToolCallTurns(ctx, taskID); err == nil &&
		int(n) < model.DefaultMaxLowValueToolCalls {
		return 0
	}
	turns, err := funcCallRegistry.turnDAO.ListTurnsByTaskID(ctx, taskID)
	if err != nil {
		log.Printf("[func-call-actuator][%s] list turns for low-value streak failed: %v", taskID, err)
		return 0
	}
	return lowValueStreakFromTurns(turns)
}
