// Package middleware 装 cube-gateway 的 gin 中间件。
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/YunBright/cube/pkg/apierror"

	"github.com/gin-gonic/gin"
)

// RequestID 是一个 gin 中间件:从 X-Request-Id 头读取,空则生成 16 字节随机 id,
// 写入响应头,并通过 ctx 的 apierror.CtxKeyRequestID 暴露给后续 handler / Write。
//
// handler 调用 apierror.Write(c, err) 时,Write 会自动从 ctx 读 request_id 并:
//   - 回填到响应头 X-Request-Id
//   - 写入 Details.request_id 字段
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-Id")
		if rid == "" {
			rid = randomRequestID()
		}
		c.Header("X-Request-Id", rid)
		ctx := context.WithValue(c.Request.Context(), apierror.CtxKeyRequestID, rid)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func randomRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read 几乎不会失败;失败时退到一个固定占位(让链路至少能跑)。
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}
