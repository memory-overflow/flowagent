package handler

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/pkg/logger"
)

// CommonResponse 统一响应结构
type CommonResponse struct {
	Code  int         `json:"code"`
	Msg   string      `json:"msg"`
	ReqID string      `json:"req_id"`
	Data  interface{} `json:"data,omitempty"`
}

// HandlerFunc 处理器函数签名
type HandlerFunc func(ctx *gin.Context, req interface{}) (resp interface{}, err error)

// BinderFunc 请求绑定函数签名
type BinderFunc func() interface{}

// Wrap 将 HandlerFunc 包装为 gin.HandlerFunc
func Wrap(binder BinderFunc, handler HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := c.GetHeader("X-Request-ID")
		if reqID == "" {
			reqID = c.GetString("req_id")
		}
		if reqID == "" {
			reqID = uuid.New().String()
		}
		c.Set("req_id", reqID)

		path := c.Request.URL.Path

		// 绑定请求
		var req interface{}
		if binder != nil {
			req = binder()
		}

		// 解析请求体
		if req != nil {
			body, err := c.GetRawData()
			if err != nil {
				log.Printf("[handler][%s] path=%s read body failed: %v", reqID, path, err)
				writeError(c, http.StatusBadRequest, "解析请求体失败", reqID)
				return
			}
			if len(body) > 0 {
				if err := json.Unmarshal(body, req); err != nil {
					log.Printf("[handler][%s] path=%s unmarshal failed: %v raw=%s", reqID, path, err, string(body))
					writeError(c, http.StatusBadRequest, "请求参数格式错误: "+err.Error(), reqID)
					return
				}
			}
		}

		// 打印请求（debug 级：完整请求体较啰嗦，默认级别不输出）
		if reqBytes, err := json.Marshal(req); err == nil {
			logger.Debugf("[handler][%s] path=%s req=%s", reqID, path, string(reqBytes))
		} else {
			logger.Debugf("[handler][%s] path=%s req=<marshal err: %v>", reqID, path, err)
		}

		// 执行 handler
		resp, err := handler(c, req)
		if err != nil {
			// 区分 APIError 和普通 error
			if apiErr, ok := err.(*APIError); ok {
				log.Printf("[handler][%s] path=%s api_error code=%d msg=%s", reqID, path, apiErr.Code, apiErr.Msg)
				writeError(c, apiErr.Code, apiErr.Msg, reqID)
			} else {
				log.Printf("[handler][%s] path=%s error: %v", reqID, path, err)
				writeError(c, http.StatusInternalServerError, "服务器内部错误", reqID)
			}
			return
		}

		// 打印响应（debug 级：完整响应体较啰嗦，默认级别不输出）
		if rspBytes, mErr := json.Marshal(resp); mErr == nil {
			logger.Debugf("[handler][%s] path=%s rsp=%s", reqID, path, string(rspBytes))
		} else {
			logger.Debugf("[handler][%s] path=%s rsp=<marshal err: %v>", reqID, path, mErr)
		}

		c.JSON(http.StatusOK, CommonResponse{
			Code:  0,
			Msg:   "ok",
			ReqID: reqID,
			Data:  resp,
		})
	}
}

func writeError(c *gin.Context, code int, msg string, reqID string) {
	httpStatus := http.StatusOK
	if code >= 400 && code < 600 {
		httpStatus = code
	}
	c.JSON(httpStatus, CommonResponse{
		Code:  code,
		Msg:   msg,
		ReqID: reqID,
	})
}

// ============================================================
// 界面语言协商（仅影响「展示文案」，不影响给 LLM 的 prompt）
// ============================================================
//
// 前端在 axios 拦截器里统一带 X-Lang: zh|en；缺省 zh。
// handler 用 LangFromContext + PickDisplay 把中英双份字段折成展示值：
//   - 英文条目（*En）为空时回落中文原文 —— 所以用户自建、未翻译的条目
//     在英文界面下显示中文，不会出现空白。
//   - 中文永远是基准数据（DB 权威值），英文只是展示层的翻译。

const (
	// CtxKeyUILang 存进 gin.Context 的语言键
	CtxKeyUILang = "ui_lang"
	// HeaderUILang 前端携带界面语言的请求头
	HeaderUILang = "X-Lang"
)

// LangFromContext 读取界面语言，仅 zh / en 两个取值，缺省 zh。
func LangFromContext(c *gin.Context) string {
	lang := c.GetHeader(HeaderUILang)
	if lang == "en" {
		return "en"
	}
	return "zh"
}

// PickDisplay 按语言挑选展示值。en 且 enVal 非空返回 enVal，否则一律回落中文原值。
func PickDisplay(lang, zhVal, enVal string) string {
	if lang == "en" && enVal != "" {
		return enVal
	}
	return zhVal
}
