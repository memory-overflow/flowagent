package model

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// ============================================================
// 通用 JSON 字段类型 — 对标 weaver-backend model.Json[T]
// ============================================================

var _ sql.Scanner = (*JsonField[any])(nil)
var _ driver.Valuer = JsonField[any]{}

// JsonField 通用 JSON 字段，可用于 gorm model 直接存取 JSON 数据
type JsonField[T any] struct {
	Data T
}

// Scan 实现 sql.Scanner
func (j *JsonField[T]) Scan(src any) error {
	var zero T
	j.Data = zero
	if src == nil {
		return nil
	}
	var bytes []byte
	switch v := src.(type) {
	case string:
		bytes = []byte(v)
	case []byte:
		bytes = v
	case time.Time:
		bytes, _ = json.Marshal(v)
	default:
		bytes, _ = json.Marshal(v)
	}
	if len(bytes) > 0 {
		return json.Unmarshal(bytes, &j.Data)
	}
	return nil
}

// Value 实现 driver.Valuer
func (j JsonField[T]) Value() (driver.Value, error) {
	return json.Marshal(j.Data)
}

// ============================================================
// Conversation — 对话表
// ============================================================

// ConversationStatus 对话状态
type ConversationStatus int

const (
	ConversationStatusActive    ConversationStatus = 1
	ConversationStatusCompleted ConversationStatus = 2
	ConversationStatusArchived  ConversationStatus = 3
)

// Conversation 对话
type Conversation struct {
	ChatID string             `gorm:"column:chat_id;size:100;primaryKey" json:"chat_id"`
	Title  string             `gorm:"column:title;size:512;not null" json:"title"`
	Status ConversationStatus `gorm:"column:status;not null;default:1" json:"status"`
	// CreateUser 所属用户。idx_conv_user_created 覆盖列表页按用户 + 创建时间
	// 瀑布流分页（ListConversations / CountConversations）的查询，避免全表扫描。
	CreateUser string `gorm:"column:create_user;size:100;not null;index:idx_conv_user_created,priority:1" json:"create_user"`
	// WorkflowTaskID 本对话「当前激活」的工作流任务 ID（task.task_type = TaskTypeWorkflow）。
	//
	// 一个对话可以绑定多个工作流（chat_id + task_type=2 的 task 行即全集），
	// 本列只记录最近一次新建/追加所针对的那一个 —— 前端默认展示它，
	// 用户可在工作流列表里切换查看其它工作流。
	//
	// 语义变化：v1.4 之前它是「唯一绑定的工作流」，v1.4 起改为「激活态」。
	// 每次新建工作流或在某工作流上追加节点后都会把它指向该工作流，
	// 这样用户新提的需求总能立刻看到对应的 DAG 变化。
	//
	// 只存 ID 不存 DAG 本身：DAG 的唯一真相是工作流任务的 task_output，
	// 两处都存必然出现更新不同步（节点执行状态变化时尤其明显）。
	WorkflowTaskID string `gorm:"column:workflow_task_id;size:100" json:"workflow_task_id,omitempty"`
	// ModelID 本对话选用的模型配置 ID（model_config.id）。
	// 为空表示未显式指定，解析时回落到默认模型（is_default=1），再回落到 config.LLM。
	// 用 not null;default:'' 而非可空列，避免存量行 NULL 直接扫进 string 报错
	// （见 GetWorkflowTaskID 的 NullString 注释）。
	ModelID   string         `gorm:"column:model_id;size:100;not null;default:''" json:"model_id,omitempty"`
	CreatedAt time.Time      `gorm:"column:created_time;not null;index:idx_conv_user_created,priority:2" json:"created_time"`
	UpdatedAt time.Time      `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
	DeletedAt gorm.DeletedAt `gorm:"column:delete_time;index" json:"-"`
}

func (Conversation) TableName() string { return "conversation" }
