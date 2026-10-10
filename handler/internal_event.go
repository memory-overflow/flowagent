package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/memory-overflow/flowagent/pkg/stream"
)

// internalToken 内部事件鉴权 token。server 启动时由 config 注入；
// 为空表示不校验（仅限内网 / 本地开发），非空则要求请求携带同名 header。
var internalToken string

// SetInternalToken 设置内部事件鉴权 token。由 cmd/server 启动时调用。
func SetInternalToken(token string) {
	internalToken = token
}

// InternalEvent 接收 scheduler 进程回调的事件，转成本地 Hub 广播，
// 从而送达本进程里的 SSE 订阅者。
//
// 这是方案 B（HTTP 内部回调）在 server 侧的收口：scheduler 的
// stream.HTTPForwarder POST 到这里，本接口解析后调 GetHub().Publish，
// 事件即进入 server 进程内的 stream.Hub，SSE 的 Subscribe 随即收到。
func InternalEvent(c *gin.Context) {
	// 鉴权：token 非空时校验 X-Internal-Token header。
	if internalToken != "" && c.GetHeader("X-Internal-Token") != internalToken {
		c.JSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "msg": "invalid internal token"})
		return
	}

	var ev stream.Event
	if err := c.ShouldBindJSON(&ev); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": http.StatusBadRequest, "msg": "invalid event payload"})
		return
	}
	if ev.TaskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": http.StatusBadRequest, "msg": "task_id required"})
		return
	}

	stream.GetHub().Publish(ev)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
