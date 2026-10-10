package actuator

import (
	"fmt"
	"strings"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// ============================================================
// 当前任务内工具结果的「老化」
// ============================================================
//
// 背景：agent loop 每轮都从 task_turn 重建完整历史，重建时每条工具结果被
// 截断到 maxToolResultChars(4000) 后回喂。单条有上限，但**条数没有**：
// 一轮并行 4~6 个调用，跑十几轮就是几十条 × 4000 = 近 20 万字符的工具结果
// 常驻上下文 —— 真正需要被模型看见的其实只有最近几轮。
//
// 与既有机制的分工：
//   - truncateToolResult：单条上限，无差别作用于每一条；
//   - context_compact  ：跨 task 的历史压缩，用 LLM 生成摘要（有损、需 recall 取回）；
//   - 本文件            ：**单 task 内**按新旧分级降级，纯规则、零 LLM、确定性。
//
// 为什么可以纯规则：老化只是把内容「挪出」上下文，不是丢掉 ——
// 原文仍在 task_turn 里，占位行给出 recall_context(turn_id=) 这个零 LLM 的
// 取回通道（见 context_recall.go）。需要摘要语义的场景才用 LLM，那是
// context_compact 的职责，不在本层做。

const (
	// agingStartTotalChars current 段工具结果累计字符超过该阈值才启动老化。
	//
	// 只按字符闸门、不按条数闸门：条数多但每条都很短时（例如几十条 500 字的
	// 结果），老化没有收益，只会无故丢信息。60K 字符约 30k token，
	// 是「工具结果已经开始挤占推理空间」的经验分界。
	agingStartTotalChars = 60000

	// agingHotToolCount 最近多少条工具结果保持全文（热档）。
	//
	// 8 条 ≈ 两轮并行调用（实测一轮 4~6 个），覆盖「模型此刻正在消化的材料」。
	agingHotToolCount = 8

	// agingWarmToolCount 热档之外再保留多少条为「温档」（压到摘要级长度）。
	//
	// 倒数第 9~24 条：可能还要回头引用（如刚查过的对比数据），
	// 但不必逐字在上下文里，压到 agingWarmChars 足够对得上号。
	agingWarmToolCount = 24

	// agingWarmChars 温档的字符上限（头 2/3 + 尾 1/3）。
	agingWarmChars = 600

	// agingMinContentChars 短于此的结果不参与老化。
	//
	// 它本来就占不了多少空间，降级成占位反而让「内容」变成「描述内容的话」，
	// 得不偿失（写文件回执、报错说明都在这一档）。
	agingMinContentChars = 400

	// agingPreviewChars 冷档占位里保留的原文预览长度。
	agingPreviewChars = 120
)

// ageCurrentToolResults 对当前任务的 tool 结果做分级老化。
//
// 只改 Content，不动 Role / ToolName / SourceTurnID —— 工具结果与 assistant
// 的 tool_calls 配对关系不受影响，provider 侧看到的仍是合法的函数返回。
//
// 纯函数（不落库、不调 LLM），便于单测。
func ageCurrentToolResults(msgs []llm.Message) []llm.Message {
	idxs := make([]int, 0, len(msgs))
	total := 0
	for i := range msgs {
		if msgs[i].Role != llm.RoleTool {
			continue
		}
		idxs = append(idxs, i)
		total += len([]rune(msgs[i].Content))
	}
	if len(idxs) == 0 || total <= agingStartTotalChars {
		return msgs
	}

	last := len(idxs) - 1
	for k, i := range idxs {
		content := msgs[i].Content
		if len([]rune(content)) <= agingMinContentChars {
			continue // 短结果不参与老化
		}
		switch dist := last - k; {
		case dist < agingHotToolCount:
			// 热档：原样保留（已由 truncateToolResult 限到 4000）
		case dist < agingWarmToolCount:
			msgs[i].Content = truncateKeepHeadTail(content, agingWarmChars)
		default:
			msgs[i].Content = agedToolPlaceholder(msgs[i].ToolName, content, msgs[i].SourceTurnID)
		}
	}
	return msgs
}

// agedToolPlaceholder 冷档工具结果的占位文本。
//
// 必须带三样东西，否则等于把信息扔了：
//  1. 它是什么（工具名 + 原始规模）—— 让模型知道这里原本有东西；
//  2. 它大概说了什么（首行预览）—— 让模型判断要不要取回；
//  3. 怎么拿回来（recall_context 的 turn_id 句柄）—— 取回是零 LLM 直查。
func agedToolPlaceholder(toolName, content string, turnID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[已归档] 较早一次 %s 的返回（原 %d 字符）已移出上下文，预览：%s",
		toolName, len([]rune(content)), firstLinePreview(content, agingPreviewChars))
	if turnID > 0 {
		fmt.Fprintf(&b, "\n需要原文请调用 recall_context(turn_id=%d)。", turnID)
	}
	return b.String()
}

// firstLinePreview 取正文首个非空行作为预览，超长截断。
func firstLinePreview(s string, limit int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) <= limit {
			return line
		}
		return string(runes[:limit]) + "…"
	}
	return "(无正文)"
}

// truncateKeepHeadTail 按 rune 截断，保留头尾并标注省略量。
//
// 头 2/3 + 尾 1/3：开头是结论与要点，结尾常是来源链接 / 附录，两头都不能丢。
func truncateKeepHeadTail(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	tail := limit / 3
	if tail < 1 {
		tail = 1
	}
	head := limit - tail
	return fmt.Sprintf("%s\n…(省略 %d 字符)…\n%s",
		string(runes[:head]), len(runes)-limit, string(runes[len(runes)-tail:]))
}
