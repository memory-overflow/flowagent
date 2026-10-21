package handler

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
)

// ============================================================
// 系统设置 —— 数据库连接
// ============================================================
//
// 为什么存在这一组接口：云上 MySQL 每次查询都要跨公网往返，本地开发时
// 慢到影响调试。数据库参数又不能存在数据库里（要先连上库才能读设置，
// 而读设置正是为了决定连哪个库），所以落在 conf.yaml 同级的
// config/db_config.json —— 改库不用动 conf.yaml，密码也不随代码提交。
//
// 生效时机：**保存即生效**。server 保存成功后立刻就地换库并 Migrate；
// scheduler 由设置文件 watcher（cmd/scheduler/main.go）在 2s 内跟随切换。
// 切换过程对在途请求透明：旧连接池延迟 30s 关闭（dao.Reload），
// 它们在旧库上把当前事务跑完，新请求落到新库。

// activeDBConfig 当前进程实际生效的数据库配置。
//
// 由 server / scheduler 启动时注入（就是它们拿来 dao.Init 的那一份），
// 切换成功后同步更新。「系统设置」页据此展示现状 —— 否则页面上会出现
// 「文件里是 A、进程在跑 B」却看不出区别的情况。
var activeDBConfig *config.DatabaseConfig

// dbReloader server 侧的换库动作（dao.Reload + Migrate 全模型清单），
// 由 server 启动时注入 —— Migrate 需要完整模型清单，handler 不该知道。
var dbReloader func(cfg config.DatabaseConfig) error

// SetActiveDBConfig 记录当前进程生效的数据库配置。
func SetActiveDBConfig(cfg config.DatabaseConfig) {
	activeDBConfig = &cfg
}

// SetDBReloader 注入换库动作（server 启动时调用）。
// fn 返回错误时保存接口会拒绝落盘后的切换并回滚设置文件。
func SetDBReloader(fn func(cfg config.DatabaseConfig) error) {
	dbReloader = fn
}

// DBConfigItem 前端「系统设置」页展示的数据库连接配置。
type DBConfigItem struct {
	Driver       string `json:"driver"`        // mysql / sqlite
	Host         string `json:"host"`          // MySQL 地址
	Port         int    `json:"port"`          // MySQL 端口
	User         string `json:"user"`          // 用户名
	Password     string `json:"password"`      // 密码（明文返回，便于本地编辑；与模型配置页口径一致）
	DBName       string `json:"dbname"`        // 数据库名
	Charset      string `json:"charset"`       // 字符集
	Path         string `json:"path"`          // sqlite 文件路径
	SettingsPath string `json:"settings_path"` // 设置文件路径（提示用户「密码不在 conf.yaml 里」）
	FromSettings bool   `json:"from_settings"` // 当前生效配置是否来自设置文件（false = 来自 conf.yaml 或改完未重启）
	ActiveDesc   string `json:"active_desc"`   // 当前进程实际生效的库（脱敏）
}

// UpdateDBConfigRequest 保存数据库连接配置。
type UpdateDBConfigRequest struct {
	Driver   string `json:"driver"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
	Charset  string `json:"charset"`
	Path     string `json:"path"`
}

// UpdateDBConfigResponse 保存结果。
//
// RequireRestart 恒为 false（热切换）：保留字段只为前端兼容旧响应结构。
type UpdateDBConfigResponse struct {
	OK             bool          `json:"ok"`
	RequireRestart bool          `json:"require_restart"`
	Msg            string        `json:"msg"`
	Item           *DBConfigItem `json:"item,omitempty"`
}

// TestDBConfigRequest 连通性测试：用请求里的参数试连（不落盘、不影响当前进程）。
//
// 字段与保存完全相同 —— 测的就是「即将保存的这一套参数」，两边共用一份
// 定义，避免测试通过却因字段映射不同而保存出一个连不上的库。
type TestDBConfigRequest = UpdateDBConfigRequest

// TestDBConfigResponse 连通性测试结果。
type TestDBConfigResponse struct {
	Success  bool   `json:"success"`
	ErrorMsg string `json:"error_msg,omitempty"`
	Desc     string `json:"desc,omitempty"` // 被测目标的脱敏描述，回显给用户确认测的是哪个库
}

// GetDBConfig 返回当前数据库配置（优先系统设置，否则 conf.yaml 里的值）。
func GetDBConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	return buildDBConfigItem(loadDBConfigForEdit()), nil
}

func GetDBConfigBinder() interface{} { return &struct{}{} }

// UpdateDBConfig 保存数据库配置到设置文件，并就地热切换到新库。
func UpdateDBConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*UpdateDBConfigRequest)
	cfg := toDatabaseConfig(r)

	// 与当前生效配置一致就不用折腾（重复点保存）。
	if activeDBConfig != nil && cfg.Desc() == activeDBConfig.Desc() {
		return &UpdateDBConfigResponse{
			OK:   true,
			Msg:  "配置未变化，当前已在用 " + cfg.Desc(),
			Item: buildDBConfigItem(&cfg),
		}, nil
	}

	// 保存前先试连：与其让用户切过去才发现连不上，不如在这里拦下。
	if err := dao.Ping(cfg); err != nil {
		return nil, &APIError{
			Code: http.StatusBadRequest,
			Msg:  "连接失败，未保存：" + err.Error(),
		}
	}

	path := config.DBSettingsPath()
	if err := config.SaveDBSettings(path, cfg); err != nil {
		return nil, err
	}

	// 就地切换：换连接池 + 对新库 Migrate。失败则回滚设置文件，
	// 保持「文件 = 进程在用的库」这一不变量，页面状态才不会骗人。
	if dbReloader != nil {
		if err := dbReloader(cfg); err != nil {
			_ = os.Remove(path)
			return nil, &APIError{
				Code: http.StatusInternalServerError,
				Msg:  "已试连成功但切换失败（设置文件已回滚）：" + err.Error(),
			}
		}
		SetActiveDBConfig(cfg)
	}

	msg := "已保存到 " + path + " 并立即生效"
	if dbReloader == nil {
		// 防御：进程没注入 reloader（理论不可达），退化为重启生效
		msg += "（本进程未启用热切换，重启后生效）"
	}
	return &UpdateDBConfigResponse{
		OK:   true,
		Msg:  msg,
		Item: buildDBConfigItem(&cfg),
	}, nil
}

func UpdateDBConfigBinder() interface{} { return &UpdateDBConfigRequest{} }

// TestDBConfig 用请求里的参数试连数据库（不落盘）。
func TestDBConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*TestDBConfigRequest)
	cfg := toDatabaseConfig(r)
	if err := dao.Ping(cfg); err != nil {
		return &TestDBConfigResponse{Success: false, ErrorMsg: err.Error(), Desc: cfg.Desc()}, nil
	}
	return &TestDBConfigResponse{Success: true, Desc: cfg.Desc()}, nil
}

func TestDBConfigBinder() interface{} { return &TestDBConfigRequest{} }

// ============================================================
// 辅助函数
// ============================================================

// loadDBConfigForEdit 取「待编辑」的配置：设置文件优先，否则用进程当前
// 生效的配置（conf.yaml 来的），让用户看到的就是即将被改的那一份。
func loadDBConfigForEdit() *config.DatabaseConfig {
	path := config.DBSettingsPath()
	saved, err := config.LoadDBSettings(path)
	if err == nil && saved != nil {
		return saved
	}
	if activeDBConfig != nil {
		cp := *activeDBConfig
		return &cp
	}
	return &config.DatabaseConfig{}
}

func toDatabaseConfig(r *UpdateDBConfigRequest) config.DatabaseConfig {
	return config.DatabaseConfig{
		Driver:   strings.ToLower(strings.TrimSpace(r.Driver)),
		Host:     strings.TrimSpace(r.Host),
		Port:     r.Port,
		User:     strings.TrimSpace(r.User),
		Password: r.Password,
		DBName:   strings.TrimSpace(r.DBName),
		Charset:  strings.TrimSpace(r.Charset),
		Path:     strings.TrimSpace(r.Path),
	}
}

func buildDBConfigItem(cfg *config.DatabaseConfig) *DBConfigItem {
	if cfg == nil {
		cfg = &config.DatabaseConfig{}
	}
	item := &DBConfigItem{
		Driver:       cfg.DriverName(),
		Host:         cfg.Host,
		Port:         cfg.Port,
		User:         cfg.User,
		Password:     cfg.Password,
		DBName:       cfg.DBName,
		Charset:      cfg.Charset,
		Path:         cfg.SQLitePath(),
		SettingsPath: config.DBSettingsPath(),
	}
	if activeDBConfig != nil {
		item.ActiveDesc = activeDBConfig.Desc()
		// 设置文件存在且与进程在用的库一致，才说明「生效来源是设置文件」；
		// 两者不一致就是改完还没重启。
		saved, err := config.LoadDBSettings(config.DBSettingsPath())
		item.FromSettings = err == nil && saved != nil && saved.Desc() == activeDBConfig.Desc()
	}
	return item
}

// ============================================================
// 系统设置 —— Agent 工具调用轮次
// ============================================================

func GetAgentSettings(ctx *gin.Context, req interface{}) (interface{}, error) {
	cfg, err := config.LoadAgentSettings(config.AgentSettingsPath())
	if err != nil {
		return nil, &APIError{Code: http.StatusInternalServerError, Msg: "读取 Agent 设置失败: " + err.Error()}
	}
	return cfg, nil
}

func GetAgentSettingsBinder() interface{} { return &struct{}{} }

func UpdateAgentSettings(ctx *gin.Context, req interface{}) (interface{}, error) {
	body, ok := req.(*config.AgentSettings)
	if !ok || body == nil {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "请求参数格式错误"}
	}
	if err := config.ValidateAgentSettings(*body); err != nil {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: err.Error()}
	}
	if err := config.SaveAgentSettings(config.AgentSettingsPath(), *body); err != nil {
		return nil, &APIError{Code: http.StatusInternalServerError, Msg: "保存 Agent 设置失败: " + err.Error()}
	}
	return *body, nil
}

func UpdateAgentSettingsBinder() interface{} { return &config.AgentSettings{} }
