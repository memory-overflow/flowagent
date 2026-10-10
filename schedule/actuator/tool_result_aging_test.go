package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// mkToolMsg 造一条工具结果消息（long 指定正文长度）。
func mkToolMsg(toolName string, long int, turnID int64) llm.Message {
	return llm.Message{
		Role:         llm.RoleTool,
		ToolName:     toolName,
		Content:      strings.Repeat("资", long),
		SourceTurnID: turnID,
	}
}

// mkHistory 造一段含 n 条工具结果的历史（每条 long 字符），末尾补一条 assistant。
func mkHistory(n, long int) []llm.Message {
	msgs := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, mkToolMsg("fetch__fetch", long, int64(100+i)))
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "继续"})
	return msgs
}

// TestAgeCurrentToolResults_SmallTaskUntouched 累计字符没超阈值时不做任何老化：
// 短历史全量保留，避免为了「省」而无故丢信息。
func TestAgeCurrentToolResults_SmallTaskUntouched(t *testing.T) {
	msgs := mkHistory(4, 500) // 2000 字符，远低于 agingStartTotalChars
	orig := append([]llm.Message{}, msgs...)
	got := ageCurrentToolResults(msgs)
	for i := range got {
		if got[i].Content != orig[i].Content {
			t.Fatalf("msg #%d must stay untouched when total is below the threshold", i)
		}
	}
}

// TestAgeCurrentToolResults_Tiers 超阈值后按新旧分三档：热档全文、温档压长度、
// 冷档换成带 recall 句柄的占位。
func TestAgeCurrentToolResults_Tiers(t *testing.T) {
	const n = 40
	const long = 4000 // 合计 160K > 60K 阈值
	got := ageCurrentToolResults(mkHistory(n, long))

	hotStart := n - agingHotToolCount
	warmStart := n - agingWarmToolCount
	for i := 0; i < n; i++ {
		runes := len([]rune(got[i].Content))
		switch {
		case i >= hotStart:
			if runes != long {
				t.Errorf("hot msg #%d: len = %d, want untouched %d", i, runes, long)
			}
		case i >= warmStart:
			if runes > agingWarmChars+40 {
				t.Errorf("warm msg #%d: len = %d, want <= ~%d", i, runes, agingWarmChars)
			}
			if !strings.Contains(got[i].Content, "省略") {
				t.Errorf("warm msg #%d must mark the dropped part", i)
			}
		default:
			if !strings.Contains(got[i].Content, "[已归档]") {
				t.Errorf("cold msg #%d must be replaced by a placeholder, got %q", i, got[i].Content[:40])
			}
			if !strings.Contains(got[i].Content, "recall_context(turn_id=1") {
				t.Errorf("cold msg #%d must carry the recall handle, got %q", i, got[i].Content)
			}
			if runes > 300 {
				t.Errorf("cold msg #%d: placeholder too long (%d runes)", i, runes)
			}
		}
	}
}

// TestAgeCurrentToolResults_ShortResultsUntouched 短结果不参与老化：
// 把它换成「描述它的话」反而更长，得不偿失。
func TestAgeCurrentToolResults_ShortResultsUntouched(t *testing.T) {
	const n = 40
	msgs := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		// 前几条超阈值触发老化闸门，后面的短结果必须原样保留
		long := 3000
		if i > 5 {
			long = 100
		}
		msgs = append(msgs, mkToolMsg("web_search", long, int64(200+i)))
	}
	got := ageCurrentToolResults(msgs)
	for i := 6; i < n; i++ {
		if got[i].Content != msgs[i].Content {
			t.Errorf("short msg #%d must stay untouched", i)
		}
	}
}

// TestAgeCurrentToolResults_KeepsPairingFields 老化只改内容，
// 角色 / 工具名 / 来源 turn 必须保留 —— 否则会破坏 assistant 与 tool 结果的配对。
func TestAgeCurrentToolResults_KeepsPairingFields(t *testing.T) {
	got := ageCurrentToolResults(mkHistory(30, 3000))
	for i := 0; i < 30; i++ {
		if got[i].Role != llm.RoleTool {
			t.Fatalf("msg #%d: role changed to %q", i, got[i].Role)
		}
		if got[i].ToolName != "fetch__fetch" {
			t.Fatalf("msg #%d: tool name changed to %q", i, got[i].ToolName)
		}
		if got[i].SourceTurnID != int64(100+i) {
			t.Fatalf("msg #%d: source turn id changed", i)
		}
	}
}

// TestAgedToolPlaceholder_Preview 占位里必须能看出原文大概说了什么，
// 否则模型无法判断要不要取回。
func TestAgedToolPlaceholder_Preview(t *testing.T) {
	body := "\n\n  LangGraph 支持 checkpoint 与 human-in-the-loop\n正文……\n"
	got := agedToolPlaceholder("fetch__fetch", body, 4242)
	if !strings.Contains(got, "LangGraph") {
		t.Errorf("placeholder must keep a preview of the original, got %q", got)
	}
	if !strings.Contains(got, "recall_context(turn_id=4242)") {
		t.Errorf("placeholder must carry the recall handle, got %q", got)
	}
	if !strings.Contains(got, "已归档") {
		t.Errorf("placeholder must say the result was archived, got %q", got)
	}
	// 空正文不能输出空预览
	if got := agedToolPlaceholder("fetch", "", 1); !strings.Contains(got, "无正文") {
		t.Errorf("empty content must degrade gracefully, got %q", got)
	}
}

func TestTruncateKeepHeadTail(t *testing.T) {
	if got := truncateKeepHeadTail("短", 100); got != "短" {
		t.Error("content within limit must be untouched")
	}
	got := truncateKeepHeadTail(strings.Repeat("甲", 100), 30)
	if !strings.Contains(got, "省略") {
		t.Error("truncated content must mark the dropped part")
	}
	// 头尾都要在
	if !strings.HasPrefix(got, "甲甲") || !strings.HasSuffix(got, "甲甲") {
		t.Errorf("truncation must keep both head and tail, got %q", got)
	}
}
