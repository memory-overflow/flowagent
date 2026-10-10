package dao

import (
	"context"
	"fmt"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// SkillDAO Skill 数据访问对象
type SkillDAO struct {
	Tx *gorm.DB
}

// ListSkills 查询 Skill 列表
func (d *SkillDAO) ListSkills(ctx context.Context, keyword string) ([]*model.Skill, error) {
	db := d.getDB().WithContext(ctx)
	query := db.Model(&model.Skill{})
	if keyword != "" {
		query = query.Where("name LIKE ? OR description LIKE ?",
			fmt.Sprintf("%%%s%%", keyword), fmt.Sprintf("%%%s%%", keyword))
	}
	var skills []*model.Skill
	if err := query.Order("created_time DESC").Find(&skills).Error; err != nil {
		return nil, fmt.Errorf("list skills failed: %w", err)
	}
	return skills, nil
}

// GetSkillByID 根据 ID 获取 Skill
func (d *SkillDAO) GetSkillByID(ctx context.Context, skillID string) (*model.Skill, error) {
	db := d.getDB().WithContext(ctx)
	var skill model.Skill
	if err := db.Where("id = ?", skillID).First(&skill).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get skill failed: %w", err)
	}
	return &skill, nil
}

// CreateSkill 创建 Skill
func (d *SkillDAO) CreateSkill(ctx context.Context, skill *model.Skill) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Create(skill).Error; err != nil {
		return fmt.Errorf("create skill failed: %w", err)
	}
	return nil
}

// UpdateSkill 更新 Skill
func (d *SkillDAO) UpdateSkill(ctx context.Context, skillID string, updates map[string]interface{}) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.Skill{}).Where("id = ?", skillID).Updates(updates).Error; err != nil {
		return fmt.Errorf("update skill failed: %w", err)
	}
	return nil
}

// DeleteSkill 删除 Skill（软删除）
func (d *SkillDAO) DeleteSkill(ctx context.Context, skillID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("id = ?", skillID).Delete(&model.Skill{}).Error; err != nil {
		return fmt.Errorf("delete skill failed: %w", err)
	}
	return nil
}

// GetSkillsByIDs 批量获取 Skill 信息
func (d *SkillDAO) GetSkillsByIDs(ctx context.Context, skillIDs []string) ([]*model.Skill, error) {
	if len(skillIDs) == 0 {
		return nil, nil
	}
	db := d.getDB().WithContext(ctx)
	var skills []*model.Skill
	if err := db.Where("id IN ?", skillIDs).Find(&skills).Error; err != nil {
		return nil, fmt.Errorf("get skills by ids failed: %w", err)
	}
	return skills, nil
}

func (d *SkillDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
