package model

import (
	"time"

	"gorm.io/gorm"
)

// ============================================================
// Skill — Skill 配置表
// ============================================================

// Skill 全局 skill 配置。
//
// 对齐行业通行的 Agent Skills 模式（SKILL.md）：skill 只是「一段指令 +
// 可选的随包脚本」，不区分类型、不注册为独立工具。Content 注入 system
// prompt，模型按指令自行拼命令，通过内置 run_command 执行随包脚本。
//
// 历史上曾有 type（prompt/command）与 command/args/env 字段（command 型
// 注册为单 input 参数的工具），已整体移除；存量库的这几列由
// dao.DropLegacySkillColumns 在迁移时删除。
//
// Name 用于内置清单按名判重（与 MCP 一致）；Content 是核心指令正文。
type Skill struct {
	ID          string `gorm:"column:id;size:100;primaryKey" json:"id"`
	Name        string `gorm:"column:name;size:255;not null" json:"name"`
	Description string `gorm:"column:description;size:1024" json:"description"`
	// NameEn/DescriptionEn 英文展示名与描述，分工同 MCPServer 的注释：
	// 中文原文给 LLM 与编辑表单，英文仅用于前端展示，为空回落中文。
	NameEn        string         `gorm:"column:name_en;size:255" json:"name_en,omitempty"`
	DescriptionEn string         `gorm:"column:description_en;size:1024" json:"description_en,omitempty"`
	Content       string         `gorm:"column:content;type:longtext" json:"content"` // markdown 指令正文
	CreatedAt     time.Time      `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedAt     time.Time      `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
	DeletedAt     gorm.DeletedAt `gorm:"column:delete_time;index" json:"-"`
}

func (Skill) TableName() string { return "skill" }
