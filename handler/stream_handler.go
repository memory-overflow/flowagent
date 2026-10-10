package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/pkg/stream"
)

// sseHeartbeatInterval SSE 心跳间隔。用注释行（: ping）保活，
// 防止中间层 / 代理因长时间无数据把连接超时掐断。
const sseHeartbeatInterval = 15 * time.Second

// TaskStream 以 SSE 推送任务执行过程。
//
// 时序：
//  1. 校验任务存在，建立 SSE 连接（text/event-stream）
//  2. 先推一条 snapshot —— 已完成的全部 turn，保证新连接的客户端
//     立即看到历史，而不是干等下一个增量
//  3. 订阅 stream.Hub，转发 turn_done / task_done 增量事件
//  4. 任务已终态 → 推 task_done 后关闭；否则靠心跳保活，客户端断连即退出
//
// 设计定位：SSE 是「尽力而为」的实时通道，不保证不丢事件。
// 前端在断连、缓冲溢出时降级回轮询接口兜底，最终一致由轮询保证。
func TaskStream(c *gin.Context) {
	taskID := c.Query("task_id")
	if taskID == "" {
		c.JSON(http.StatusBadRequest, CommonResponse{Code: http.StatusBadRequest, Msg: "task_id 必填"})
		return
	}

	taskDAO := &dao.TaskDAO{}
	task, err := taskDAO.GetTaskByID(c.Request.Context(), taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, CommonResponse{Code: http.StatusInternalServerError, Msg: "查询任务失败"})
		return
	}
	if task == nil {
		c.JSON(http.StatusNotFound, CommonResponse{Code: http.StatusNotFound, Msg: "任务不存在"})
		return
	}

	// SSE 响应头。X-Accel-Buffering 关闭 nginx 层缓冲，否则代理会把流攒成一段才下发。
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	// 1. 快照。失败不阻断连接，仍继续推增量。
	snapshot, snapErr := buildTaskTurnsSnapshot(c.Request.Context(), taskID)
	if snapErr != nil {
		snapshot = gin.H{"error": snapErr.Error()}
	}
	writeSSEEvent(c, stream.Event{
		Type: stream.EventTypeSnapshot, TaskID: taskID, Data: snapshot,
	})

	// 2. 已终态 → 补一条 task_done 后结束，前端据此停止等待。
	if task.IsTerminal() {
		writeSSEEvent(c, stream.Event{
			Type: stream.EventTypeTaskDone, TaskID: taskID,
			Data: gin.H{"status": int(task.Status), "status_name": taskStatusName(task.Status)},
		})
		return
	}

	// 3. 订阅增量
	ch, cancel := stream.GetHub().Subscribe(taskID, stream.DefaultBufferSize)
	defer cancel()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case ev := <-ch:
			writeSSEEvent(c, ev)
			if ev.Type == stream.EventTypeTaskDone {
				return
			}
		case <-heartbeat.C:
			if _, err := c.Writer.WriteString(": ping\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case <-c.Request.Context().Done():
			return
		}
	}
}

// writeSSEEvent 写一条 SSE 事件。
// type 同时落到 event: 行与 data JSON 内：前端既可用 addEventListener(type)
// 精确监听，也可 onmessage 后按 JSON 的 type 字段统一分发。
func writeSSEEvent(c *gin.Context, ev stream.Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if _, err := c.Writer.WriteString("event: " + ev.Type + "\n"); err != nil {
		return
	}
	if _, err := c.Writer.WriteString("data: " + string(payload) + "\n\n"); err != nil {
		return
	}
	c.Writer.Flush()
}
