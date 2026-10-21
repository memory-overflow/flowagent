// Package config 提供 YAML 配置文件解析
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// 支持的数据库驱动。
const (
	DriverMySQL  = "mysql"
	DriverSQLite = "sqlite"
)

// DBConfigFileName 数据库「系统设置」的持久化文件名。
//
// 它必须与 conf.yaml 分开：数据库连接参数本身不能存在数据库里（要先连上库
// 才能读设置，而读设置又为了决定连哪个库），只能落在本地磁盘。单独成文件
// 的另一个好处是密码不进 conf.yaml，也就不会随代码提交。
//
// 存放位置与 conf.yaml 同级（默认 config/db_config.json），server 与
// scheduler 启动时都从同一份文件读，保证两个进程用的是同一个库。
const DBConfigFileName = "db_config.json"

// AgentSettingsFileName 保存可在运行时调整的 Agent 循环预算。
const AgentSettingsFileName = "agent_settings.json"

const (
	DefaultPlannerMaxToolCallRounds     = 4
	MinPlannerMaxToolCallRounds         = 1
	MaxPlannerMaxToolCallRounds         = 20
	DefaultSingleAgentMaxToolCallRounds = 100
	MinSingleAgentMaxToolCallRounds     = 1
	MaxSingleAgentMaxToolCallRounds     = 500
)

// AgentSettings 是规划器与单 Agent 各自的工具调用轮次预算。
// 独立落盘，scheduler 每轮直接读取，因此保存后无需重启即可影响后续流转。
type AgentSettings struct {
	PlannerMaxToolCallRounds     int `json:"planner_max_tool_call_rounds"`
	SingleAgentMaxToolCallRounds int `json:"single_agent_max_tool_call_rounds"`
}

func DefaultAgentSettings() AgentSettings {
	return AgentSettings{
		PlannerMaxToolCallRounds:     DefaultPlannerMaxToolCallRounds,
		SingleAgentMaxToolCallRounds: DefaultSingleAgentMaxToolCallRounds,
	}
}

func ValidateAgentSettings(cfg AgentSettings) error {
	if cfg.PlannerMaxToolCallRounds < MinPlannerMaxToolCallRounds || cfg.PlannerMaxToolCallRounds > MaxPlannerMaxToolCallRounds {
		return fmt.Errorf("planner_max_tool_call_rounds must be between %d and %d", MinPlannerMaxToolCallRounds, MaxPlannerMaxToolCallRounds)
	}
	if cfg.SingleAgentMaxToolCallRounds < MinSingleAgentMaxToolCallRounds || cfg.SingleAgentMaxToolCallRounds > MaxSingleAgentMaxToolCallRounds {
		return fmt.Errorf("single_agent_max_tool_call_rounds must be between %d and %d", MinSingleAgentMaxToolCallRounds, MaxSingleAgentMaxToolCallRounds)
	}
	return nil
}

// Config 应用配置根结构
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Log       LogConfig       `yaml:"log"`
	LLM       LLMConfig       `yaml:"llm"`
	Stream    StreamConfig    `yaml:"stream"`
	Workspace WorkspaceConfig `yaml:"workspace"`
}

// WorkspaceConfig 工作流落地执行的工作区配置。
//
// 内置 native 文件工具（write_file / read_file / run_command 等）都落在
// 工作区内，路径被沙箱强制约束在 Root 之下，杜绝越界写系统目录。
// 每个工作流任务在 Root 下拥有一个以 task_id 命名的独立子目录。
type WorkspaceConfig struct {
	Root string `yaml:"root"` // 工作区根目录，默认 ./workspace
}

// StreamConfig 跨进程事件推送（方案 B：HTTP 内部回调）配置。
//
// scheduler 进程完成 turn / task 后，通过 HTTP POST 把事件回调到 server 的
// /internal/events，由 server 转成进程内 Hub 广播送达 SSE 订阅者。
type StreamConfig struct {
	CallbackURL   string `yaml:"callback_url"`   // scheduler → server 的回调根地址（如 http://127.0.0.1:8080），为空则关闭跨进程推送
	CallbackToken string `yaml:"callback_token"` // 内部事件鉴权 token；为空则不校验（仅限内网 / 本地开发）
}

// LLMConfig 大模型调用配置。
//
// Provider 决定走哪条接入通道：
//   - gemini（默认）：Gemini 网关，endpoint 为 :streamGenerateContent，聚合返回
//   - openai：OpenAI 兼容协议（如腾讯混元 tokenhub），支持真流式
//   - azure：Azure OpenAI 协议（如 某 Azure OpenAI 网关上的 gpt-5.6 系列），支持真流式
//
// base_url / api_key / model 三个 provider 复用同一组字段，但 base_url 的
// 层级约定各不相同：
//   - gemini 原生协议（Vertex AI 风格，endpoint 为 {base}/{model}:streamGenerateContent），如
//     https://gateway.example.com/llm-service/gcs/public/publishers/google/models
//   - openai 含 /v1，如 https://openai.example.com/v1
//   - azure  到 openai 段为止（不含 /deployments），如
//     https://<your-resource>.cognitiveservices.azure.com/openai
//     —— provider 会自动补 /deployments/<model>/chat/completions?api-version=<ver>
//
// api_version 仅 azure 使用；留空时 provider 取自身默认值。
type LLMConfig struct {
	Provider   string `yaml:"provider"`    // 接入通道: gemini / openai / azure，默认 gemini
	APIKey     string `yaml:"api_key"`     // 网关密钥（gemini/openai 走 Bearer，azure 走 api-key header）
	BaseURL    string `yaml:"base_url"`    // 网关根地址，层级约定见上
	Model      string `yaml:"model"`       // 模型名，如 gemini-3.1-pro-preview / hy3 / gpt-5.6-sol
	APIVersion string `yaml:"api_version"` // 仅 azure：api-version query 参数，如 2025-01-01-preview
}

// ModelProtocol 把旧 provider 口径映射到模型配置协议枚举（openai / gemini / gpt / claude）。
//
// config.yaml 的 llm.provider 沿用旧口径 gemini/openai/azure/claude，而 model_config 表
// 的 protocol 是 openai/gemini/gpt/claude。azure 即 Azure OpenAI（gpt-5.6 系列），
// 映射到 gpt。此方法让 server（种子默认模型）与 scheduler（兜底客户端）
// 共用同一份映射，避免两处各自维护 switch 而漂移。
func (l *LLMConfig) ModelProtocol() string {
	switch l.Provider {
	case "openai":
		return "openai"
	case "azure":
		return "gpt"
	case "claude":
		return "claude"
	default:
		return "gemini"
	}
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Port int    `yaml:"port"` // 监听端口，默认 8080
	Mode string `yaml:"mode"` // gin 运行模式: debug / release / test
}

// DatabaseConfig 数据库配置
//
// 两种驱动：
//   - mysql（默认）：走 host/port/user/password/dbname，云上库延迟高的场景可
//     在系统设置里改成指向本机 MySQL；
//   - sqlite：单机零依赖，只认 Path（本地文件）。延迟问题最彻底的解法 ——
//     查询全是本机文件 IO，没有网络往返。
//
// 结构体同时带 yaml 与 json 两种 tag：yaml 用于 conf.yaml，json 用于
// 「系统设置」的持久化文件（db_config.json），两者字段一致，避免维护两份映射。
type DatabaseConfig struct {
	Driver   string `yaml:"driver" json:"driver"`     // mysql / sqlite，默认 mysql
	Host     string `yaml:"host" json:"host"`         // MySQL 地址，默认 127.0.0.1
	Port     int    `yaml:"port" json:"port"`         // MySQL 端口，默认 3306
	User     string `yaml:"user" json:"user"`         // 用户名
	Password string `yaml:"password" json:"password"` // 密码
	DBName   string `yaml:"dbname" json:"dbname"`     // 数据库名
	Charset  string `yaml:"charset" json:"charset"`   // 字符集，默认 utf8mb4
	Path     string `yaml:"path" json:"path"`         // sqlite 数据库文件路径，默认 ./data/flowagent.db
}

// LogConfig 日志配置
type LogConfig struct {
	Level string `yaml:"level"` // 日志级别: debug / info / warn / error，默认 info
	File  string `yaml:"file"`  // 日志文件路径，为空则输出到 stdout
}

// DriverName 返回归一化后的驱动名（空串视为 mysql）。
func (d *DatabaseConfig) DriverName() string {
	switch strings.ToLower(strings.TrimSpace(d.Driver)) {
	case DriverSQLite:
		return DriverSQLite
	default:
		return DriverMySQL
	}
}

// DSN 根据 DatabaseConfig 生成 MySQL DSN
func (d *DatabaseConfig) DSN() string {
	charset := d.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	// 超时参数必须显式设置：默认 readTimeout/writeTimeout 为 0（无限等待），
	// 一旦连接半开（公网链路静默断开空闲连接），读操作会永久阻塞，调度循环卡死。
	// timeout 控制建连、readTimeout/writeTimeout 控制读写；超时后驱动返回
	// driver.ErrBadConn，database/sql 会自动丢弃坏连接并重试，调度得以恢复。
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=True&loc=Local&timeout=5s&readTimeout=30s&writeTimeout=30s",
		d.User, d.Password, d.Host, d.Port, d.DBName, charset)
}

// SQLiteDSN 生成 sqlite 的 DSN（文件路径 + 关键 pragma）。
//
// 三个 pragma 都是必须的，不是调优：
//   - busy_timeout：写锁冲突时轮询等待而不是立刻报 SQLITE_BUSY。scheduler 与
//     server 两个进程同时写一个库文件，没有它并发写必炸。
//   - journal_mode=WAL：读写不互相阻塞（WAL 下读不阻塞写、写不阻塞读）。
//   - foreign_keys：sqlite 默认不开启外键约束，打开以对齐 MySQL 行为。
func (d *DatabaseConfig) SQLiteDSN() string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		d.SQLitePath())
}

// SQLitePath 返回 sqlite 文件路径（未配置时取默认值）。
func (d *DatabaseConfig) SQLitePath() string {
	if strings.TrimSpace(d.Path) != "" {
		return strings.TrimSpace(d.Path)
	}
	return "./data/flowagent.db"
}

// Desc 给日志用的脱敏描述：区分驱动，且不打印密码。
func (d *DatabaseConfig) Desc() string {
	if d.DriverName() == DriverSQLite {
		return fmt.Sprintf("sqlite(%s)", d.SQLitePath())
	}
	return fmt.Sprintf("mysql(%s:%d/%s)", d.Host, d.Port, d.DBName)
}

// defaults 填充默认值
func applyDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.Mode == "" {
		cfg.Server.Mode = "release"
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Workspace.Root == "" {
		cfg.Workspace.Root = "./workspace"
	}
	applyDatabaseDefaults(&cfg.Database)
}

// applyDatabaseDefaults 填充数据库块的默认值。
//
// 单独抽出来是因为它要在两处调用：conf.yaml 解析后、以及被「系统设置」
// 覆盖后（覆盖文件可能只写了部分字段，缺省项仍需兜底）。
func applyDatabaseDefaults(d *DatabaseConfig) {
	if d.Host == "" {
		d.Host = "127.0.0.1"
	}
	if d.Port == 0 {
		d.Port = 3306
	}
	if d.Charset == "" {
		d.Charset = "utf8mb4"
	}
	if d.DBName == "" {
		d.DBName = "flowagent"
	}
}

// dbSettingsPath 最近一次 Load 所用的 conf.yaml 对应的设置文件路径。
//
// 之所以由 Load 记住、而不是让调用方各自拼：server / scheduler / tools 三个
// 入口都只在启动时知道自己的 config 路径，系统设置 API 要写回同一个文件，
// 必须有一个大家都认的落点。
var dbSettingsPath string

// DBSettingsPath 返回数据库系统设置的持久化路径（未 Load 过则用默认 config/db_config.json）。
func DBSettingsPath() string {
	if dbSettingsPath == "" {
		return filepath.Join("config", DBConfigFileName)
	}
	return dbSettingsPath
}

// LoadDBSettings 读取数据库系统设置。文件不存在时返回 (nil, nil) —— 这在
// 「还没在系统设置里配过」是完全正常的，调用方回落到 conf.yaml 即可。
func LoadDBSettings(path string) (*DatabaseConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read db settings %s: %w", path, err)
	}
	var cfg DatabaseConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse db settings %s: %w", path, err)
	}
	applyDatabaseDefaults(&cfg)
	return &cfg, nil
}

// SaveDBSettings 保存数据库系统设置（覆盖写，权限 0600 —— 里面有密码）。
func SaveDBSettings(path string, cfg DatabaseConfig) error {
	applyDatabaseDefaults(&cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal db settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create db settings dir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write db settings %s: %w", path, err)
	}
	return nil
}

// AgentSettingsPath 返回 Agent 运行设置的持久化路径。
func AgentSettingsPath() string {
	return filepath.Join(filepath.Dir(DBSettingsPath()), AgentSettingsFileName)
}

// LoadAgentSettings 读取 Agent 设置。文件不存在时返回默认值；已存在的旧文件
// 若缺少新增字段，也按默认值补齐，保持向后兼容。
func LoadAgentSettings(path string) (AgentSettings, error) {
	cfg := DefaultAgentSettings()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read agent settings %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultAgentSettings(), fmt.Errorf("parse agent settings %s: %w", path, err)
	}
	if err := ValidateAgentSettings(cfg); err != nil {
		return DefaultAgentSettings(), err
	}
	return cfg, nil
}

func SaveAgentSettings(path string, cfg AgentSettings) error {
	if err := ValidateAgentSettings(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create agent settings dir %s: %w", filepath.Dir(path), err)
	}
	// 先写临时文件再 rename，避免 scheduler 恰好读到半份 JSON。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("write agent settings %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace agent settings %s: %w", path, err)
	}
	return nil
}

// CurrentAgentSettings 在每个调度决策点读取最新设置。读取异常时沿用安全默认值，
// 避免设置文件的瞬时 I/O 问题中断在途任务。
func CurrentAgentSettings() AgentSettings {
	cfg, err := LoadAgentSettings(AgentSettingsPath())
	if err != nil {
		return DefaultAgentSettings()
	}
	return cfg
}

// Load 从 YAML 文件加载配置
//
// 加载顺序：conf.yaml → config/db_config.json（系统设置，若存在则整体覆盖
// database 块）。后者优先，这样前端改完库地址不用再去动 conf.yaml。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	applyDefaults(&cfg)

	settingsPath := filepath.Join(filepath.Dir(path), DBConfigFileName)
	override, err := LoadDBSettings(settingsPath)
	if err != nil {
		return nil, err
	}
	dbSettingsPath = settingsPath
	if override != nil {
		cfg.Database = *override
	}
	return &cfg, nil
}
