package model

import (
	"time"

	"gorm.io/gorm"
)

// ============================================================
// ConversationMCP — 对话与 MCP 服务器的关联表（多对多）
// ============================================================

type ConversationMCP struct {
	ID        int64          `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	ChatID    string         `gorm:"column:chat_id;size:100;not null;index:idx_chat_server,unique" json:"chat_id"`
	ServerID  string         `gorm:"column:server_id;size:100;not null;index:idx_chat_server,unique" json:"server_id"`
	Enabled   bool           `gorm:"column:enabled;not null;default:true" json:"enabled"`
	CreatedAt time.Time      `gorm:"column:created_time;not null" json:"created_time"`
	DeletedAt gorm.DeletedAt `gorm:"column:delete_time;index" json:"-"`
}

func (ConversationMCP) TableName() string { return "conversation_mcp" }
