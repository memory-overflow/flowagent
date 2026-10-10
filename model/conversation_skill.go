package model

import (
	"time"

	"gorm.io/gorm"
)

// ============================================================
// ConversationSkill — 对话与 Skill 的关联表（多对多）
// ============================================================

type ConversationSkill struct {
	ID        int64          `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	ChatID    string         `gorm:"column:chat_id;size:100;not null;index:idx_chat_skill,unique" json:"chat_id"`
	SkillID   string         `gorm:"column:skill_id;size:100;not null;index:idx_chat_skill,unique" json:"skill_id"`
	Enabled   bool           `gorm:"column:enabled;not null;default:true" json:"enabled"`
	CreatedAt time.Time      `gorm:"column:created_time;not null" json:"created_time"`
	DeletedAt gorm.DeletedAt `gorm:"column:delete_time;index" json:"-"`
}

func (ConversationSkill) TableName() string { return "conversation_skill" }
