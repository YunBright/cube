package middleware

import (
	"io"

	"github.com/YunBright/cube/pkg/apierror"
	"github.com/YunBright/cube/pkg/log"

	"github.com/gin-gonic/gin"
)

// ErrorRecovery 替换 gin.Recovery():panic 时记日志,然后用 INTERNAL_ERROR 错误
// 包络响应(代替 gin 默认的 500 + stack trace)。
func ErrorRecovery(lg *log.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		rid, _ := c.Request.Context().Value(apierror.CtxKeyRequestID).(string)
		if lg != nil {
			lg.Error("panic recovered",
				"request_id", rid,
				"path", c.Request.URL.Path,
				"method", c.Request.Method,
				"panic", recovered,
			)
		}
		WriteError(c, apierror.New(apierror.INTERNAL_ERROR, "internal server error"))
	})
}
