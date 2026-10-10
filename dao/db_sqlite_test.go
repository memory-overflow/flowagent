package dao

import (
	"path/filepath"
	"testing"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm/clause"
)

// TestSQLiteDialect 验证「系统设置切到 sqlite」后整条链路能跑通：
//
//  1. 全部模型能 AutoMigrate（模型里有 MySQL 口径的 type:longtext / size 等，
//     sqlite 侧必须都能建出来，否则切过去第一件事就是启动失败）；
//  2. 带 clause.Locking 的查询不报错 —— sqlite 没有 SELECT ... FOR UPDATE
//     语法，若 gorm 不做方言屏蔽，AppendSubTaskParentTurnIDs 会直接炸。
//
// 用真实 sqlite 文件（放在临时目录）而不是 mock：方言问题只有真跑 SQL 才暴露。
func TestSQLiteDialect(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DatabaseConfig{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(dir, "sub", "agent.db"), // 故意带一层不存在的子目录
	}

	d, drv, err := open(cfg)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if drv != config.DriverSQLite {
		t.Fatalf("driver = %q, want sqlite", drv)
	}

	models := []interface{}{
		&model.Conversation{}, &model.Task{}, &model.TaskTemp{}, &model.TaskTurn{},
		&model.MCPServer{}, &model.ConversationMCP{}, &model.Skill{}, &model.ConversationSkill{},
		&model.ModelConfig{}, &model.SearchConfig{}, &model.ContextSummary{}, &model.TaskContextSummary{},
	}
	if err := d.AutoMigrate(models...); err != nil {
		t.Fatalf("automigrate on sqlite: %v", err)
	}

	// FOR UPDATE：sqlite 不支持，必须被静默忽略而不是报错。
	var n int64
	err = d.Model(&model.Task{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("task_id = ?", "not-exist").Count(&n).Error
	if err != nil {
		t.Fatalf("query with FOR UPDATE on sqlite: %v", err)
	}
	if n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
}
