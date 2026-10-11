// Package main — agent-supervisor：后端进程守护 + 前端逃生入口。
//
// 职责：
//  1. 托管 agent-server 与 agent-scheduler：子进程异常退出自动拉起（带重启
//     间隔，避免 crash 风暴）。
//  2. 提供独立于 server 的管理端点（默认 127.0.0.1:9090）：
//     POST /restart —— 前端「逃生入口」：server 卡死/退出时，前端直连本端点
//     强制重启两个子进程，不经过卡死的 server（:8080）；
//     GET  /health  —— supervisor 自身存活探测（前端用于等待服务恢复）。
//
// 为什么逃生端点必须独立端口：server 卡死时 :8080 上的任何接口都不可达，
// 若逃生入口也挂在 server 上就等于没有逃生。supervisor 是极简进程（纯
// fork/kill + 一个小 HTTP handler），自身几乎不会卡。
//
// 用法：
//
//	go build -o bin/agent-supervisor ./cmd/supervisor
//	./bin/agent-supervisor -config config/config.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// restartDelay 子进程退出后的重启间隔：太短会 crash 风暴刷屏，太长逃生体感差。
const restartDelay = 2 * time.Second

// ============================================================
// managed — 被托管的子进程
// ============================================================

// managed 一个被 supervisor 托管的子进程。
//
// 并发模型：spawn/kill 由 supervise 循环与 HTTP handler 并发调用，
// 全部经 mu 保护；done channel 在子进程退出时关闭，supervise 据此感知退出。
type managed struct {
	name string   // 展示名，如 agent-server
	bin  string   // 可执行文件路径
	args []string // 启动参数

	mu   sync.Mutex
	cmd  *exec.Cmd     // 当前子进程；nil 表示未运行
	done chan struct{} // cmd 退出时关闭
}

// spawn 启动子进程（非阻塞）。stdout/stderr 直通 supervisor 终端，
// 便于在同一个窗口里看到两个子进程的日志。
func (m *managed) spawn() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		return fmt.Errorf("%s 已在运行", m.name)
	}
	cmd := exec.Command(m.bin, m.args...)
	// 子进程与 supervisor 同 cwd（项目根）：skill 的相对路径
	// skills/... 依赖这一点，与 start.sh 直接启动的行为一致。
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	m.cmd = cmd
	m.done = make(chan struct{})
	log.Printf("[supervisor] %s 已启动 (pid %d)", m.name, cmd.Process.Pid)

	// 独立 goroutine 等待退出并关闭 done；只有 cmd 仍是当前实例时才收尾，
	// 避免 stop() 已把 cmd 置 nil 后旧 Wait 回调误关新实例的 done。
	go func(c *exec.Cmd, d chan struct{}) {
		_ = c.Wait()
		m.mu.Lock()
		if m.cmd == c {
			m.cmd = nil
			close(d)
		}
		m.mu.Unlock()
	}(cmd, m.done)
	return nil
}

// kill 强杀当前子进程（若有）。退出后 supervise 循环会按间隔自动拉起。
func (m *managed) kill() {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		log.Printf("[supervisor] 正在停止 %s (pid %d) ...", m.name, cmd.Process.Pid)
		_ = cmd.Process.Kill()
	}
}

// running 当前子进程是否存活。
func (m *managed) running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil
}

// supervise 托管循环：启动 → 等退出 → 延迟 → 再启动，直到 ctx 取消。
func (m *managed) supervise(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := m.spawn(); err != nil {
			log.Printf("[supervisor] 启动 %s 失败: %v（%v 后重试）", m.name, err, restartDelay)
		} else {
			m.mu.Lock()
			done := m.done
			m.mu.Unlock()
			select {
			case <-done:
				log.Printf("[supervisor] %s 已退出，%v 后自动重启", m.name, restartDelay)
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartDelay):
		}
	}
}

// ============================================================
// 管理端点
// ============================================================

// corsMiddleware 为管理端点加 CORS 头。
//
// 前端跑在独立端口（vite dev 默认 3000），与 supervisor（:9090）跨源，
// 浏览器会拦截无 CORS 头的响应，前端 fetch 表现为 "Failed to fetch"。
// 管理端点只提供本机重启/健康探测能力，Allow-Origin 用 * 即可。
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerHandlers 注册管理端点。
//
// POST /restart：前端逃生入口。kill 两个子进程，supervise 循环随后自动
// 重启（延迟 restartDelay）。用 202 表示「已受理、重启进行中」，前端据此
// 轮询 /health 与 server 的可用性。
func registerHandlers(mux *http.ServeMux, procs []*managed) {
	mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		for _, p := range procs {
			p.kill()
		}
		log.Printf("[supervisor] 收到重启请求（来自 %s），已触发全部子进程重启", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(w, `{"code":0,"msg":"restart triggered"}`)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		status := make(map[string]bool, len(procs))
		allUp := true
		for _, p := range procs {
			up := p.running()
			status[p.name] = up
			if !up {
				allUp = false
			}
		}
		if !allUp {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintf(w, `{"code":0,"data":{"all_up":%t}}`, allUp)
	})
}

// ============================================================
// main
// ============================================================

func main() {
	configPath := flag.String("config", "config/config.yaml", "传给子进程的 YAML 配置文件路径")
	addr := flag.String("addr", "127.0.0.1:9090", "管理端点监听地址（默认仅本机可达）")
	binDir := flag.String("bin", "bin", "子进程二进制所在目录")
	flag.Parse()

	procs := []*managed{
		{name: "agent-server", bin: filepath.Join(*binDir, "agent-server"), args: []string{"-config", *configPath}},
		{name: "agent-scheduler", bin: filepath.Join(*binDir, "agent-scheduler"), args: []string{"-config", *configPath}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 每个子进程一个托管循环：启动 → 等退出 → 自动重启。
	for _, p := range procs {
		go p.supervise(ctx)
	}

	// 管理端点：只绑 loopback（默认），逃生入口不对外网暴露。
	mux := http.NewServeMux()
	registerHandlers(mux, procs)
	srv := &http.Server{Addr: *addr, Handler: corsMiddleware(mux)}
	go func() {
		log.Printf("[supervisor] 管理端点已就绪: http://%s（POST /restart = 逃生重启）", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[supervisor] 管理端点启动失败: %v", err)
		}
	}()

	// 优雅退出：supervisor 收到信号 → 停托管循环 → kill 子进程 → 结束。
	// 不做复杂的子进程优雅停机：逃生场景的语义就是「强制拉回可用状态」。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("[supervisor] 收到信号 %s，停止全部子进程 ...", sig)
	cancel()
	for _, p := range procs {
		p.kill()
	}
	time.Sleep(500 * time.Millisecond) // 给 Wait 回调留出收尾时间
	log.Println("[supervisor] 已退出")
}
