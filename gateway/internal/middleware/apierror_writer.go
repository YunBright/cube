// Package middleware 装 cube-gateway 的 gin 中间件 + gin 适配工具。
package middleware

import (
	"github.com/YunBright/cube/pkg/apierror"

	"github.com/gin-gonic/gin"
)

// WriteError 把 *apierror.Error 序列化为 gin 响应:
//
//   - 响应头 X-Request-Id(若 ctx 含 request_id)
//   - HTTP status = apierror.HTTPStatus(code)
//   - Body = { code, message, details{...request_id...} }
//
// handler 包也可以调用本函数(handler 不能 import pkg/apierror 里的 gin 适配,
// 因为 pkg 不能依赖 gin)。
func WriteError(c *gin.Context, e *apierror.Error) {
	if e == nil {
		e = apierror.New(apierror.INTERNAL_ERROR, "unknown error")
	}
	if rid, ok := apierror.RequestIDFromCtx(c.Request.Context()); ok && rid != "" {
		c.Header("X-Request-Id", rid)
		if e.Details == nil {
			e.Details = map[string]any{}
		}
		if _, has := e.Details["request_id"]; !has {
			e.Details["request_id"] = rid
		}
	}
	c.JSON(e.HTTPStatus(), e)
}

// WriteErrorWithSource 是 source 路径上的便利包装 —— 自动把 source 加入 details。
func WriteErrorWithSource(c *gin.Context, code apierror.Code, msg, source string, extra map[string]any) {
	d := map[string]any{"source": source}
	for k, v := range extra {
		d[k] = v
	}
	WriteError(c, apierror.New(code, msg).WithDetails(d))
}
