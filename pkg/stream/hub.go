// Package stream 提供任务执行过程的进程内事件广播（SSE 的数据源）。
//
// 定位：actuator / container 在任务推进的关键节点（turn 落库、任务终态）发布事件，
// handler 层的 SSE 连接按 taskID 订阅并转发给前端。它是纯内存的尽力而为通道，
// 不做持久化、不保证投递 —— 断连、进程重启、缓冲溢出都会丢事件，
// 最终一致性由前端的轮询兜底。
package stream

import "sync"

// 事件类型。snapshot 为连接建立时下发的全量快照；turn_done / task_done
// 为增量事件；tool_calling 通知某个工具调用已开始执行（turn 已建、尚未完成）；
// delta 预留给流式文本（P1），当前不发送；compacting 通知「正在压缩上下文」
// （携带 phase=start/done），压缩会阻塞本轮 LLM 调用数秒到数十秒，
// 不给提示前端看起来就是卡住了。
const (
	EventTypeSnapshot    = "snapshot"
	EventTypeTurnDone    = "turn_done"
	EventTypeTaskDone    = "task_done"
	EventTypeToolCalling = "tool_calling"
	EventTypeDelta       = "delta"
	EventTypeCompacting  = "compacting"
)

// DefaultBufferSize 订阅者默认缓冲大小。一个 task 的 turn 数通常远小于此值，
// 缓冲不足时丢弃最旧事件，由前端轮询补齐。
const DefaultBufferSize = 128

// Event 一条广播事件。
//
// 事件负载刻意保持轻量（只带定位信息），完整数据由前端收到事件后
// 通过既有接口（getTaskDetail / listTaskTurns）回拉 —— 这样事件里
// 的元数据与 DB 永远一致，不会出现「事件带了旧数据」的漂移问题。
type Event struct {
	Type    string `json:"type"`
	TaskID  string `json:"task_id"`
	TurnID  int64  `json:"turn_id,omitempty"`
	TurnSeq int32  `json:"turn_seq,omitempty"`
	// Data 附加负载（snapshot 时为全量 turn 视图，task_done 时为终态摘要）。
	Data interface{} `json:"data,omitempty"`
}

// subscriber 单个订阅者。取消订阅只从 topic 移除，不关闭 channel ——
// 关闭后 Publish 写已关闭 channel 会 panic，且订阅方用 context 感知退出。
type subscriber struct {
	ch chan Event
}

// topic 一个任务的订阅者集合。
type topic struct {
	mu   sync.RWMutex
	subs map[int64]*subscriber
	next int64
}

// Hub 进程内事件广播中心。
type Hub struct {
	mu     sync.RWMutex
	topics map[string]*topic
}

// NewHub 创建广播中心。
func NewHub() *Hub {
	return &Hub{topics: make(map[string]*topic)}
}

// globalHub 全局单例。actuator / container / handler 三处通过 GetHub 共享。
var globalHub = NewHub()

// GetHub 返回全局广播中心。
func GetHub() *Hub {
	return globalHub
}

// Subscribe 订阅指定任务的事件流。
//
// 返回只读 channel 与取消函数。取消函数幂等：多次调用无副作用，
// 当 topic 最后一个订阅者退出时一并清理 topic，避免任务数增长导致内存累积。
func (h *Hub) Subscribe(taskID string, bufferSize int) (<-chan Event, func()) {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}

	h.mu.Lock()
	t := h.topics[taskID]
	if t == nil {
		t = &topic{subs: make(map[int64]*subscriber)}
		h.topics[taskID] = t
	}
	t.mu.Lock()
	id := t.next
	t.next++
	sub := &subscriber{ch: make(chan Event, bufferSize)}
	t.subs[id] = sub
	t.mu.Unlock()
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.unsubscribe(taskID, id)
		})
	}
	return sub.ch, cancel
}

// unsubscribe 从 topic 移除订阅者；若已无订阅者则删除 topic。
func (h *Hub) unsubscribe(taskID string, id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	t := h.topics[taskID]
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.subs, id)
	empty := len(t.subs) == 0
	t.mu.Unlock()

	if empty {
		delete(h.topics, taskID)
	}
}

// Publish 向订阅了该任务的所有客户端广播事件。
//
// 非阻塞：订阅者缓冲已满时丢弃最旧一条再塞入最新。丢弃是刻意的 ——
// 实时性由 SSE 尽力保证，完整性由轮询兜底，不值得为等一个慢客户端
// 阻塞执行链路（actuator 在任务执行的关键路径上调用 Publish）。
func (h *Hub) Publish(ev Event) {
	h.mu.RLock()
	t := h.topics[ev.TaskID]
	h.mu.RUnlock()
	if t == nil {
		return
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, sub := range t.subs {
		select {
		case sub.ch <- ev:
		default:
			// 满：丢弃最旧，塞入最新
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}
