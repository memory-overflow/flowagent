package mcp

import (
	"os"
	"testing"
	"time"
)

// newTestConn 造一个不连真实 server 的池内条目。
// 回收判定只读 cfg / dead / lastUsed，不碰 cli，所以 cli 留 nil 是安全的。
func newTestConn(name string, serverID string, lastUsed time.Time) *pooledConn {
	c := &pooledConn{
		cfg: &ServerConfig{ServerName: name, ServerID: serverID, Transport: "stdio"},
		sem: make(chan struct{}, 1),
	}
	c.lastUsed.Store(lastUsed.UnixNano())
	return c
}

func TestPoolKey(t *testing.T) {
	// DB 里的 server 用主键区分；内置 MCP（ServerID 为空）回落到名字，
	// 且要和真实主键不同名，避免撞车。
	if got := poolKey(&ServerConfig{ServerID: "abc", ServerName: "playwright"}); got != "abc" {
		t.Errorf("ServerID 应作为键，得到 %q", got)
	}
	if got := poolKey(&ServerConfig{ServerName: "playwright"}); got != "builtin:playwright" {
		t.Errorf("内置 server 键应为 builtin:playwright，得到 %q", got)
	}
}

func TestPoolable(t *testing.T) {
	cases := map[string]bool{
		"stdio":           true,
		"STDIO":           true,
		"sse":             false,
		"streamable_http": false,
		"":                false,
	}
	for transport, want := range cases {
		if got := poolable(&ServerConfig{Transport: transport}); got != want {
			t.Errorf("poolable(%q) = %v, 期望 %v", transport, got, want)
		}
	}
}

func TestPoolIdleTTL(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv(poolIdleTTLEnv) })

	os.Unsetenv(poolIdleTTLEnv)
	if got := poolIdleTTL(); got != defaultPoolIdleTTL {
		t.Errorf("未设置时应回落默认值，得到 %v", got)
	}

	os.Setenv(poolIdleTTLEnv, "90")
	if got := poolIdleTTL(); got != 90*time.Second {
		t.Errorf("应解析为 90s，得到 %v", got)
	}

	// 0 是合法值：表示常驻不回收，不能被当成非法值回落到默认值。
	os.Setenv(poolIdleTTLEnv, "0")
	if got := poolIdleTTL(); got != 0 {
		t.Errorf("0 应表示不回收，得到 %v", got)
	}

	for _, bad := range []string{"abc", "-5", "3.5"} {
		os.Setenv(poolIdleTTLEnv, bad)
		if got := poolIdleTTL(); got != defaultPoolIdleTTL {
			t.Errorf("非法值 %q 应回落默认值，得到 %v", bad, got)
		}
	}
}

// TestReapOnce 是回收判定的核心用例：只回收「空闲超时」和「已失效」的条目，
// 正在心跳中的活跃连接绝不能被误回收。
func TestReapOnce(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv(poolIdleTTLEnv) })
	os.Setenv(poolIdleTTLEnv, "60")

	now := time.Now()
	live := newTestConn("live", "id-live", now)                                  // 刚用过
	boundary := newTestConn("boundary", "id-boundary", now.Add(-60*time.Second)) // 恰好到 TTL
	idle := newTestConn("idle", "id-idle", now.Add(-10*time.Minute))             // 明显超时
	dead := newTestConn("dead", "id-dead", now.Add(-time.Second))                // 很新但已失效
	dead.dead.Store(true)

	p := &connPool{entries: map[string]*pooledConn{
		"id-live":     live,
		"id-boundary": boundary,
		"id-idle":     idle,
		"id-dead":     dead,
	}}

	victims := p.reapOnce(now)

	got := map[string]bool{}
	for _, c := range victims {
		got[c.cfg.ServerName] = true
	}
	if len(victims) != 3 {
		t.Fatalf("应回收 3 条，实际 %d 条: %v", len(victims), got)
	}
	for _, name := range []string{"boundary", "idle", "dead"} {
		if !got[name] {
			t.Errorf("%s 应被回收", name)
		}
	}
	if got["live"] {
		t.Errorf("刚用过的连接被误回收")
	}

	// 回收的必须已经从池里摘掉，否则下一次 acquire 会拿到旧条目
	if len(p.entries) != 1 {
		t.Fatalf("池内应只剩 1 条，实际 %d", len(p.entries))
	}
	if _, ok := p.entries["id-live"]; !ok {
		t.Errorf("活跃连接不应被移出池")
	}
}

// TestReapOnceDisabled 验证 MCP_POOL_IDLE_TTL=0 时永不回收（浏览器常驻场景）。
func TestReapOnceDisabled(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv(poolIdleTTLEnv) })
	os.Setenv(poolIdleTTLEnv, "0")

	now := time.Now()
	old := newTestConn("old", "id-old", now.Add(-24*time.Hour))
	p := &connPool{entries: map[string]*pooledConn{"id-old": old}}

	if victims := p.reapOnce(now); len(victims) != 0 {
		t.Errorf("TTL=0 时不应回收任何连接，实际回收 %d 条", len(victims))
	}
}

// TestReapOnceEmpty 空池不应 panic（启动后从未调用过任何 MCP 工具时就会走到这）。
func TestReapOnceEmpty(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv(poolIdleTTLEnv) })
	os.Unsetenv(poolIdleTTLEnv)

	p := &connPool{}
	if victims := p.reapOnce(time.Now()); len(victims) != 0 {
		t.Errorf("空池应返回空，实际 %d 条", len(victims))
	}
}

// TestIsReusableConnErr 只有「请求确定未送达」的错误才允许重试 ——
// 否则会把工具的副作用执行两次（例如点两次同一个按钮）。
func TestIsReusableConnErr(t *testing.T) {
	if !isReusableConnErr(errPoolClosed) {
		t.Error("errPoolClosed 应可重试")
	}
	if isReusableConnErr(nil) {
		t.Error("nil 不应被判为可重试")
	}
	if isReusableConnErr(os.ErrNotExist) {
		t.Error("无关错误不应被判为可重试")
	}
}
