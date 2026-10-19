// Package mcp — stdio 连接的会话级复用（连接池）
//
// 为什么需要它：
// mcp.go 原本对每个 server 都「用完即走」——每次 CallTool 都新起一个子进程、
// 握手、执行、立刻关闭。对 fetch / git 这类无状态 server 只是多付一次握手；
// 但对 **有进程内状态的 server 是致命的**：最典型的是 playwright ——
// 浏览器是 MCP server 的子进程，寿命绑在 server 进程上。于是每次调用都变成
// 「新 server → 新浏览器 → 调用返回 → 立刻 Close → server 被杀 → 浏览器退出」，
// 表现为「浏览器打开就退出」，而且跨调用的页面/标签状态全部丢失，
// 多步浏览器自动化（navigate → click → snapshot）根本无法成立。
//
// 这里给 stdio 连接加一层池：同一 server 的连接在空闲 TTL 内复用，
// 使 server 进程（及其内部的浏览器等状态）跨调用存活。
//
// 边界与取舍：
//   - 只池化 stdio。sse / streamable_http 指向的是**远端常驻服务**，本地建连
//     很轻、池化收益小，而重连语义（远端重启、session id 失效）复杂得多，
//     保持「用完即走」更稳妥。
//   - ListTools 仍走短连接。预热只需要把 npx 包下载 / 内核加载这类一次性
//     开销付掉，没必要在启动期就长期占住 N 个子进程。
//   - 同一 server 的调用串行化。playwright 这类 server 的内部状态经不起
//     并发操作（两次导航互相打架），串行是最小惊讶的选择；对无状态 server
//     只是轻微排队，等待期间尊重 ctx，不会把调用方卡死。
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	// poolReapInterval 空闲回收的扫描间隔
	poolReapInterval = 30 * time.Second

	// defaultPoolIdleTTL 连接默认空闲存活时长。
	// 15 分钟够用户在一次会话里连续操作同一个浏览器，又不会让已经没人用的
	// npx / python 子进程在后台无限期挂着。
	defaultPoolIdleTTL = 15 * time.Minute

	// poolIdleTTLEnv 覆盖空闲存活时长的环境变量（整数秒）。
	// 设为 0 表示不回收 —— 连接常驻到进程退出（浏览器会一直开着）。
	poolIdleTTLEnv = "MCP_POOL_IDLE_TTL"
)

// errPoolClosed 池内连接已被关闭、请求确定没有送达。
//
// 单独定义是为了让上层能区分「可以安全重试」的失败：只有这种错误
// （以及 transport.ErrTransportClosed）才允许换一条新连接重试一次，
// 否则可能把工具的副作用执行两次（例如把同一个按钮点两下）。
var errPoolClosed = errors.New("mcp: pooled connection closed")

// isReusableConnErr 判断错误是否属于「连接失效且请求未送达」，可以安全重试。
func isReusableConnErr(err error) bool {
	return errors.Is(err, transport.ErrTransportClosed) || errors.Is(err, errPoolClosed)
}

// pooledConn 池内的一条 stdio 连接。
type pooledConn struct {
	cfg *ServerConfig
	cli *client.Client

	// sem 容量为 1，把同一 server 的调用串行化（见包注释）。
	sem chan struct{}

	lastUsed atomic.Int64 // UnixNano
	dead     atomic.Bool
}

func (c *pooledConn) touch() { c.lastUsed.Store(time.Now().UnixNano()) }

func (c *pooledConn) idleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, c.lastUsed.Load()))
}

func (c *pooledConn) isDead() bool { return c.dead.Load() }

// call 在池化连接上执行一次工具调用。
func (c *pooledConn) call(ctx context.Context, toolName string, args map[string]interface{}) (*ToolResult, error) {
	// 等待本 server 的使用权；等待期间若调用方已超时/取消就立刻返回，
	// 避免请求在队列里堆积。
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	if c.isDead() {
		return nil, fmt.Errorf("mcp: conn for %s unavailable: %w", c.cfg.ServerName, errPoolClosed)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args

	resp, err := c.cli.CallTool(ctx, req)
	if err != nil {
		return nil, err
	}
	c.touch()

	content, images := flattenContent(resp.Content)
	return &ToolResult{
		Content: content,
		Images:  images,
		IsError: resp.IsError,
		Raw:     resp,
	}, nil
}

// close 关闭连接并标记为不可用。
//
// 先拿到 sem 再关，保证不会在请求执行中途把连接掀掉。
func (c *pooledConn) close() {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()
	if c.dead.Swap(true) {
		return
	}
	if c.cli != nil {
		_ = c.cli.Close()
	}
}

// connPool 按 server 维度的 stdio 连接池。
type connPool struct {
	mu      sync.Mutex
	entries map[string]*pooledConn
	started sync.Once
}

var globalPool = &connPool{entries: make(map[string]*pooledConn)}

// poolKey 池内唯一键：优先用 DB 主键；内置 MCP（ServerID 为空）用 "builtin:"+名字。
func poolKey(cfg *ServerConfig) string {
	if cfg.ServerID != "" {
		return cfg.ServerID
	}
	return "builtin:" + cfg.ServerName
}

// poolable 该连接是否走池化（见包注释：只池化 stdio）。
func poolable(cfg *ServerConfig) bool {
	return strings.EqualFold(cfg.Transport, "stdio")
}

// acquire 取一条可用连接；没有或不健康时新建。
func (p *connPool) acquire(ctx context.Context, cfg *ServerConfig) (*pooledConn, error) {
	p.started.Do(func() { go p.reapLoop() })

	key := poolKey(cfg)
	p.mu.Lock()
	if c, ok := p.entries[key]; ok && !c.isDead() {
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	// 建连（可能包含 npx 拉包 / 进程启动）放在锁外：不能因为某一个 server
	// 冷启动就把其它 server 的取用一起堵住。
	cli, _, err := connect(ctx, cfg)
	if err != nil {
		return nil, err
	}

	c := &pooledConn{cfg: cfg, cli: cli, sem: make(chan struct{}, 1)}
	c.touch()

	p.mu.Lock()
	if old, ok := p.entries[key]; ok && !old.isDead() {
		// 并发建连竞争：保留先到的，丢掉自己这条，避免多出一个孤儿进程。
		p.mu.Unlock()
		_ = cli.Close()
		return old, nil
	}
	p.entries[key] = c
	p.mu.Unlock()
	return c, nil
}

// discard 把某条连接移出池并关闭（连接失效后重建前调用）。
func (p *connPool) discard(c *pooledConn) {
	key := poolKey(c.cfg)
	p.mu.Lock()
	if cur, ok := p.entries[key]; ok && cur == c {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	c.close()
}

// reapLoop 周期性回收空闲超时或已失效的连接。
func (p *connPool) reapLoop() {
	t := time.NewTicker(poolReapInterval)
	defer t.Stop()
	for range t.C {
		for _, c := range p.reapOnce(time.Now()) {
			reason := "dead"
			if !c.isDead() {
				reason = "idle " + c.idleFor(time.Now()).Truncate(time.Second).String()
			}
			log.Printf("[mcp-pool] closing %s (%s)", c.cfg.ServerName, reason)
			c.close()
		}
	}
}

// reapOnce 做一轮回收判定：把「已失效」或「空闲超过 TTL」的连接移出池并返回。
//
// 从 reapLoop 里抽出来是为了可单测 —— 回收判定写错的代价不只是多关一条连接，
// 而是可能误关心跳中的连接。调用方负责真正 close 返回的条目。
func (p *connPool) reapOnce(now time.Time) []*pooledConn {
	ttl := poolIdleTTL()
	if ttl <= 0 {
		return nil
	}

	var victims []*pooledConn
	p.mu.Lock()
	for k, c := range p.entries {
		if c.isDead() || c.idleFor(now) >= ttl {
			delete(p.entries, k)
			victims = append(victims, c)
		}
	}
	p.mu.Unlock()
	return victims
}

// poolIdleTTL 解析空闲存活时长。
//
// MCP_POOL_IDLE_TTL 用整数秒覆盖；0 表示不回收；未设置或非法值回落到默认。
func poolIdleTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv(poolIdleTTLEnv))
	if v == "" {
		return defaultPoolIdleTTL
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec < 0 {
		log.Printf("[mcp-pool] invalid %s=%q, fallback to %s", poolIdleTTLEnv, v, defaultPoolIdleTTL)
		return defaultPoolIdleTTL
	}
	return time.Duration(sec) * time.Second
}

// CloseAll 关闭池内全部连接，供进程优雅退出时调用。
//
// 为什么必须显式调用：这些连接持有 npx / python 子进程（以及它们拉起的
// 浏览器）。Windows 上父进程退出**不会**自动带走子进程，不主动关就会留下
// 一批孤儿进程。并发关闭，整体耗时约等于最慢的那一条。
func CloseAll() {
	globalPool.mu.Lock()
	entries := make([]*pooledConn, 0, len(globalPool.entries))
	for _, c := range globalPool.entries {
		entries = append(entries, c)
	}
	globalPool.entries = make(map[string]*pooledConn)
	globalPool.mu.Unlock()

	if len(entries) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, c := range entries {
		wg.Add(1)
		go func(c *pooledConn) {
			defer wg.Done()
			c.close()
		}(c)
	}
	wg.Wait()
	log.Printf("[mcp-pool] closed %d pooled connection(s)", len(entries))
}
