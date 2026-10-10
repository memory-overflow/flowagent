package dao

import (
	"context"
	"fmt"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// ModelConfigDAO 模型配置数据访问对象。
type ModelConfigDAO struct {
	Tx *gorm.DB
}

// ListModels 查询所有模型（按 is_default 降序、创建时间倒序）。
func (d *ModelConfigDAO) ListModels(ctx context.Context) ([]*model.ModelConfig, error) {
	db := d.getDB().WithContext(ctx)
	var models []*model.ModelConfig
	if err := db.Model(&model.ModelConfig{}).
		Order("is_default DESC, created_time ASC").
		Find(&models).Error; err != nil {
		return nil, fmt.Errorf("list model configs failed: %w", err)
	}
	return models, nil
}

// GetByID 按 ID 获取模型配置。
func (d *ModelConfigDAO) GetByID(ctx context.Context, id string) (*model.ModelConfig, error) {
	db := d.getDB().WithContext(ctx)
	var m model.ModelConfig
	if err := db.Where("id = ?", id).First(&m).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get model config failed: %w", err)
	}
	return &m, nil
}

// GetDefault 获取当前默认且启用的模型；无则返回 nil。
func (d *ModelConfigDAO) GetDefault(ctx context.Context) (*model.ModelConfig, error) {
	db := d.getDB().WithContext(ctx)
	var m model.ModelConfig
	if err := db.Where("is_default = ? AND enabled = ?", true, true).
		First(&m).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get default model config failed: %w", err)
	}
	return &m, nil
}

// Count 统计模型总数。
func (d *ModelConfigDAO) Count(ctx context.Context) (int64, error) {
	db := d.getDB().WithContext(ctx)
	var n int64
	if err := db.Model(&model.ModelConfig{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count model configs failed: %w", err)
	}
	return n, nil
}

// CountAvailable 统计「可用」模型数。
//
// 口径 = enabled = true 且 base_url / model 都非空。为什么还要卡这两个字段：
//
//   - 空库首次启动时后端会 seed 一条占位默认模型（见 seedDefaultModelConfig），
//     它 enabled = true 但 base_url / api_key / model 全是空串 —— 只是为了让
//     「默认模型」这个引用有落点，本身发不出任何请求。只按 enabled 判的话，
//     全新部署会被判成「已有可用模型」，引导永远不触发，用户进来第一条消息
//     必然失败，恰恰是这个功能最该拦住的场景。
//   - base_url 与 model 是 validateModelFields 里硬校验的必填项，两者都非空
//     才谈得上「配好了」。
//   - api_key 刻意不卡：本地部署（ollama 等）不需要 key，卡了会误伤。
//
// 为什么不用「测试通过」当口径：连通性测试是可选动作（用户可以直接保存），
// 拿它当门槛会把大量实际能正常对话的配置判成不可用。
func (d *ModelConfigDAO) CountAvailable(ctx context.Context) (int64, error) {
	db := d.getDB().WithContext(ctx)
	var n int64
	if err := db.Model(&model.ModelConfig{}).
		Where("enabled = ?", true).
		Where("base_url <> ''").
		Where("model <> ''").
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count available model configs failed: %w", err)
	}
	return n, nil
}

// CreateModel 创建模型配置。
func (d *ModelConfigDAO) CreateModel(ctx context.Context, m *model.ModelConfig) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Create(m).Error; err != nil {
		return fmt.Errorf("create model config failed: %w", err)
	}
	return nil
}

// UpdateModel 更新模型配置。
func (d *ModelConfigDAO) UpdateModel(ctx context.Context, id string, updates map[string]interface{}) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.ModelConfig{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return fmt.Errorf("update model config failed: %w", err)
	}
	return nil
}

// DeleteModel 删除模型配置（软删除）。
func (d *ModelConfigDAO) DeleteModel(ctx context.Context, id string) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Where("id = ?", id).Delete(&model.ModelConfig{}).Error; err != nil {
		return fmt.Errorf("delete model config failed: %w", err)
	}
	return nil
}

// ClearDefault 把某模型之外的 is_default 全部清掉。
// excludeID 非空时跳过该模型（配合「设为默认」时先清后设）。
func (d *ModelConfigDAO) ClearDefault(ctx context.Context, excludeID string) error {
	db := d.getDB().WithContext(ctx)
	q := db.Model(&model.ModelConfig{}).Where("is_default = ?", true)
	if excludeID != "" {
		q = q.Where("id <> ?", excludeID)
	}
	if err := q.Update("is_default", false).Error; err != nil {
		return fmt.Errorf("clear default model config failed: %w", err)
	}
	return nil
}

func (d *ModelConfigDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
