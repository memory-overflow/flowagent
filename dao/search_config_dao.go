package dao

import (
	"context"
	"fmt"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SearchConfigDAO 检索引擎全局配置数据访问对象。
//
// 与 ModelConfigDAO 的「多行 CRUD」不同，search_config 表只有一行（固定主键
// global）。因此 DAO 只提供 GetOrCreate / Get / Update 三个幂等方法，不做删除。
type SearchConfigDAO struct {
	Tx *gorm.DB
}

// GetOrCreate 取当前配置；不存在时插入默认行（engine=baidu）后返回。
//
// 用 OnConflict DoNothing 防并发双写：两个 scheduler/server 同时首启时，
// 只有一方真正插入，另一方跳过插入转而去读已有行，不会因主键冲突报错。
func (d *SearchConfigDAO) GetOrCreate(ctx context.Context) (*model.SearchConfig, error) {
	db := d.getDB().WithContext(ctx)
	now := time.Now()
	seed := &model.SearchConfig{
		ID:        model.SearchConfigGlobalID,
		Engine:    model.SearchEngineBaidu,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(seed).Error; err != nil {
		return nil, fmt.Errorf("ensure search config failed: %w", err)
	}
	return d.Get(ctx)
}

// Get 取当前配置；无记录返回 nil。
func (d *SearchConfigDAO) Get(ctx context.Context) (*model.SearchConfig, error) {
	db := d.getDB().WithContext(ctx)
	var cfg model.SearchConfig
	if err := db.Where("id = ?", model.SearchConfigGlobalID).First(&cfg).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get search config failed: %w", err)
	}
	return &cfg, nil
}

// Update 幂等更新单行配置（updates 为要改的列，键用 gorm 列名）。
func (d *SearchConfigDAO) Update(ctx context.Context, updates map[string]interface{}) error {
	db := d.getDB().WithContext(ctx)
	if err := db.Model(&model.SearchConfig{}).
		Where("id = ?", model.SearchConfigGlobalID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("update search config failed: %w", err)
	}
	return nil
}

func (d *SearchConfigDAO) getDB() *gorm.DB {
	if d.Tx != nil {
		return d.Tx
	}
	return DB()
}
