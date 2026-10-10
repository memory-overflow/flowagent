package container

import (
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

func newTestStore() *StageTransitionStore {
	return &StageTransitionStore{hints: make(map[string]*model.StageTransitionHint)}
}

// TestConsecutiveFailuresPerFingerprint 连续失败熔断按「调用指纹」（工具名+参数）
// 记账：同一指纹连续失败累计到阈值熔断；换工具/换参数是新的指纹、从 1 起算
// 不背旧账（那是模型在换方法尝试）；同指纹成功一次只清本指纹。
func TestConsecutiveFailuresPerFingerprint(t *testing.T) {
	store := newTestStore()
	id := "task-fp"
	const fpA = `read_file:{"path":"a.txt"}`
	const fpB = `read_file:{"path":"b.txt"}`

	// 同一指纹连续失败 2 次
	store.IncrConsecutiveFailures(id, fpA)
	store.IncrConsecutiveFailures(id, fpA)
	if hint := store.GetHint(id); hint.ConsecutiveFailures != 2 {
		t.Fatalf("同指纹失败 2 次后 ConsecutiveFailures = %d, want 2", hint.ConsecutiveFailures)
	}

	// 换参数（新指纹）失败 1 次：各指纹独立，最大值仍是 2 —— 换法重试不背旧账
	store.IncrConsecutiveFailures(id, fpB)
	hint := store.GetHint(id)
	if hint.ConsecutiveFailures != 2 {
		t.Fatalf("换参数后 ConsecutiveFailures = %d, want 2（新指纹从 1 起算）", hint.ConsecutiveFailures)
	}
	if hint.FailStreaks[fpA] != 2 || hint.FailStreaks[fpB] != 1 {
		t.Fatalf("各指纹计数错误: %+v", hint.FailStreaks)
	}

	// 同指纹第 3 次失败 → 达到熔断阈值
	store.IncrConsecutiveFailures(id, fpA)
	if hint = store.GetHint(id); hint.ConsecutiveFailures != 3 {
		t.Fatalf("同指纹第 3 次失败后 ConsecutiveFailures = %d, want 3", hint.ConsecutiveFailures)
	}

	// 原指纹成功一次 → 只清本指纹，其他指纹计数保留
	store.ResetConsecutiveFailures(id, fpA)
	if hint = store.GetHint(id); hint.ConsecutiveFailures != 1 {
		t.Fatalf("清零后 ConsecutiveFailures = %d, want 1（剩余 fpB 的 1 次）", hint.ConsecutiveFailures)
	}
	if _, ok := store.GetHint(id).FailStreaks[fpA]; ok {
		t.Fatalf("fpA 成功后应被清零: %+v", store.GetHint(id).FailStreaks)
	}
}

// TestConsecutiveFailuresNoHint Reset 在无 hint / 无计数时不 panic、不误建条目。
func TestConsecutiveFailuresNoHint(t *testing.T) {
	store := newTestStore()
	// 无 hint 时 Reset：不 panic
	store.ResetConsecutiveFailures("task-none", "fp")
	if store.GetHint("task-none") != nil {
		t.Fatal("Reset 不应创建 hint 条目")
	}
	// 有 hint（其他字段被写过）但无失败计数时 Reset：无副作用
	store.SetDeferred("task-deferred")
	store.ResetConsecutiveFailures("task-deferred", "fp")
	if hint := store.GetHint("task-deferred"); !hint.Deferred || hint.ConsecutiveFailures != 0 {
		t.Fatalf("Reset 不应影响已有 hint 的其他字段: %+v", hint)
	}
}
