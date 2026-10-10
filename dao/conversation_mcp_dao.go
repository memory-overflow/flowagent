package dao

import (
	"context"
	"fmt"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ConversationMCPDAO 对话-MCP 关联数据访问对象
type ConversationMCPDAO struct {
	Tx *gorm.DB
}

// ListConversationMCP 查询对话关联的 MCP 服务器列表
func (d *ConversationMCPDAO) ListConversationMCP(ctx context.Context, chatID string) ([]*model.ConversationMCP, error) {
	db := d.getDB().WithContext(ctx)
	var items []*model.ConversationMCP
	if err := db.Where("chat_id = ?", chatID).Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list conversation mcp failed: %w", err)
	}
	return items, nil
}

// AddConversationMCP 批量添加对话 MCP 关联（已存在的自动跳过）
func (d *ConversationMCPDAO) AddConversationMCP(ctx context.Context, chatID string, serverIDs []string) error {
	if len(serverIDs) == 0 {
		return nil
	}
	db := d.getDB().WithContext(ctx)

	items := make([]*model.ConversationMCP, 0, len(serverIDs))
	for _, serverID := range serverIDs {
		items = append(items, &model.ConversationMCP{
			ChatID:   chatID,
			ServerID: serverID,
			Enabled:  true,
		})
	}

	// OnConflict{DoNothing: true} → MySQL INSERT IGNORE，并发安全
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&items).Error; err != nil {
		return fmt.Errorf("add conversation mcp failed: %w", err)
	}
	return nil
}

// RemoveConversationMCP 批量移除对话 MCP 关联
func (d *ConversationMCPDAO) RemoveConversationMCP(ctx context.Context, chatID string, serverIDs []string) error {
	if len(serverIDs) == 0 {
		return nil
	}
	db := d.getDB().WithContext(ctx)
	if err := db.Where("chat_id = ? AND server_id IN ?", chatID, serverIDs).
		Unscoped().Delete(&model.ConversationMCP{}).Error; err != nil {
		return fmt.Errorf("remove conversation mcp failed: %w", err)
	}
	return nil
}

// RemoveAllConversationMCP 移除对话的所有 MCP 关联
func (d *ConversationMCPDAO) RemoveAllConversationMCP(ctx context.Context, chatID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("chat_id = ?", chatID).Unscoped().Delete(&model.ConversationMCP{}).Error; err != nil {
		return fmt.Errorf("remove all conversation mcp failed: %w", err)
	}
	return nil
}

// UpdateConversationMCPEnabled 更新对话 MCP 启用状态
func (d *ConversationMCPDAO) UpdateConversationMCPEnabled(ctx context.Context, chatID, serverID string, enabled bool) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.ConversationMCP{}).
		Where("chat_id = ? AND server_id = ?", chatID, serverID).
		Update("enabled", enabled).Error; err != nil {
		return fmt.Errorf("update conversation mcp enabled failed: %w", err)
	}
	return nil
}

func (d *ConversationMCPDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
