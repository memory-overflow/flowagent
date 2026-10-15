package dao

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/glebarez/sqlite" // 纯 Go 实现（modernc.org/sqlite），不依赖 CGO
	"github.com/memory-overflow/flowagent/config"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	dbMu sync.RWMutex
	db   *gorm.DB
	// currentDriver 与 db 同锁保护。读方一律走 Driver()/IsSQLite()，
	// 不要直接引用变量。
	currentDriver string
)

// Init 按配置里的驱动初始化数据库连接（进程内只做一次）。
//
// 两种驱动由「系统设置」决定（config/db_config.json，见 config 包）：
//   - mysql：云库或本机 MySQL。云上库延迟高的场景，改指本机即可；
//   - sqlite：单机文件库，没有网络往返，本地开发最省心的选择。
func Init(cfg config.DatabaseConfig) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if db != nil {
		return nil
	}
	return reloadLocked(cfg)
}

// Reload 热切换数据库连接（「系统设置」保存后不重启直接生效）。
//
// 顺序有讲究：先建新池、后换指针、旧池延迟关闭 ——
//   - 新池建失败直接返回错误，当前连接不受影响（保存接口借此拦下坏配置）；
//   - 在途请求手里握着旧 *gorm.DB，指针一换它们照常在旧库上跑完，
//     延迟 30s 再关旧池给它们留足收尾时间（单次查询都是亚秒级）。
//
// 调用方负责换库后的 Migrate（server 保存接口 / scheduler watcher 各有
// 自己的表清单，dao 层不关心）。
func Reload(cfg config.DatabaseConfig) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	return reloadLocked(cfg)
}

// reloadLocked 建新池并替换全局指针。调用方须持有写锁。
func reloadLocked(cfg config.DatabaseConfig) error {
	d, drv, err := open(cfg)
	if err != nil {
		return err
	}
	old, oldDriver := db, currentDriver
	db, currentDriver = d, drv

	if old != nil {
		// 旧池延迟关闭：在途请求（拿着旧 *gorm.DB）跑完再收。
		// goroutine 里做，不阻塞切换；Close 失败无碍——进程退出也会回收。
		go func() {
			time.Sleep(30 * time.Second)
			if sqlDB, err := old.DB(); err == nil {
				sqlDB.Close()
			}
			log.Printf("[dao] closed old %s connection pool", oldDriver)
		}()
	}
	log.Printf("[dao] database switched to %s", cfg.Desc())
	return nil
}

// open 按驱动建立连接并完成连接池配置。
func open(cfg config.DatabaseConfig) (*gorm.DB, string, error) {
	gormCfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Info)}
	drv := cfg.DriverName()

	var dialector gorm.Dialector
	switch drv {
	case config.DriverSQLite:
		path := cfg.SQLitePath()
		// sqlite 不会自己建目录；路径不带目录时 Dir 是 "."，MkdirAll 是安全的
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, drv, fmt.Errorf("create sqlite dir %s: %w", filepath.Dir(path), err)
		}
		dialector = sqlite.Open(cfg.SQLiteDSN())
	default:
		dialector = mysql.Open(cfg.DSN())
	}

	d, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		return nil, drv, fmt.Errorf("failed to connect database (%s): %w", cfg.Desc(), err)
	}
	sqlDB, err := d.DB()
	if err != nil {
		return nil, drv, fmt.Errorf("failed to get sql db: %w", err)
	}
	if drv == config.DriverSQLite {
		// sqlite 的连接池关注点完全不同：
		//   - 写操作全局排他（WAL 下仍只允许一个写者），开太多连接只会让它们
		//     互相等锁；锁冲突由 busy_timeout 兜底（写在 SQLiteDSN 里）。
		//   - 连接不过期：文件句柄没有「半开连接」问题，回收反而会反复重开
		//     文件、丢掉 WAL 缓存。
		//   - 上限取 8 而不是 1：事务内若再走全局 db 查询（历史代码里有这种
		//     用法），单连接会把自己锁死到超时。
		sqlDB.SetMaxOpenConns(8)
		sqlDB.SetMaxIdleConns(4)
		sqlDB.SetConnMaxLifetime(0)
		sqlDB.SetConnMaxIdleTime(0)
	} else {
		// MySQL 侧：不设默认 MaxOpenConns 无上限、连接永不过期，
		// 公网访问下空闲连接会被静默断开形成半开连接，复用后卡死。
		// 这里限制连接数并定期回收空闲/过期连接，配合 DSN 的读写超时双保险。
		sqlDB.SetMaxOpenConns(50)
		sqlDB.SetMaxIdleConns(10)
		sqlDB.SetConnMaxLifetime(5 * time.Minute)
		sqlDB.SetConnMaxIdleTime(1 * time.Minute)
	}
	return d, drv, nil
}

// Ping 用给定配置试连一次数据库（「系统设置」页的连通性测试用）。
//
// 不跑迁移、不动进程内连接：测试的目的只是回答「这套参数能不能连上」，
// 连完立刻关闭。sqlite 会被动创建文件（这也是测试的一部分 —— 路径写不了
// 就趁早报错）。
func Ping(cfg config.DatabaseConfig) error {
	d, _, err := open(cfg)
	if err != nil {
		return err
	}
	sqlDB, err := d.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	return sqlDB.Ping()
}

// DB 获取数据库连接。每次调用取当前指针：热切换后新请求自然落到新库，
// 在途请求手里的旧指针由 Reload 的延迟关池保证可用。
func DB() *gorm.DB {
	dbMu.RLock()
	defer dbMu.RUnlock()
	return db
}

// Driver 返回当前生效的数据库驱动（mysql / sqlite）。
//
// 未初始化时返回空串。供少量需要区分方言的代码用（如 sqlite 不支持
// SELECT ... FOR UPDATE）。
func Driver() string {
	dbMu.RLock()
	defer dbMu.RUnlock()
	return currentDriver
}

// IsSQLite 当前是否为 sqlite 驱动。
func IsSQLite() bool {
	return Driver() == config.DriverSQLite
}

// Ready 数据库是否已完成初始化。
//
// 给「有则记、无则跳过」的旁路留痕用（如上下文压缩的调用记录）：
// 那些路径在单测里没有 DB，直接调 DB() 会拿到 nil 并 panic。
func Ready() bool {
	dbMu.RLock()
	defer dbMu.RUnlock()
	return db != nil
}

// Transaction 执行数据库事务
func Transaction(fn func(tx *gorm.DB) error) error {
	d := DB()
	if d == nil {
		return fmt.Errorf("database not initialized")
	}
	return d.Transaction(fn)
}

// Migrate 自动迁移表结构，并清理已从模型移除的遗留列（见 legacy_columns.go）。
func Migrate(models ...interface{}) error {
	d := DB()
	if d == nil {
		return fmt.Errorf("database not initialized")
	}
	if err := d.AutoMigrate(models...); err != nil {
		return err
	}
	dropLegacyColumns(d)
	return nil
}
