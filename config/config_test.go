package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDBSettingsRoundTrip 系统设置的落盘与回读。
//
// 这是「改完库重启后到底生效不生效」的唯一保证：写进去的必须是读出来的
// 同一份，且缺省项要被补齐（否则重启后会出现 host 为空的半截配置）。
func TestDBSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DBConfigFileName)

	in := DatabaseConfig{
		Driver:   DriverMySQL,
		Host:     "127.0.0.1",
		Port:     3306,
		User:     "root",
		Password: "s3cret",
		DBName:   "workflow_agent_test",
	}
	if err := SaveDBSettings(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Fatalf("settings file perm = %o, want owner-only (it holds the db password)", perm)
	}

	out, err := LoadDBSettings(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out == nil {
		t.Fatal("load returned nil, want saved config")
	}
	// 注意不整体比 struct：保存/读取都会补齐缺省项（charset / dbname），
	// 这正是期望行为 —— 否则重启后会出现「host 有值、dbname 为空」的半截配置。
	for name, gotWant := range map[string][2]string{
		"driver":   {out.DriverName(), DriverMySQL},
		"host":     {out.Host, in.Host},
		"user":     {out.User, in.User},
		"password": {out.Password, in.Password},
		"dbname":   {out.DBName, in.DBName},
		"charset":  {out.Charset, "utf8mb4"},
	} {
		if gotWant[0] != gotWant[1] {
			t.Fatalf("%s = %q, want %q", name, gotWant[0], gotWant[1])
		}
	}
	if out.Port != in.Port {
		t.Fatalf("port = %d, want %d", out.Port, in.Port)
	}

	// 缺失文件的语义：nil（调用方回落 conf.yaml），而不是报错
	missing, err := LoadDBSettings(filepath.Join(dir, "nope", DBConfigFileName))
	if err != nil || missing != nil {
		t.Fatalf("missing file should be (nil, nil), got (%v, %v)", missing, err)
	}
}

// TestSQLiteDefaults 未配置时的 sqlite 默认值与 DSN。
func TestSQLiteDefaults(t *testing.T) {
	var cfg DatabaseConfig
	if cfg.DriverName() != DriverMySQL {
		t.Fatalf("empty driver should fall back to mysql, got %q", cfg.DriverName())
	}
	cfg.Driver = "SQLite" // 大小写不敏感（前端可能传任意形态）
	if cfg.DriverName() != DriverSQLite {
		t.Fatalf("driver = %q, want sqlite", cfg.DriverName())
	}
	if got := cfg.SQLitePath(); got != "./data/flowagent.db" {
		t.Fatalf("default sqlite path = %q", got)
	}
	dsn := cfg.SQLiteDSN()
	for _, want := range []string{"busy_timeout(5000)", "journal_mode(WAL)"} {
		if !contains(dsn, want) {
			t.Fatalf("dsn %q missing %q", dsn, want)
		}
	}
}

// TestLoadOverridesDatabaseWithSettings conf.yaml 与设置文件共存时，后者整体覆盖
// database 块 —— 这是「改库不用动 conf.yaml」的实现基础。
func TestLoadOverridesDatabaseWithSettings(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yamlBody := "server:\n  port: 8080\ndatabase:\n  host: cloud.example.com\n  port: 27250\n  user: root\n  password: cloudpw\n  dbname: workflow_agent_test\n"
	if err := os.WriteFile(yamlPath, []byte(yamlBody), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := SaveDBSettings(filepath.Join(dir, DBConfigFileName), DatabaseConfig{
		Driver: DriverSQLite,
		Path:   filepath.Join(dir, "local.db"),
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.DriverName() != DriverSQLite {
		t.Fatalf("driver = %q, want sqlite (settings file should win)", cfg.Database.DriverName())
	}
	if cfg.Database.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want default 127.0.0.1 (missing fields still filled)", cfg.Database.Host)
	}
	// 覆盖只作用于 database 块，其余不受影响
	if cfg.Server.Port != 8080 {
		t.Fatalf("server port = %d, want 8080", cfg.Server.Port)
	}
	if got := DBSettingsPath(); got != filepath.Join(dir, DBConfigFileName) {
		t.Fatalf("DBSettingsPath() = %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
