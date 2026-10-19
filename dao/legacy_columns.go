package dao

import (
	"log"

	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// legacyColumns 已从模型移除、但存量库里还残留的列。
//
// GORM AutoMigrate 只增不删：字段从 struct 里删掉后，旧库的列会一直留着。
// 对 NOT NULL 且无默认值的残留列，新代码的 INSERT 不带它会直接失败，所以
// 必须显式删除。这里集中登记，迁移时逐列「存在才删」，可重复执行。
var legacyColumns = []struct {
	model   interface{}
	table   string
	columns []string
}{
	// skill 不再区分 prompt/command 类型（对齐行业 Agent Skills 模式，
	// 全部 skill 只注入 prompt、脚本经 run_command 执行），type 与只在
	// command 型下有意义的 command/args/env 一并移除。
	{&model.Skill{}, "skill", []string{"type", "command", "args", "env"}},
}

// dropLegacyColumns 删除 legacyColumns 里登记、且当前库中仍存在的列。
//
// 失败只记日志不中断启动：server 与 scheduler 启动时会并发迁移，另一侧
// 可能刚好删掉了同一列（HasColumn 与 DropColumn 之间的竞态），此时报错
// 是无害的；列真的没删掉也不影响读写（残留列都可空或有默认值）。
func dropLegacyColumns(d *gorm.DB) {
	m := d.Migrator()
	sqlite := d.Dialector.Name() == "sqlite"
	for _, lc := range legacyColumns {
		if !m.HasTable(lc.table) {
			continue
		}
		for _, col := range lc.columns {
			if !m.HasColumn(lc.model, col) {
				continue
			}
			var err error
			if sqlite {
				// 不用 Migrator().DropColumn：glebarez/sqlite 的实现是解析建表 DDL、
				// 用正则摘掉列再重建表，而正则要求列名带引号或前导空格 —— 列名
				// 没加引号时匹配不上，却仍返回 nil（静默什么都没删）。直接用
				// SQLite 3.35+ 原生的 DROP COLUMN，不依赖 DDL 文本格式。
				err = d.Exec("ALTER TABLE `" + lc.table + "` DROP COLUMN `" + col + "`").Error
			} else {
				err = m.DropColumn(lc.model, col)
			}
			if m.HasColumn(lc.model, col) {
				log.Printf("[dao] drop legacy column %s.%s failed: %v", lc.table, col, err)
				continue
			}
			log.Printf("[dao] dropped legacy column %s.%s", lc.table, col)
		}
	}
}
