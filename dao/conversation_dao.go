package dao

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// ConversationDAO 对话数据访问对象
type ConversationDAO struct {
	Tx *gorm.DB
}

// CountConversations 查询用户对话总数
func (d *ConversationDAO) CountConversations(ctx context.Context, userID string) (int64, error) {
	db := d.getDB()
	var total int64
	if err := db.WithContext(ctx).Model(&model.Conversation{}).
		Where("create_user = ?", userID).Count(&total).Error; err != nil {
		return 0, fmt.Errorf("count conversations failed: %w", err)
	}
	return total, nil
}

// ListConversations 查询用户对话列表（瀑布流分页）
func (d *ConversationDAO) ListConversations(
	ctx context.Context, userID string, lastTs int64, keyword string, pageSize int,
) ([]*model.Conversation, int64, error) {
	db := d.getDB().WithContext(ctx)

	query := db.Model(&model.Conversation{}).Where("create_user = ?", userID)

	// 瀑布流分页：上一页最后一条数据的时间戳（毫秒）
	if lastTs > 0 {
		query = query.Where("created_time < ?", parseTsToTime(lastTs))
	}

	// 关键词模糊搜索
	if keyword != "" {
		query = query.Where("title LIKE ?", fmt.Sprintf("%%%s%%", keyword))
	}

	// 统计总数
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count conversations failed: %w", err)
	}

	// 查询列表，按创建时间倒序
	var conversations []*model.Conversation
	if err := query.Order("created_time DESC").Limit(pageSize).Find(&conversations).Error; err != nil {
		return nil, 0, fmt.Errorf("list conversations failed: %w", err)
	}

	return conversations, total, nil
}

// CreateConversation 创建新对话
func (d *ConversationDAO) CreateConversation(ctx context.Context, conv *model.Conversation) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Create(conv).Error; err != nil {
		return fmt.Errorf("create conversation failed: %w", err)
	}
	return nil
}

// GetConversationByID 根据 chat_id 获取对话
func (d *ConversationDAO) GetConversationByID(ctx context.Context, chatID string) (*model.Conversation, error) {
	db := d.getDB().WithContext(ctx)
	var conv model.Conversation
	if err := db.Where("chat_id = ?", chatID).First(&conv).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get conversation failed: %w", err)
	}
	return &conv, nil
}

// UpdateConversationModel 修改对话选用的模型（model_config.id）。
// modelID 传空串表示「恢复默认」，解析时回落到默认模型再回落到 config.LLM。
func (d *ConversationDAO) UpdateConversationModel(ctx context.Context, chatID, modelID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.Conversation{}).Where("chat_id = ?", chatID).
		Update("model_id", modelID).Error; err != nil {
		return fmt.Errorf("update conversation model failed: %w", err)
	}
	return nil
}

// UpdateConversationTitle 修改对话标题
func (d *ConversationDAO) UpdateConversationTitle(ctx context.Context, chatID, title string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.Conversation{}).Where("chat_id = ?", chatID).
		Update("title", title).Error; err != nil {
		return fmt.Errorf("update conversation title failed: %w", err)
	}
	return nil
}

// GetActiveWorkflowTaskID 读取对话「当前激活」的工作流任务 ID（无则返回空串）。
//
// v1.4 起一个对话可有多个工作流，本列只表示最近一次新建/追加所针对的那一个，
// 前端默认展示它（其余通过工作流列表切换查看）。
//
// 用 sql.NullString 而非 string 承接：workflow_task_id 是后加的列，
// AutoMigrate 给存量对话行填的是 NULL，直接扫进 string 会报
// 「converting NULL to string is unsupported」。
// 语义上 NULL 与空串都表示「尚无任何工作流」，统一归一成空串。
func (d *ConversationDAO) GetActiveWorkflowTaskID(ctx context.Context, chatID string) (string, error) {
	db := d.getDB().WithContext(ctx)
	var taskID sql.NullString
	if err := db.Model(&model.Conversation{}).Where("chat_id = ?", chatID).
		Select("workflow_task_id").Scan(&taskID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return "", nil
		}
		return "", fmt.Errorf("get conversation workflow task id failed: %w", err)
	}
	return taskID.String, nil
}

// SetActiveWorkflowTask 把某个工作流任务设为对话的「当前激活」工作流。
//
// 与 v1.3 的 BindWorkflowTask 不同：那时一个对话只能有一个工作流，需要
// 「仅在未绑定时生效」的 CAS 来防止并发建出两条。现在允许多个工作流，
// 新建永远成功，不存在争用，因此改成无条件覆盖 —— 每次新建或在某条上
// 追加节点后都把激活态指过去，用户就能立刻看到刚发生变化的那条 DAG。
func (d *ConversationDAO) SetActiveWorkflowTask(
	ctx context.Context, chatID, taskID string,
) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.Conversation{}).Where("chat_id = ?", chatID).
		Updates(map[string]interface{}{
			"workflow_task_id": taskID,
			"updated_time":     time.Now(),
		}).Error; err != nil {
		return fmt.Errorf("set active workflow task failed: %w", err)
	}
	return nil
}

// DeleteConversation 删除对话（软删除）
func (d *ConversationDAO) DeleteConversation(ctx context.Context, chatID string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("chat_id = ?", chatID).Delete(&model.Conversation{}).Error; err != nil {
		return fmt.Errorf("delete conversation failed: %w", err)
	}
	return nil
}

// getDB 获取 DB 实例（支持事务注入）
func (d *ConversationDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}

// parseTsToTime 毫秒时间戳 → time
func parseTsToTime(ts int64) interface{} {
	return ts // gorm 会自行处理
}
