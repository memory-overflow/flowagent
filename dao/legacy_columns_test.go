package dao

import (
	"path/filepath"
	"testing"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/model"
)

// TestDropLegacySkillColumns 存量库（旧 schema 带 type/command/args/env）
// 迁移后：遗留列被删掉、已有数据保留、新模型能正常插入；重复执行幂等。
//
// 用真实 sqlite 文件：sqlite 的 DROP COLUMN 走的是方言实现（必要时重建表），
// 只有真跑 SQL 才能确认数据不丢。
func TestDropLegacySkillColumns(t *testing.T) {
	d, _, err := open(config.DatabaseConfig{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "legacy.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	// 1. 模拟旧版本建出来的表：type NOT NULL DEFAULT 1 + command/args/env
	if err := d.Exec(`CREATE TABLE skill (
		id varchar(100) PRIMARY KEY,
		name varchar(255) NOT NULL,
		description varchar(1024),
		name_en varchar(255),
		description_en varchar(1024),
		type integer NOT NULL DEFAULT 1,
		content text,
		command varchar(512),
		args text,
		env text,
		created_time datetime NOT NULL,
		updated_time datetime NOT NULL,
		delete_time datetime
	)`).Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := d.Exec(`INSERT INTO skill (id, name, type, content, command, created_time, updated_time)
		VALUES ('s1', 'shell', 2, 'old content', '/bin/sh', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`).Error; err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// 2. 迁移：AutoMigrate + 删遗留列
	if err := d.AutoMigrate(&model.Skill{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	dropLegacyColumns(d)

	m := d.Migrator()
	for _, col := range []string{"type", "command", "args", "env"} {
		if m.HasColumn(&model.Skill{}, col) {
			t.Errorf("legacy column %q should be dropped", col)
		}
	}

	// 3. 旧数据保留
	var got model.Skill
	if err := d.Where("id = ?", "s1").First(&got).Error; err != nil {
		t.Fatalf("legacy row lost: %v", err)
	}
	if got.Name != "shell" || got.Content != "old content" {
		t.Errorf("legacy row mangled: %+v", got)
	}

	// 4. 新模型可正常插入
	if err := d.Create(&model.Skill{ID: "s2", Name: "pdf", Content: "x"}).Error; err != nil {
		t.Fatalf("insert with new model: %v", err)
	}

	// 5. 幂等：再跑一次不报错、不出问题
	dropLegacyColumns(d)
	var n int64
	d.Model(&model.Skill{}).Count(&n)
	if n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
}
