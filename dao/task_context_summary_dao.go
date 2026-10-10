package dao

import (
	"context"
	"fmt"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TaskContextSummaryDAO 任务级压缩水位的数据访问对象。
//
// 目前只有工作流节点子任务使用（并发节点必须各自持有水位，见
// model.TaskContextSummary 的说明）。
type TaskContextSummaryDAO struct {
	Tx *gorm.DB
}

// GetByTaskID 读取某任务的压缩水位；不存在返回 (nil, nil)。
//
// 「不存在」是正常状态 —— 绝大多数任务根本长不到需要压缩，
// 调用方据此走「无水位」分支，不应视为错误。
func (d *TaskContextSummaryDAO) GetByTaskID(
	ctx context.Context, taskID string,
) (*model.TaskContextSummary, error) {
	db := d.getDB().WithContext(ctx)
	var s model.TaskContextSummary
	if err := db.Where("task_id = ?", taskID).First(&s).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get task context summary failed: %w", err)
	}
	return &s, nil
}

// Upsert 写入或覆盖某任务的压缩水位。
//
// 用 ON DUPLICATE KEY UPDATE：同一个任务的不同 turn 可能落在不同的
// 调度器副本上，先查后写会在竞态下丢掉一次压缩结果。
func (d *TaskContextSummaryDAO) Upsert(ctx context.Context, s *model.TaskContextSummary) error {
	db := d.getDB().WithContext(ctx)
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now

	if err := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "task_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"compacted_msgs", "compacted_turn_ids", "summary", "summary_tokens",
			"compact_count", "original_tokens", "updated_time",
		}),
	}).Create(s).Error; err != nil {
		return fmt.Errorf("upsert task context summary failed: %w", err)
	}
	return nil
}

// DeleteByTaskID 删除某任务的压缩水位（任务被清理时调用）。
func (d *TaskContextSummaryDAO) DeleteByTaskID(ctx context.Context, taskID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("task_id = ?", taskID).
		Delete(&model.TaskContextSummary{}).Error; err != nil {
		return fmt.Errorf("delete task context summary failed: %w", err)
	}
	return nil
}

func (d *TaskContextSummaryDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
