package handler

import (
	"encoding/json"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// TestExtractContextBlocks 验证从 turn input 快照 JSON 抽取分块明细。
//
// 三类输入必须行为正确：
//   - 新数据：带 context_blocks → 正常抽出
//   - 老数据：无该字段 → (nil, false)，前端按「无数据」展示
//   - 脏数据：JSON 解不开 → (nil, false)，不允许 panic 拖挂接口
func TestExtractContextBlocks(t *testing.T) {
	snap := &model.TurnLLMInput{
		Model: "hy4-preview",
		ContextBlocks: &model.TurnContextBlocks{
			SystemPrompt:   2477,
			Tools:          1807,
			Detail:         9000,
			UserAndCurrent: 1447,
			Total:          14731,
			ContextLimit:   16000,
		},
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	b, ok := extractContextBlocks(string(raw))
	if !ok {
		t.Fatalf("expected ok=true for snapshot with context_blocks")
	}
	if b.Total != 14731 || b.SystemPrompt != 2477 || b.Tools != 1807 {
		t.Fatalf("unexpected blocks: %+v", b)
	}
	// ContextLimit 不透出（响应顶层统一给），避免两处口径不一致
	_ = b

	if _, ok := extractContextBlocks(`{"model":"old-turn-no-blocks"}`); ok {
		t.Fatalf("expected ok=false for legacy snapshot without context_blocks")
	}
	if _, ok := extractContextBlocks(`not-json`); ok {
		t.Fatalf("expected ok=false for invalid json")
	}
	if _, ok := extractContextBlocks(``); ok {
		t.Fatalf("expected ok=false for empty input")
	}
}

// TestExtractContextBlocks_AllZero 空块视为无数据：
// JSON 有字段但全是 0 时，展示出来只会是一张空卡，不如按无数据处理。
func TestExtractContextBlocks_AllZero(t *testing.T) {
	raw := `{"context_blocks":{"system_prompt":0,"tools":0,"total":0}}`
	if _, ok := extractContextBlocks(raw); ok {
		t.Fatalf("expected ok=false for all-zero blocks")
	}
}
