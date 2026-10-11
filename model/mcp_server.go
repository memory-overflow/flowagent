package model

import (
	"time"

	"gorm.io/gorm"
)

// ============================================================
// MCPServerStatus — MCP 连接状态枚举
// ============================================================

type MCPServerStatus int

const (
	MCPServerStatusConnected    MCPServerStatus = 1
	MCPServerStatusDisconnected MCPServerStatus = 2
	MCPServerStatusError        MCPServerStatus = 3
)

// ============================================================
// MCPServer — MCP 服务器配置表
// ============================================================

type MCPServer struct {
	ID          string `gorm:"column:id;size:100;primaryKey" json:"id"`
	Name        string `gorm:"column:name;size:255;not null" json:"name"`
	Description string `gorm:"column:description;size:1024" json:"description"`
	// NameEn/DescriptionEn 英文展示名与描述。两套字段分工：
	//   - 中文原文（Name/Description）：给 LLM 的 prompt、编辑表单回填、英文缺失时的回落值。
	//   - 英文（*En）：仅用于前端英文界面展示，允许为空（回落中文）。
	// 内置清单导入时写入；用户手建的可后续在编辑表单补填。
	DescriptionEn string          `gorm:"column:description_en;size:1024" json:"description_en,omitempty"`
	Transport     string          `gorm:"column:transport;size:50;not null" json:"transport"` // "stdio" | "sse" | "streamable_http"
	Command       string          `gorm:"column:command;size:512" json:"command,omitempty"`
	Args          string          `gorm:"column:args;type:text" json:"args,omitempty"` // JSON array
	Env           string          `gorm:"column:env;type:text" json:"env,omitempty"`   // JSON map
	URL           string          `gorm:"column:url;size:1024" json:"url,omitempty"`
	Headers       string          `gorm:"column:headers;type:text" json:"headers,omitempty"` // JSON map
	Status        MCPServerStatus `gorm:"column:status;not null;default:2" json:"status"`
	ErrorMsg      string          `gorm:"column:error_msg;size:1024" json:"error_msg,omitempty"`
	ToolsCount    int32           `gorm:"column:tools_count;not null;default:0" json:"tools_count"`
	CreatedAt     time.Time       `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedAt     time.Time       `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
	DeletedAt     gorm.DeletedAt  `gorm:"column:delete_time;index" json:"-"`
}

func (MCPServer) TableName() string { return "mcp_server" }
