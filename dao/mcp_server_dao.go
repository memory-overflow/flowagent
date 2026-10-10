package dao

import (
	"context"
	"fmt"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// MCPServerDAO MCP 服务器数据访问对象
type MCPServerDAO struct {
	Tx *gorm.DB
}

// ListServers 查询 MCP 服务器列表
func (d *MCPServerDAO) ListServers(ctx context.Context, keyword string) ([]*model.MCPServer, error) {
	db := d.getDB().WithContext(ctx)
	query := db.Model(&model.MCPServer{})
	if keyword != "" {
		query = query.Where("name LIKE ? OR description LIKE ?",
			fmt.Sprintf("%%%s%%", keyword), fmt.Sprintf("%%%s%%", keyword))
	}
	var servers []*model.MCPServer
	if err := query.Order("created_time DESC").Find(&servers).Error; err != nil {
		return nil, fmt.Errorf("list mcp servers failed: %w", err)
	}
	return servers, nil
}

// GetServerByID 根据 ID 获取服务器信息
func (d *MCPServerDAO) GetServerByID(ctx context.Context, serverID string) (*model.MCPServer, error) {
	db := d.getDB().WithContext(ctx)
	var server model.MCPServer
	if err := db.Where("id = ?", serverID).First(&server).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get mcp server failed: %w", err)
	}
	return &server, nil
}

// CreateServer 创建 MCP 服务器
func (d *MCPServerDAO) CreateServer(ctx context.Context, server *model.MCPServer) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Create(server).Error; err != nil {
		return fmt.Errorf("create mcp server failed: %w", err)
	}
	return nil
}

// UpdateServer 更新 MCP 服务器
func (d *MCPServerDAO) UpdateServer(ctx context.Context, serverID string, updates map[string]interface{}) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.MCPServer{}).Where("id = ?", serverID).Updates(updates).Error; err != nil {
		return fmt.Errorf("update mcp server failed: %w", err)
	}
	return nil
}

// DeleteServer 删除 MCP 服务器（软删除）
func (d *MCPServerDAO) DeleteServer(ctx context.Context, serverID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("id = ?", serverID).Delete(&model.MCPServer{}).Error; err != nil {
		return fmt.Errorf("delete mcp server failed: %w", err)
	}
	return nil
}

// GetServersByIDs 批量获取服务器信息
func (d *MCPServerDAO) GetServersByIDs(ctx context.Context, serverIDs []string) ([]*model.MCPServer, error) {
	if len(serverIDs) == 0 {
		return nil, nil
	}
	db := d.getDB().WithContext(ctx)
	var servers []*model.MCPServer
	if err := db.Where("id IN ?", serverIDs).Find(&servers).Error; err != nil {
		return nil, fmt.Errorf("get mcp servers by ids failed: %w", err)
	}
	return servers, nil
}

// UpdateServerStatus 更新 MCP 服务器的连接状态和工具数量
func (d *MCPServerDAO) UpdateServerStatus(ctx context.Context, serverID string, status model.MCPServerStatus, toolsCount int32, errorMsg string) error {
	db := d.getDB().WithContext(ctx)
	updates := map[string]interface{}{
		"status":      status,
		"tools_count": toolsCount,
		"error_msg":   errorMsg,
	}
	if err := db.Model(&model.MCPServer{}).Where("id = ?", serverID).Updates(updates).Error; err != nil {
		return fmt.Errorf("update mcp server status failed: %w", err)
	}
	return nil
}

func (d *MCPServerDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
