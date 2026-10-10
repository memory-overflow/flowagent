package stream

import (
	"testing"
	"time"
)

func TestPublishDeliversToSubscriber(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe("task-1", 8)
	defer cancel()

	h.Publish(Event{Type: EventTypeTurnDone, TaskID: "task-1", TurnID: 1})

	select {
	case ev := <-ch:
		if ev.Type != EventTypeTurnDone || ev.TaskID != "task-1" || ev.TurnID != 1 {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestPublishIgnoresUnknownTask(t *testing.T) {
	h := NewHub()
	// 无订阅者时 Publish 不应 panic 或阻塞
	h.Publish(Event{Type: EventTypeTaskDone, TaskID: "no-such-task"})
}

func TestBufferOverflowDropsOldest(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe("task-1", 2)
	defer cancel()

	// 塞入 3 条，缓冲 2 → 第 1 条被丢弃，后两条保留
	for i := 1; i <= 3; i++ {
		h.Publish(Event{Type: EventTypeTurnDone, TaskID: "task-1", TurnID: int64(i)})
	}

	// 读到的第一条应是 TurnID=2（TurnID=1 已丢）
	ev := <-ch
	if ev.TurnID != 2 {
		t.Fatalf("expected TurnID=2 as first retained event, got %d", ev.TurnID)
	}
	ev = <-ch
	if ev.TurnID != 3 {
		t.Fatalf("expected TurnID=3, got %d", ev.TurnID)
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe("task-1", 8)

	h.Publish(Event{Type: EventTypeTurnDone, TaskID: "task-1", TurnID: 1})
	<-ch

	cancel()
	h.Publish(Event{Type: EventTypeTurnDone, TaskID: "task-1", TurnID: 2})

	select {
	case ev := <-ch:
		t.Fatalf("received event after cancel: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// 预期无事件
	}
}

func TestLastSubscriberCleanup(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe("task-1", 8)
	cancel()

	h.mu.RLock()
	_, ok := h.topics["task-1"]
	h.mu.RUnlock()
	if ok {
		t.Fatal("expected topic to be removed after last subscriber cancelled")
	}
}
