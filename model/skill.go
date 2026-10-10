package model

import (
	"time"

	"gorm.io/gorm"
)

// ============================================================
// SkillType — Skill 类型枚举
// ============================================================
//
// Skill 是可复用的能力包，按「是否有可执行命令」分两类：
//   - prompt  指令型：只有一段 markdown 指令，注入 system prompt，
//     由模型遵循指令、结合 MCP 工具或自身知识完成任务。
//   - command 可执行型：额外携带 shell 命令，作为工具暴露给模型，
//     模型可主动调用它，后端真正执行命令并回传 stdout。
type SkillType int

const (
	SkillTypePrompt  SkillType = 1 // 指令型：仅注入 system prompt
	SkillTypeCommand SkillType = 2 // 可执行型：暴露为工具，可被调用执行
)

// ============================================================
// Skill — Skill 配置表
// ============================================================

// Skill 全局 skill 配置。
//
// Name 用于内置清单按名判重（与 MCP 一致）；Content 是核心指令正文，
// Command/Args/Env 仅在 type=command 时有意义。
type Skill struct {
	ID          string         `gorm:"column:id;size:100;primaryKey" json:"id"`
	Name        string         `gorm:"column:name;size:255;not null" json:"name"`
	Description string         `gorm:"column:description;size:1024" json:"description"`
	// NameEn/DescriptionEn 英文展示名与描述，分工同 MCPServer 的注释：
	// 中文原文给 LLM 与编辑表单，英文仅用于前端展示，为空回落中文。
	NameEn        string `gorm:"column:name_en;size:255" json:"name_en,omitempty"`
	DescriptionEn string `gorm:"column:description_en;size:1024" json:"description_en,omitempty"`
	Type        SkillType      `gorm:"column:type;not null;default:1" json:"type"`
	Content     string         `gorm:"column:content;type:longtext" json:"content"` // markdown 指令正文
	Command     string         `gorm:"column:command;size:512" json:"command,omitempty"`
	Args        string         `gorm:"column:args;type:text" json:"args,omitempty"` // JSON array
	Env         string         `gorm:"column:env;type:text" json:"env,omitempty"`   // JSON map
	CreatedAt   time.Time      `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedAt   time.Time      `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
	DeletedAt   gorm.DeletedAt `gorm:"column:delete_time;index" json:"-"`
}

func (Skill) TableName() string { return "skill" }
