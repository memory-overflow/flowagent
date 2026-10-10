package dao

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// 压缩水位的 upsert 必须带上「取回索引」两列
// ============================================================

// openSummaryTestDB 开一个独立 sqlite 连接（不抢 dao 全局连接）。
func openSummaryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "summary.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := d.AutoMigrate(&model.ContextSummary{}, &model.TaskContextSummary{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return d
}

// TestContextSummaryUpsertKeepsIndexColumns 更新路径也要写索引列。
//
// Upsert 走 ON DUPLICATE KEY UPDATE + **显式列清单**，新增的列忘了加进清单
// 就会只在 INSERT 时生效：绝大多数对话是「已压过一次」的存量行，走的是
// UPDATE 分支 —— 索引与 turn 列表会被静默丢掉，模型手里重新变得没有句柄。
// 这个 bug 只在对同一对话压第二次时才暴露，必须有回归测试守住。
func TestContextSummaryUpsertKeepsIndexColumns(t *testing.T) {
	db := openSummaryTestDB(t)
	ctx := context.Background()
	d := &ContextSummaryDAO{Tx: db}

	s := &model.ContextSummary{ChatID: "chat-1", Summary: "第一版摘要"}
	s.AppendCoveredTaskIndex(model.TaskIndexEntry{TaskID: "t1", Label: "第一轮提问"})
	s.SetCurrentCompactedTurns([]int64{11, 22})
	if err := d.Upsert(ctx, s); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// 第二次压：行已存在 → 走 UPDATE 分支
	s2 := &model.ContextSummary{
		ChatID:               "chat-1",
		Summary:              "第二版摘要",
		SummaryTokens:        30,
		CurrentTaskID:        "t2",
		CurrentCompactedMsgs: 4,
		CurrentSummary:       "当前摘要",
	}
	s2.CoveredTaskIndex = s.CoveredTaskIndex
	s2.AppendCoveredTaskIndex(model.TaskIndexEntry{TaskID: "t2", Label: "第二轮提问"})
	s2.SetCurrentCompactedTurns([]int64{11, 22, 33})
	if err := d.Upsert(ctx, s2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := d.GetByChatID(ctx, "chat-1")
	if err != nil || got == nil {
		t.Fatalf("reload: %v / %+v", err, got)
	}
	if got.Summary != "第二版摘要" {
		t.Errorf("摘要未更新：%q", got.Summary)
	}
	entries := got.CoveredTaskIndexEntries()
	if len(entries) != 2 || entries[1].TaskID != "t2" {
		t.Errorf("任务索引在 UPDATE 分支丢失：%+v", entries)
	}
	if ids := got.CurrentCompactedTurns(); len(ids) != 3 || ids[2] != 33 {
		t.Errorf("turn 列表在 UPDATE 分支丢失：%v", ids)
	}
}

// TestTaskContextSummaryUpsertKeepsTurnIDs 子任务水位同理。
func TestTaskContextSummaryUpsertKeepsTurnIDs(t *testing.T) {
	db := openSummaryTestDB(t)
	ctx := context.Background()
	d := &TaskContextSummaryDAO{Tx: db}

	if err := d.Upsert(ctx, &model.TaskContextSummary{
		TaskID: "task-1", CompactedMsgs: 2, Summary: "v1",
		CompactedTurnIDs: "[7,8]",
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := d.Upsert(ctx, &model.TaskContextSummary{
		TaskID: "task-1", CompactedMsgs: 5, Summary: "v2",
		CompactedTurnIDs: "[7,8,9]",
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := d.GetByTaskID(ctx, "task-1")
	if err != nil || got == nil {
		t.Fatalf("reload: %v / %+v", err, got)
	}
	if got.CompactedTurnIDs != "[7,8,9]" {
		t.Errorf("compacted_turn_ids 在 UPDATE 分支丢失：%q", got.CompactedTurnIDs)
	}
	if got.CompactedMsgs != 5 {
		t.Errorf("compacted_msgs = %d, want 5", got.CompactedMsgs)
	}
}
