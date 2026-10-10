// Package stream — 跨进程事件转发（方案 B：HTTP 内部回调）。
//
// Hub 是进程内内存广播，仅覆盖同一进程里的订阅者。scheduler 与 server
// 是独立进程，scheduler 侧 actuator 发布的 turn_done / delta / task_done
// 必须通过 HTTP 回调转发到 server，再由 server 转成本地 Hub 广播，
// 才能送达 server 进程里的 SSE 订阅者。
//
// 设计约束：推送是「尽力而为」，绝不阻塞执行链路 —— Forward 非阻塞入队，
// 单 worker 串行发送保证顺序，队列满丢弃最新，发送失败只记日志不重试，
// 最终一致性由前端轮询兜底。
package stream

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// internalEventPath server 侧接收内部事件的路由（见 handler/router.go）。
const internalEventPath = "/internal/events"

// DefaultForwardBufferSize 转发队列缓冲。一个 task 的 turn 数 + 流式 delta
// 通常远小于此值；满则丢弃，由前端轮询补齐。
const DefaultForwardBufferSize = 1024

// DefaultForwardTimeout 单次转发的 HTTP 超时。本地/内网回调应在毫秒级完成，
// 超时即视为失败放弃，不阻塞后续事件。
const DefaultForwardTimeout = 2 * time.Second

// Forwarder 跨进程事件转发器。实现必须保证 Forward 非阻塞（快速返回），
// 由内部 worker 异步发送。
type Forwarder interface {
	Forward(ev Event)
}

// forwarder 进程级单例。只有 scheduler 进程会注册，server 进程不注册，
// 因此 server 侧调用 stream.Publish 不会自回调。
var forwarder atomic.Value // 存 Forwarder

// SetForwarder 注册全局转发器。scheduler 进程启动时调用一次。
func SetForwarder(f Forwarder) {
	forwarder.Store(f)
}

// Publish 广播事件：先做进程内 Hub 广播，再转发给跨进程消费者。
//
// scheduler 侧 actuator / container 统一改调本函数（替代 stream.GetHub().Publish），
// 这样同进程订阅者仍能即时收到，同时事件也会异步推给 server 的 SSE。
// server 侧自身发布（如 StopTask）仍走 GetHub().Publish，避免自回调。
func Publish(ev Event) {
	globalHub.Publish(ev)
	if f, ok := forwarder.Load().(Forwarder); ok && f != nil {
		f.Forward(ev)
	}
}

// HTTPForwarder 通过 HTTP POST 把事件转发到 server 的 /internal/events。
type HTTPForwarder struct {
	url    string
	token  string
	client *http.Client
	queue  chan Event
	done   chan struct{}
}

// NewHTTPForwarder 创建转发器并启动内部发送 worker。
// baseURL 为 server 根地址（如 http://127.0.0.1:8080），不含路径。
func NewHTTPForwarder(baseURL, token string) *HTTPForwarder {
	f := &HTTPForwarder{
		url:    strings.TrimRight(baseURL, "/") + internalEventPath,
		token:  token,
		client: &http.Client{Timeout: DefaultForwardTimeout},
		queue:  make(chan Event, DefaultForwardBufferSize),
		done:   make(chan struct{}),
	}
	go f.run()
	return f
}

// Forward 非阻塞入队，由 worker 异步发送。队列满丢弃最新事件。
func (f *HTTPForwarder) Forward(ev Event) {
	select {
	case f.queue <- ev:
	default:
		log.Printf("[stream-forward] queue full, drop event %s (task %s)", ev.Type, ev.TaskID)
	}
}

// Close 停止 worker（优雅退出，等待当前发送完成）。
func (f *HTTPForwarder) Close() {
	close(f.done)
}

// run 串行消费队列，保证事件按发布顺序送达。
func (f *HTTPForwarder) run() {
	for {
		select {
		case ev := <-f.queue:
			f.send(ev)
		case <-f.done:
			return
		}
	}
}

// send 发送单条事件，失败只记日志不重试。
func (f *HTTPForwarder) send(ev Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		log.Printf("[stream-forward] marshal event %s failed: %v", ev.Type, err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, f.url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[stream-forward] build request failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if f.token != "" {
		req.Header.Set("X-Internal-Token", f.token)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		log.Printf("[stream-forward] post event %s (task %s) failed: %v", ev.Type, ev.TaskID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[stream-forward] post event %s (task %s) got status %d", ev.Type, ev.TaskID, resp.StatusCode)
	}
}
