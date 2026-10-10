package dao

import (
	"context"
	"fmt"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ContextSummaryDAO 对话级已压缩历史摘要的数据访问对象。
type ContextSummaryDAO struct {
	Tx *gorm.DB
}

// GetByChatID 读取某对话的压缩摘要；不存在返回 (nil, nil)。
//
// 「不存在」是正常状态（该对话还没长到需要压缩），调用方据此走
// 「无已压缩摘要」分支，不应视为错误。
func (d *ContextSummaryDAO) GetByChatID(ctx context.Context, chatID string) (*model.ContextSummary, error) {
	db := d.getDB().WithContext(ctx)
	var s model.ContextSummary
	if err := db.Where("chat_id = ?", chatID).First(&s).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get context summary failed: %w", err)
	}
	return &s, nil
}

// Upsert 写入或覆盖某对话的压缩摘要。
//
// 用 ON DUPLICATE KEY UPDATE 而非「先查后写」：压缩可能由不同调度器
// 副本并发触发（同一对话的两个 task 同时推进），先查后写会在竞态下
// 丢掉一次压缩结果或插入冲突。交给 DB 做原子 upsert 最稳。
//
// 语义是「后写覆盖」而非 merge —— 压缩本身就是全量重算摘要，
// 后一次的结果必然包含前一次的信息。
func (d *ContextSummaryDAO) Upsert(ctx context.Context, s *model.ContextSummary) error {
	db := d.getDB().WithContext(ctx)
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now

	if err := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "chat_id"}},
		// current_* 四列必须一并更新：那是「当前 task 早期交互压缩」的水位。
		// 漏掉它们时，行已存在的对话（即绝大多数已压缩过的对话）在 upsert
		// 后水位仍是旧值 —— 下一轮重建会认为「当前 task 没压过」，把同一批
		// 交互从头再压一遍。插入路径不受影响（INSERT 写全列），所以这个
		// bug 只在「该对话已有摘要行」时才暴露，正是压过一次之后的每一轮。
		DoUpdates: clause.AssignmentColumns([]string{
			"summary", "covered_task_ids", "covered_task_index", "summary_tokens",
			"compact_count", "original_tokens", "updated_time",
			"current_task_id", "current_compacted_msgs", "current_compacted_turn_ids",
			"current_summary", "current_summary_tokens",
		}),
	}).Create(s).Error; err != nil {
		return fmt.Errorf("upsert context summary failed: %w", err)
	}
	return nil
}

// DeleteByChatID 删除某对话的压缩摘要（对话被删除时清理）。
func (d *ContextSummaryDAO) DeleteByChatID(ctx context.Context, chatID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("chat_id = ?", chatID).
		Delete(&model.ContextSummary{}).Error; err != nil {
		return fmt.Errorf("delete context summary failed: %w", err)
	}
	return nil
}

func (d *ContextSummaryDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
