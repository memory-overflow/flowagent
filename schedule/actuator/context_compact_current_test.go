package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// ------------------------------------------------------------
// alignCurrentSplitPoint：切点必须落在安全的消息边界上
// ------------------------------------------------------------

func TestAlignCurrentSplitPoint(t *testing.T) {
	assistant := llm.Message{Role: llm.RoleAssistant, Content: "我来查一下"}
	tool1 := llm.Message{Role: llm.RoleTool, Content: "结果一"}
	tool2 := llm.Message{Role: llm.RoleTool, Content: "结果二"}
	tail := llm.Message{Role: llm.RoleAssistant, Content: "结论是"}

	cases := []struct {
		name  string
		msgs  []llm.Message
		n     int
		want  int
		brief string
	}{
		{
			name:  "切点已在非 tool 上则不动",
			msgs:  []llm.Message{assistant, tool1, tool2, tail},
			n:     3,
			want:  3,
			brief: "tail 不是 tool，无需推",
		},
		{
			name:  "切点落在孤儿 tool 上则推过它",
			msgs:  []llm.Message{assistant, tool1, tool2, tail},
			n:     1,
			want:  3,
			brief: "tool1/tool2 一起纳入，避免留下孤儿",
		},
		{
			name:  "assistant 已纳入时其 tool 结果必须跟随",
			msgs:  []llm.Message{tail, assistant, tool1, tool2, tail},
			n:     2,
			want:  4,
			brief: "整次工具调用完整进摘要，不出现「提问进了摘要答案被丢」",
		},
		{
			name:  "末尾全是 tool 时推到末尾",
			msgs:  []llm.Message{assistant, tool1, tool2},
			n:     1,
			want:  3,
			brief: "末尾无 assistant，切点只能到末尾",
		},
		{
			name:  "越界入参被夹紧",
			msgs:  []llm.Message{assistant, tool1},
			n:     99,
			want:  2,
			brief: "n 超过长度时夹紧到 len",
		},
		{
			name:  "负数入参被夹紧",
			msgs:  []llm.Message{assistant, tool1},
			n:     -5,
			want:  0,
			brief: "n 为负时夹紧到 0（此处 0 是非 tool）",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := alignCurrentSplitPoint(c.msgs, c.n); got != c.want {
				t.Fatalf("alignCurrentSplitPoint = %d, want %d（%s）", got, c.want, c.brief)
			}
		})
	}
}

// TestAlignCurrentSplitPoint_NoOrphanLeft 对齐后剩下的消息里不应有孤儿 tool。
//
// 这是对齐规则的最终目的：切断点后若残留孤儿 tool，dropOrphanToolResults
// 会静默删掉它 —— 那不是压缩，是无声丢信息。
func TestAlignCurrentSplitPoint_NoOrphanLeft(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "查一下深圳天气"},
		{Role: llm.RoleAssistant, Content: "好，我去查"},
		{Role: llm.RoleTool, Content: "晴 28℃"},
		{Role: llm.RoleTool, Content: "湿度 60%"},
		{Role: llm.RoleAssistant, Content: "再查下明天"},
		{Role: llm.RoleTool, Content: "多云 27℃"},
	}

	for n := 0; n <= len(msgs); n++ {
		cut := alignCurrentSplitPoint(msgs, n)
		rest := msgs[cut:]
		if len(rest) > 0 && rest[0].Role == llm.RoleTool {
			t.Fatalf("cut=%d 后残留孤儿 tool 结果：%q", cut, rest[0].Content)
		}
	}
}

// ------------------------------------------------------------
// pickCurrentCut：按 token 缺口选切点，并守住尾部保留下限
// ------------------------------------------------------------

// buildMsgs 构造 n 条消息，第 i 条内容长度随 i 线性增长，
// 便于验证「按 token 选」而不是「按条数选」。
func buildMsgs(n int) []llm.Message {
	out := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, llm.Message{
			Role:    llm.RoleAssistant,
			Content: strings.Repeat("内容", (i+1)*20),
		})
	}
	return out
}

func TestPickCurrentCut(t *testing.T) {
	counter := contextmgr.GetCounter()
	msgs := buildMsgs(10)

	t.Run("缺口为零不推进", func(t *testing.T) {
		if got := pickCurrentCut(msgs, 0, 0, counter); got != 0 {
			t.Fatalf("needTokens=0 时应不推进，got %d", got)
		}
	})

	t.Run("缺口越大压得越多", func(t *testing.T) {
		small := pickCurrentCut(msgs, 0, 100, counter)
		large := pickCurrentCut(msgs, 0, 2000, counter)
		if !(small < large) {
			t.Fatalf("缺口更大时应压更多：small=%d large=%d", small, large)
		}
	})

	t.Run("守住尾部保留下限", func(t *testing.T) {
		// 给一个不可能满足的巨大缺口，切点应停在「只剩 2 条」附近，
		// 而不是把整个序列压光。
		got := pickCurrentCut(msgs, 0, 999999, counter)
		if got > len(msgs)-2 {
			t.Fatalf("缺口再大也应保留尾部原文：cut=%d, len=%d", got, len(msgs))
		}
		if got == 0 {
			t.Fatalf("缺口巨大时应尽量推进，而不是原地不动")
		}
	})

	t.Run("已压到只剩两条时让步到留一条", func(t *testing.T) {
		from := len(msgs) - 2
		got := pickCurrentCut(msgs, from, 999999, counter)
		if got <= from {
			t.Fatalf("已压到只剩 2 条仍超限时，应让步再压一条：from=%d got=%d", from, got)
		}
	})

	t.Run("尾部已无空间则不再推进", func(t *testing.T) {
		// 只剩 1 条可压（from 已在倒数第一条），无法再动
		from := len(msgs) - 1
		got := pickCurrentCut(msgs, from, 999999, counter)
		if got > from {
			t.Fatalf("只剩最后一条时不应再压：from=%d got=%d", from, got)
		}
	})
}

// TestPickCurrentCut_RespectsWatermark 切点必须从水位之后开始，不能回退。
func TestPickCurrentCut_RespectsWatermark(t *testing.T) {
	counter := contextmgr.GetCounter()
	msgs := buildMsgs(10)
	from := 4
	if got := pickCurrentCut(msgs, from, 10, counter); got < from {
		t.Fatalf("切点不应回退到水位之前：from=%d got=%d", from, got)
	}
}

// ------------------------------------------------------------
// ContextSummary 水位字段
// ------------------------------------------------------------

func TestContextSummary_HasCurrentCompaction(t *testing.T) {
	var nilSummary *model.ContextSummary
	if nilSummary.HasCurrentCompaction() {
		t.Fatal("nil 摘要不应报「有当前压缩水位」")
	}
	if (&model.ContextSummary{}).HasCurrentCompaction() {
		t.Fatal("空摘要不应报「有当前压缩水位」")
	}
	// 只有 task ID 没有摘要正文，也不算（缺一不可）
	if (&model.ContextSummary{CurrentTaskID: "t1"}).HasCurrentCompaction() {
		t.Fatal("缺摘要正文时不应报「有当前压缩水位」")
	}
	ok := &model.ContextSummary{CurrentTaskID: "t1", CurrentSummary: "摘要"}
	if !ok.HasCurrentCompaction() {
		t.Fatal("task ID 与摘要正文齐备时应报「有当前压缩水位」")
	}
}

func TestContextSummary_RollOverCurrent(t *testing.T) {
	s := &model.ContextSummary{
		ChatID:               "c1",
		Summary:              "历史摘要",
		SummaryTokens:        10,
		CoveredTaskIDs:       `["old1"]`,
		CurrentTaskID:        "cur1",
		CurrentCompactedMsgs: 7,
		CurrentSummary:       "当前 task 早期交互摘要",
		CurrentSummaryTokens: 20,
	}

	s.RollOverCurrent()

	if !strings.Contains(s.Summary, "历史摘要") {
		t.Fatalf("归位后历史摘要应保留原文，got %q", s.Summary)
	}
	if !strings.Contains(s.Summary, "当前 task 早期交互摘要") {
		t.Fatalf("归位后当前摘要应并入历史摘要，got %q", s.Summary)
	}
	if s.SummaryTokens != 30 {
		t.Fatalf("摘要 token 应累加：got %d, want 30", s.SummaryTokens)
	}
	if !s.CoveredTasks()["cur1"] {
		t.Fatalf("归位后旧 task 应进入覆盖集合，got %v", s.CoveredTaskIDs)
	}
	if !s.CoveredTasks()["old1"] {
		t.Fatalf("归位不应丢掉原有覆盖集合，got %v", s.CoveredTaskIDs)
	}
	if s.HasCurrentCompaction() {
		t.Fatalf("归位后当前水位应清空：task=%q summary=%q", s.CurrentTaskID, s.CurrentSummary)
	}
	if s.CurrentCompactedMsgs != 0 || s.CurrentSummaryTokens != 0 {
		t.Fatalf("归位后水位计数应清零：msgs=%d tokens=%d",
			s.CurrentCompactedMsgs, s.CurrentSummaryTokens)
	}
}

// TestContextSummary_RollOverCurrent_NoHistory 无历史摘要时归位不应留下空行前缀。
func TestContextSummary_RollOverCurrent_NoHistory(t *testing.T) {
	s := &model.ContextSummary{
		ChatID:         "c1",
		CurrentTaskID:  "cur1",
		CurrentSummary: "只有当前摘要",
	}
	s.RollOverCurrent()
	if s.Summary != "只有当前摘要" {
		t.Fatalf("无历史摘要时应直接接管，got %q", s.Summary)
	}
}

// TestContextSummary_RollOverCurrent_Noop 没有水位时归位应是安全的空操作。
func TestContextSummary_RollOverCurrent_Noop(t *testing.T) {
	s := &model.ContextSummary{ChatID: "c1", Summary: "历史摘要"}
	s.RollOverCurrent()
	if s.Summary != "历史摘要" {
		t.Fatalf("无水位时归位不应改动历史摘要，got %q", s.Summary)
	}
	if s.CoveredTasks()["cur1"] {
		t.Fatalf("无水位时归位不应污染覆盖集合，got %v", s.CoveredTaskIDs)
	}
}
