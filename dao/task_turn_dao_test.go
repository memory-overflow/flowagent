package dao

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// turnTokens — token 三元组的归一化
// ============================================================

func TestTurnTokens_Normalize(t *testing.T) {
	cases := []struct {
		name           string
		in             turnTokens
		wantPrompt     int
		wantCompletion int
		wantTotal      int
	}{
		{
			name:           "total 缺失时由 prompt+completion 补齐",
			in:             turnTokens{prompt: 100, completion: 20},
			wantPrompt:     100,
			wantCompletion: 20,
			wantTotal:      120,
		},
		{
			name:           "total 已给出时不覆盖",
			in:             turnTokens{prompt: 100, completion: 20, total: 130},
			wantPrompt:     100,
			wantCompletion: 20,
			wantTotal:      130,
		},
		{
			name:      "provider 未回传用量时保持全零（0 = 未知，不硬凑）",
			in:        turnTokens{},
			wantTotal: 0,
		},
		{
			// 只有 total 的反例：拆分无从推断，绝不能反推出 prompt=total
			name:           "只有 total 时不反向拆分",
			in:             turnTokens{total: 50},
			wantPrompt:     0,
			wantCompletion: 0,
			wantTotal:      50,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.normalize()
			if got.prompt != c.wantPrompt || got.completion != c.wantCompletion || got.total != c.wantTotal {
				t.Errorf("normalize() = (%d, %d, %d), want (%d, %d, %d)",
					got.prompt, got.completion, got.total,
					c.wantPrompt, c.wantCompletion, c.wantTotal)
			}
		})
	}
}

// ============================================================
// extractTurnTokens — 从输出快照里取消耗
// ============================================================

func TestExtractTurnTokens(t *testing.T) {
	t.Run("LLM 输出取出三元组", func(t *testing.T) {
		got := extractTurnTokens(&model.TurnLLMOutput{
			PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200,
		})
		if got.prompt != 1000 || got.completion != 200 || got.total != 1200 {
			t.Errorf("got (%d,%d,%d), want (1000,200,1200)", got.prompt, got.completion, got.total)
		}
	})

	t.Run("工具输出的 token 恒为零", func(t *testing.T) {
		// 工具调用不消耗 LLM token，记 0 而非「未知」
		got := extractTurnTokens(&model.TurnToolOutput{Content: "ok"})
		if got.prompt != 0 || got.completion != 0 || got.total != 0 {
			t.Errorf("tool output should yield zero tokens, got (%d,%d,%d)",
				got.prompt, got.completion, got.total)
		}
	})

	t.Run("nil 输出返回零值", func(t *testing.T) {
		got := extractTurnTokens(nil)
		if got.prompt != 0 || got.completion != 0 || got.total != 0 {
			t.Errorf("nil output should yield zero tokens, got (%d,%d,%d)",
				got.prompt, got.completion, got.total)
		}
	})

	t.Run("失败路径也要能取到消耗", func(t *testing.T) {
		// LLM 已返回响应就意味着费用已产生 —— 后续校验失败不该让这笔消耗消失
		got := extractTurnTokens(&model.TurnLLMOutput{
			PromptTokens: 800, CompletionTokens: 0, TotalTokens: 800,
		})
		if got.total != 800 {
			t.Errorf("total = %d, want 800", got.total)
		}
	})
}

// ============================================================
// FinishTurnParams.tokenUsage
// ============================================================

func TestFinishTurnParams_TokenUsage(t *testing.T) {
	got := FinishTurnParams{
		PromptTokens:     300,
		CompletionTokens: 50,
	}.tokenUsage()
	// 调用方只填了拆分值，total 应自动补上
	if got.total != 350 {
		t.Errorf("total = %d, want 350 (prompt + completion)", got.total)
	}

	// 非 LLM turn（工具调用）不填任何 token → 全零，不应写入任何列
	empty := FinishTurnParams{}.tokenUsage()
	if empty.prompt != 0 || empty.completion != 0 || empty.total != 0 {
		t.Errorf("tool turn should yield zero tokens, got (%d,%d,%d)",
			empty.prompt, empty.completion, empty.total)
	}
}

// ============================================================
// HasToolCallTurnAfter —— 幂等判断必须排除「未执行完」的 turn
// ============================================================

func openTurnIdempotentTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "turn.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := d.AutoMigrate(&model.TaskTurn{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return d
}

func mkToolCallTurn(t *testing.T, db *gorm.DB, taskID string, seq int32, status model.TurnStatus) {
	t.Helper()
	now := time.Now()
	db.Create(&model.TaskTurn{
		TaskID: taskID, ChatID: "chat-1", TurnSeq: seq,
		TurnType: model.TurnTypeToolCall, Status: status,
		StartTime: now, CreatedAt: now, UpdatedAt: now,
	})
}

// TestHasToolCallTurnAfter_ExcludesInterrupted 被中断的 tool_call turn 不能算「已执行」。
//
// 根因回归：deep_search 是同步阻塞工具（BeginTurn 后等子任务最长 15 分钟），
// 重启时它的 tool_call turn 被 MarkRunningTurnsInterrupted 标成 interrupted。
// 旧实现只看 turn 存在性、不看 status，会把 interrupted 也当「已执行」→
// 主任务重放时误判跳过 deep_search → 子任务没人等、结论没人收，整个搜索废弃。
func TestHasToolCallTurnAfter_ExcludesInterrupted(t *testing.T) {
	db := openTurnIdempotentTestDB(t)
	ctx := context.Background()
	d := &TaskTurnDAO{Tx: db}

	// 只有 interrupted 的 tool_call turn：必须返回 false（重新执行）
	mkToolCallTurn(t, db, "task-interrupted", 2, model.TurnStatusInterrupted)
	got, err := d.HasToolCallTurnAfter(ctx, "task-interrupted", 1)
	if err != nil {
		t.Fatalf("HasToolCallTurnAfter: %v", err)
	}
	if got {
		t.Error("interrupted turn 被误判为已执行，deep_search 会被跳过")
	}

	// 只有 running 的 turn：同样不算已执行
	mkToolCallTurn(t, db, "task-running", 2, model.TurnStatusRunning)
	if got, _ := d.HasToolCallTurnAfter(ctx, "task-running", 1); got {
		t.Error("running turn 被误判为已执行")
	}
}

// TestHasToolCallTurnAfter_CountsFinished 真正执行完的 turn（成功/失败）才算已执行。
func TestHasToolCallTurnAfter_CountsFinished(t *testing.T) {
	db := openTurnIdempotentTestDB(t)
	ctx := context.Background()
	d := &TaskTurnDAO{Tx: db}

	mkToolCallTurn(t, db, "task-success", 2, model.TurnStatusSuccess)
	if got, _ := d.HasToolCallTurnAfter(ctx, "task-success", 1); !got {
		t.Error("success turn 应算已执行")
	}

	mkToolCallTurn(t, db, "task-failed", 2, model.TurnStatusFailed)
	if got, _ := d.HasToolCallTurnAfter(ctx, "task-failed", 1); !got {
		t.Error("failed turn 应算已执行")
	}

	// 没有任何 tool_call turn
	if got, _ := d.HasToolCallTurnAfter(ctx, "task-empty", 1); got {
		t.Error("无 turn 不应算已执行")
	}

	// turn_seq 早于 afterSeq：不算「之后」的 turn
	mkToolCallTurn(t, db, "task-before", 0, model.TurnStatusSuccess)
	if got, _ := d.HasToolCallTurnAfter(ctx, "task-before", 1); got {
		t.Error("turn_seq <= afterSeq 不应算已执行")
	}
}
