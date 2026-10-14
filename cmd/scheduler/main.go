// Package main — Stage 调度器独立进程入口
//
// 与 API server（cmd/server）解耦：
//   - server 只负责 HTTP 接口（增删改查、建 Task 记录）
//   - scheduler 独立进程负责扫描 task 表，驱动各 stage 执行
//
// 好处：
//   - 二者可独立扩缩容（scheduler 是 CPU/LLM 密集，server 是 IO 密集）
//   - 单方发布不影响另一方
//   - 调度器崩溃不会拖垮 API
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
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
	"github.com/memory-overflow/flowagent/pkg/stream"
	"github.com/memory-overflow/flowagent/schedule"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to YAML config file")
	pprofAddr := flag.String("pprof", "127.0.0.1:6061", "pprof HTTP listen address (empty to disable)")
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 配置日志输出：若指定了文件则同时写文件+控制台
	// scheduler 使用独立日志文件，避免与 server 相互覆盖
	if cfg.Log.File != "" {
		logPath := schedulerLogPath(cfg.Log.File)
		if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
			log.Fatalf("Failed to create log dir: %v", err)
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file: %v", err)
		}
		log.SetOutput(io.MultiWriter(os.Stdout, f))
		log.Printf("Log file: %s (stdout + file)", logPath)
	}

	log.Printf("Config loaded: %s", *configPath)

	// 启动 pprof 调试端点（默认 loopback 端口，供排查调度卡住时抓 goroutine/阻塞点）。
	// 仅在显式开启时启用 block/mutex profiling，关闭时零开销。
	if *pprofAddr != "" {
		startPprof(*pprofAddr)
	}

	// 注册跨进程事件转发器（方案 B：HTTP 内部回调）。
	// scheduler 侧 actuator / container 通过 stream.Publish 发布事件，
	// 先本地 Hub 广播，再由 HTTPForwarder 异步回调到 server 的 /internal/events，
	// 从而送达 server 进程里的 SSE 订阅者。callback_url 为空则关闭跨进程推送。
	if cfg.Stream.CallbackURL != "" {
		fw := stream.NewHTTPForwarder(cfg.Stream.CallbackURL, cfg.Stream.CallbackToken)
		stream.SetForwarder(fw)
		defer fw.Close()
		log.Printf("Stream forwarder enabled: %s", cfg.Stream.CallbackURL)
	} else {
		log.Println("Stream forwarder disabled (stream.callback_url is empty)")
	}

	// 初始化数据库（scheduler 只读写 task/task_temp；建表交给 server，这里做防御性迁移）
	//
	// 驱动与连接参数读的是同一份配置：conf.yaml 的 database 块，或
	// config/db_config.json 的系统设置覆盖。
	if err := dao.Init(cfg.Database); err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}
	log.Printf("Database connected: %s (settings file: %s)", cfg.Database.Desc(), config.DBSettingsPath())

	// 确保调度器启动前所需表已存在
	// （防御性 migrate，与 server 幂等；scheduler 需读 skill 表）
	if err := migrateSchedulerModels(); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	// 设置文件 watcher：server 侧「系统设置」保存后不重启直接生效 ——
	// server 进程自己就地换库，scheduler 是独立进程，靠轮询设置文件跟随。
	// 每 2s 读一次小 JSON 并比对目标描述，未变化时开销可忽略；
	// 变化后 dao.Reload + 防御性 migrate（新库建表通常已由 server 完成，
	// 这里幂等兜底，避免 server 还没迁完时调度查询打在空表上）。
	go watchDBSettings(cfg.Database)

	// 启动自愈：把上次进程遗留的孤儿任务（祖先已终态、自己还在 running）
	// 先收口成 stopped，再交给调度器 —— 否则调度器只看 status 就会把它们
	// 重新捞起来跑（典型症状：停掉的工作流，一开调度器又继续烧 LLM）。
	// 失败不致命：退化成「照旧启动」，最多是这批任务再被捞一次。
	reconcileCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if ids, err := (&dao.TaskDAO{}).ReconcileOrphanTasks(reconcileCtx); err != nil {
		log.Printf("Reconcile orphan tasks failed (ignored): %v", err)
	} else if len(ids) > 0 {
		log.Printf("Reconciled %d orphan task(s) -> stopped: %v", len(ids), ids)
	}
	cancel()

	// 启动 stage 调度器（functional_call / tool_call）。
	// 规划不再是独立 stage，而是 functional_call 内的一个 phase。
	stageScheduler, err := schedule.NewStageScheduler(cfg)
	if err != nil {
		log.Fatalf("Failed to start stage scheduler: %v", err)
	}

	// 预热内置 MCP：后台把 npx 包下载 / Node 冷启动 / Playwright 内核加载等
	// 昂贵开销摊到启动期，避免用户首个请求吃到几十秒冷启动。失败只记日志。
	go mcppkg.PrewarmBuiltins(mcpBuiltinsDir())

	// 预热 token 计数器：首次要下载 o200k_base BPE 词典（约 3.4MB）并解析
	// 几十万条规则，慢网下实测下载可占近一分钟。与上面一样摊到启动期后台执行，
	// 否则这段时间会落在用户首个任务的上下文组装上（表现为首轮几十秒无响应）。
	// 预热未完成时计数自动走启发式估算，不阻塞、不影响链路。
	go contextmgr.WarmCounter()

	log.Println("Stage scheduler running. Press Ctrl+C to stop.")

	// 阻塞等待 SIGINT / SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("Received signal %s, shutting down...", sig)

	stageScheduler.Close()

	// 关闭 MCP 连接池：池内连接持有 npx / python 子进程（以及 playwright 拉起的
	// 浏览器）。Windows 上父进程退出不会自动带走子进程，不显式关会留下孤儿进程。
	mcppkg.CloseAll()

	log.Println("Scheduler shut down gracefully")
}

// startPprof 启动独立的 pprof HTTP 端点。
//
// 使用 net/http/pprof 提供的默认路由（/debug/pprof/），并开启：
//   - runtime.SetBlockProfileRate(1)：统计 goroutine 阻塞时长，定位「卡在 chan/锁/IO 等待」
//   - runtime.SetMutexProfileFraction(1)：统计互斥锁争用，定位锁竞争
//
// 二者仅在开启 pprof 时启用，开销可忽略；默认只绑定 127.0.0.1 避免对外暴露内存/堆栈。
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

// mcpBuiltinsDir 返回内置 MCP 清单目录，与 handler 层保持同一约定：
// 优先读 env MCP_BUILTINS_DIR，否则回落到相对工作目录的 ./mcp/builtins。
func mcpBuiltinsDir() string {
	if v := os.Getenv("MCP_BUILTINS_DIR"); v != "" {
		return v
	}
	return "./mcp/builtins"
}

// schedulerLogPath 基于 server 日志路径衍生 scheduler 的日志路径：
//
//	logs/flowagent.log -> logs/flowagent.scheduler.log
func schedulerLogPath(serverLogFile string) string {
	dir := filepath.Dir(serverLogFile)
	base := filepath.Base(serverLogFile)
	ext := filepath.Ext(base)
	name := base[:len(base)-len(ext)]
	return filepath.Join(dir, fmt.Sprintf("%s.scheduler%s", name, ext))
}

// migrateSchedulerModels scheduler 运行所需的全部表（启动与热切换共用）。
func migrateSchedulerModels() error {
	return dao.Migrate(
		&model.Task{},
		&model.TaskTemp{},
		&model.TaskTurn{},
		&model.Skill{},
		&model.ConversationSkill{},
		// scheduler 的 Resolver 要读 model_config 表解析对话模型，需保证表存在
		&model.ModelConfig{},
		&model.Conversation{},
		// scheduler 是 web_search 的实际执行方，运行时读 search_config 表选引擎
		&model.SearchConfig{},
		// 上下文压缩在 scheduler 侧执行（它才持有 LLM 客户端与历史重建逻辑），
		// 摘要的读写都发生在这里，须保证表存在
		&model.ContextSummary{},
		// 工作流节点子任务的压缩水位按 task 存，同样只在 scheduler 侧读写
		&model.TaskContextSummary{},
	)
}

// watchDBSettings 监听「系统设置」文件，目标库变化时就地换库。
//
// 不用 fsnotify：省一个依赖，且 2s 轮询一个几百字节的 JSON 开销可忽略。
// 比较口径是 Desc()（含驱动/地址/库名/路径），密码改了但目标相同不触发切换
// —— MySQL 密码在连接池层面已经建立，换池反而会打断在途请求。
func watchDBSettings(initial config.DatabaseConfig) {
	current := initial.Desc()
	for {
		time.Sleep(2 * time.Second)

		saved, err := config.LoadDBSettings(config.DBSettingsPath())
		if err != nil || saved == nil {
			continue // 文件不存在 / 暂时读不了：维持现状
		}
		desc := saved.Desc()
		if desc == current {
			continue
		}

		log.Printf("[db-watch] settings changed: %s -> %s, switching database", current, desc)
		if err := dao.Reload(*saved); err != nil {
			// 换库失败（如新库连不上）：保持旧连接继续跑，下轮再试。
			// server 保存前已做过 Ping，走到这里多半是网络刚抖动。
			log.Printf("[db-watch] switch to %s failed (keep old): %v", desc, err)
			continue
		}
		if err := migrateSchedulerModels(); err != nil {
			// 表没建好是严重问题（调度查询会报错），但回退旧库同样危险
			// （在途请求已经可能写到新库）。记错误日志，下轮 migrate 兜底。
			log.Printf("[db-watch] migrate after switch failed: %v", err)
		}
		current = desc
	}
}
