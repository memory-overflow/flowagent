package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestAppendFinalRoundInstruction 轮次耗尽时要在消息末尾追加一条收口指令。
//
// 背景（回归防护）：旧实现只在 LLM 返回后把 needToolCall 置 false，
// 而模型在工具可用时通常只回 tool_call，附带 text 是「我还要去核实 xx」
// 这类过程描述 —— 落库的就是这句话，任务看似成功、结论却是废的
// （deep_search 子任务 baffb17c 第 8 轮即是）。修法是调用前摘工具 + 追加
// 本指令，逼模型写真正的总结。
func TestAppendFinalRoundInstruction(t *testing.T) {
	base := []llm.Message{
		{Role: llm.RoleSystem, Content: "你是搜索助手"},
		{Role: llm.RoleUser, Content: "查一下 Wan2.2 的许可证"},
	}

	got := appendFinalRoundInstruction(base, 4)

	if len(got) != len(base)+1 {
		t.Fatalf("应恰好追加 1 条消息, got %d want %d", len(got), len(base)+1)
	}
	// 原有消息不能被改写（llm.Message 含 slice 字段，不能用 == 整体比较）
	for i := range base {
		if got[i].Role != base[i].Role || got[i].Content != base[i].Content {
			t.Errorf("第 %d 条原消息被改写: got %+v want %+v", i, got[i], base[i])
		}
	}

	last := got[len(got)-1]
	if last.Role != llm.RoleUser {
		t.Errorf("收口指令应是 user 消息（system 追加在末尾部分 provider 不接受）, got role=%q", last.Role)
	}
	// 上限数字必须出现在指令里：模型据此知道预算确实耗尽，不是被误伤
	if !strings.Contains(last.Content, "4") {
		t.Errorf("收口指令应带上轮次上限数字, got %q", last.Content)
	}
	if !strings.Contains(last.Content, "最终结论") {
		t.Errorf("收口指令应明确要求输出最终结论, got %q", last.Content)
	}
	if !strings.Contains(last.Content, "不要再尝试调用工具") {
		t.Errorf("收口指令应明确禁止再调工具, got %q", last.Content)
	}
}

// TestAppendFinalRoundInstructionEmptyBase 首轮就耗尽（maxRound<=0 的极端配置）
// 也要能正常追加，不能 panic。
func TestAppendFinalRoundInstructionEmptyBase(t *testing.T) {
	got := appendFinalRoundInstruction(nil, 0)
	if len(got) != 1 {
		t.Fatalf("空消息序列追加后应为 1 条, got %d", len(got))
	}
	if got[0].Role != llm.RoleUser {
		t.Errorf("role = %q, want user", got[0].Role)
	}
}
