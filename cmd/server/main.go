package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof" // 注册 /debug/pprof/ 路由到 DefaultServeMux
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/handler"
	"github.com/memory-overflow/flowagent/model"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to YAML config file")
	pprofAddr := flag.String("pprof", "127.0.0.1:6060", "pprof HTTP listen address (empty to disable)")
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	// 配置日志输出：若指定了文件则同时写文件+控制台
	if cfg.Log.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Log.File), 0755); err != nil {
			log.Fatalf("Failed to create log dir: %v", err)
		}
		f, err := os.OpenFile(cfg.Log.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file: %v", err)
		}
		multiWriter := io.MultiWriter(os.Stdout, f)
		log.SetOutput(multiWriter)
		gin.DefaultWriter = multiWriter
		gin.DefaultErrorWriter = multiWriter
		log.Printf("Log file: %s (stdout + file)", cfg.Log.File)
	}

	log.Printf("Config loaded: %s (port=%d, mode=%s)", *configPath, cfg.Server.Port, cfg.Server.Mode)

	// 启动 pprof 调试端点（默认 loopback 端口，供排查 server 卡住时抓 goroutine/阻塞点）。
	if *pprofAddr != "" {
		startPprof(*pprofAddr)
	}

	// 设置内部事件鉴权 token（scheduler → server 回调 /internal/events 时校验）。
	// 为空表示不校验，仅限内网 / 本地开发环境。
	handler.SetInternalToken(cfg.Stream.CallbackToken)

	// 注入工作区根目录：文件上传/下载端点据此定位 workspace/<chat_id>/，
	// 并对所有路径做沙箱校验。与 scheduler 侧用的是同一个 root。
	handler.SetWorkspaceRoot(cfg.Workspace.Root)

	// 初始化数据库（驱动与连接参数来自 conf.yaml 或 config/db_config.json 系统设置）
	if err := dao.Init(cfg.Database); err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}
	// 记下进程实际生效的库：前端「系统设置」页要展示现状，让用户看得出
	// 「设置文件已改、但进程还在用旧库」这种差一重启的状态。
	handler.SetActiveDBConfig(cfg.Database)
	log.Printf("Database connected: %s (settings file: %s)", cfg.Database.Desc(), config.DBSettingsPath())

	// 自动迁移表结构
	if err := migrateAll(); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}
	log.Println("Database migration completed")

	// 注入热切换动作：「系统设置」保存后不重启直接换库 ——
	// dao.Reload 换连接池，migrateAll 保证新库表结构就绪，最后同步 activeDBConfig。
	// 模型清单收在 migrateAll 里，handler 不需要知道有哪些表。
	handler.SetDBReloader(func(dbCfg config.DatabaseConfig) error {
		if err := dao.Reload(dbCfg); err != nil {
			return err
		}
		return migrateAll()
	})

	// 模型配置种子：首次启动（model_config 表为空）时，把 config.yaml 的 llm 块
	// 落成一条默认模型，让「前端模型页」开箱即有可用模型；后续增删改由前端驱动。
	if err := seedDefaultModel(cfg); err != nil {
		log.Printf("Seed default model failed (non-fatal): %v", err)
	}

	// 设置 Gin 运行模式
	gin.SetMode(cfg.Server.Mode)

	// 创建路由（带日志中间件）
	router := gin.New()
	router.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("[GIN] %s | %3d | %13v | %-7s %s\n",
			param.TimeStamp.Format("2006-01-02 15:04:05"),
			param.StatusCode,
			param.Latency,
			param.Method,
			param.Path,
		)
	}))
	router.Use(gin.Recovery())

	// 注册业务路由
	handler.RegisterRoutes(&router.RouterGroup)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("Workflow Agent API server starting on %s", addr)
	if err := router.Run(addr); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// migrateAll 对当前生效的数据库做全量 AutoMigrate（启动与热切换共用）。
func migrateAll() error {
	return dao.Migrate(
		&model.Conversation{},
		&model.Task{},
		&model.TaskTemp{},
		&model.TaskTurn{},
		&model.MCPServer{},
		&model.ConversationMCP{},
		&model.Skill{},
		&model.ConversationSkill{},
		&model.ModelConfig{},
		&model.SearchConfig{},
		&model.ContextSummary{},
		&model.TaskContextSummary{},
	)
}

// startPprof 启动独立的 pprof HTTP 端点。
//
// 使用 net/http/pprof 提供的默认路由（/debug/pprof/），并开启 block/mutex profiling
// 用于定位「卡在 chan/锁/IO 等待」的 goroutine。二者仅在开启 pprof 时启用，开销可忽略；
// 默认只绑定 127.0.0.1 避免对外暴露内存/堆栈。
func startPprof(addr string) {
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
	go func() {
		log.Printf("pprof enabled: http://%s/debug/pprof/", addr)
		if err := http.ListenAndServe(addr, nil); err != nil && err != http.ErrServerClosed {
			log.Printf("pprof server error: %v", err)
		}
	}()
}

// seedDefaultModel 首次启动时把 config.yaml 的 llm 块落成一条默认模型配置。
//
// 只在「config.yaml 配置了 llm 块（base_url + model 非空）且 model_config 表
// 为空」时执行：llm 块缺失时 seed 出来的是一条协议回落 gemini、URL/Key/模型
// 全空的空壳，既不能调用又误导用户，不如不 seed，让新用户从首次配置引导
// 干净开始。默认模型 is_default=true、enabled=true，供 scheduler 的 Resolver
// 在「对话未显式指定模型」时回落使用。
func seedDefaultModel(cfg *config.Config) error {
	if strings.TrimSpace(cfg.LLM.BaseURL) == "" || strings.TrimSpace(cfg.LLM.Model) == "" {
		return nil
	}

	modelDAO := &dao.ModelConfigDAO{}
	n, err := modelDAO.Count(context.Background())
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	now := time.Now()
	m := &model.ModelConfig{
		ID:         uuid.New().String(),
		Name:       "默认模型",
		Protocol:   model.ModelProtocol(cfg.LLM.ModelProtocol()),
		BaseURL:    cfg.LLM.BaseURL,
		APIKey:     cfg.LLM.APIKey,
		Model:      cfg.LLM.Model,
		APIVersion: cfg.LLM.APIVersion,
		IsDefault:  true,
		Enabled:    true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := modelDAO.CreateModel(context.Background(), m); err != nil {
		return err
	}
	log.Printf("Seeded default model config: id=%s name=%q protocol=%s model=%s",
		m.ID, m.Name, m.Protocol, m.Model)
	return nil
}
