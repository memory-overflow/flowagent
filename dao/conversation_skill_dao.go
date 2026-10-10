package dao

import (
	"context"
	"fmt"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ConversationSkillDAO 对话-Skill 关联数据访问对象
type ConversationSkillDAO struct {
	Tx *gorm.DB
}

// ListConversationSkill 查询对话关联的 Skill 列表
func (d *ConversationSkillDAO) ListConversationSkill(ctx context.Context, chatID string) ([]*model.ConversationSkill, error) {
	db := d.getDB().WithContext(ctx)
	var items []*model.ConversationSkill
	if err := db.Where("chat_id = ?", chatID).Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list conversation skill failed: %w", err)
	}
	return items, nil
}

// AddConversationSkill 批量添加对话 Skill 关联（已存在的自动跳过）
func (d *ConversationSkillDAO) AddConversationSkill(ctx context.Context, chatID string, skillIDs []string) error {
	if len(skillIDs) == 0 {
		return nil
	}
	db := d.getDB().WithContext(ctx)

	items := make([]*model.ConversationSkill, 0, len(skillIDs))
	for _, skillID := range skillIDs {
		items = append(items, &model.ConversationSkill{
			ChatID:  chatID,
			SkillID: skillID,
			Enabled: true,
		})
	}

	// OnConflict{DoNothing: true} → MySQL INSERT IGNORE，并发安全
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&items).Error; err != nil {
		return fmt.Errorf("add conversation skill failed: %w", err)
	}
	return nil
}

// RemoveConversationSkill 批量移除对话 Skill 关联
func (d *ConversationSkillDAO) RemoveConversationSkill(ctx context.Context, chatID string, skillIDs []string) error {
	if len(skillIDs) == 0 {
		return nil
	}
	db := d.getDB().WithContext(ctx)
	if err := db.Where("chat_id = ? AND skill_id IN ?", chatID, skillIDs).
		Unscoped().Delete(&model.ConversationSkill{}).Error; err != nil {
		return fmt.Errorf("remove conversation skill failed: %w", err)
	}
	return nil
}

// RemoveAllConversationSkill 移除对话的所有 Skill 关联
func (d *ConversationSkillDAO) RemoveAllConversationSkill(ctx context.Context, chatID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("chat_id = ?", chatID).Unscoped().Delete(&model.ConversationSkill{}).Error; err != nil {
		return fmt.Errorf("remove all conversation skill failed: %w", err)
	}
	return nil
}

// UpdateConversationSkillEnabled 更新对话 Skill 启用状态
func (d *ConversationSkillDAO) UpdateConversationSkillEnabled(ctx context.Context, chatID, skillID string, enabled bool) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.ConversationSkill{}).
		Where("chat_id = ? AND skill_id = ?", chatID, skillID).
		Update("enabled", enabled).Error; err != nil {
		return fmt.Errorf("update conversation skill enabled failed: %w", err)
	}
	return nil
}

func (d *ConversationSkillDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
