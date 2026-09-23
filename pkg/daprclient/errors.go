package daprclient

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// ErrKind 区分 dapr invocation 的失败原因。
//
// 调用方用 errors.As(&invErr) 拿到 *InvokeError,再读 Kind 字段决定如何响应
// (gateway:Timeout → 504;ConnFailure → 503;HTTPStatus → 按 StatusCode 进一步分支)。
type ErrKind int

const (
	// ErrNone 不是错误(零值)。
	ErrNone ErrKind = iota
	// ErrTimeout ctx deadline exceeded 或 http.Client 超时。
	ErrTimeout
	// ErrConnFailure 连接被拒 / DNS 失败 / TLS 失败 / 响应前 EOF。
	ErrConnFailure
	// ErrHTTPStatus 上游返回了非 2xx 响应(带状态码 + body)。
	ErrHTTPStatus
)

// String 让日志 / 调试更易读。
func (k ErrKind) String() string {
	switch k {
	case ErrNone:
		return "none"
	case ErrTimeout:
		return "timeout"
	case ErrConnFailure:
		return "conn_failure"
	case ErrHTTPStatus:
		return "http_status"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

// InvokeError 是 dapr-client 的 typed 错误,区分 Timeout / ConnFailure / HTTPStatus。
//
// 重要:本类型不应与 HTTP status 一一对应 —— 同一上游 5xx 既可以映射到
// gateway 的 UPSTREAM_ERROR,也可以因业务子码映射到 VERSION_UNSUPPORTED 等。
// 上层 handler 读 Kind + StatusCode + Body 三者后做最终映射。
type InvokeError struct {
	// TargetAppID 是被调用的 dapr app-id(也用作 source 的等价)。
	TargetAppID string
	// Method 是被调用的 HTTP method(/query 等)。
	Method string

	Kind ErrKind
	// StatusCode 仅在 Kind == ErrHTTPStatus 时有意义。
	StatusCode int
	// Body 仅在 Kind == ErrHTTPStatus 时填充 —— 上游响应 body。
	Body []byte
	// Cause 是底层 transport error(若 Kind != ErrHTTPStatus)。nil 表示无。
	Cause error
}

// Error 实现 error 接口。
func (e *InvokeError) Error() string {
	if e == nil {
		return "<nil InvokeError>"
	}
	switch e.Kind {
	case ErrHTTPStatus:
		return fmt.Sprintf("dapr invoke %s/%s: upstream %d body=%s",
			e.TargetAppID, e.Method, e.StatusCode, truncateBytes(e.Body, 256))
	case ErrTimeout:
		return fmt.Sprintf("dapr invoke %s/%s: timeout", e.TargetAppID, e.Method)
	case ErrConnFailure:
		return fmt.Sprintf("dapr invoke %s/%s: connection failure: %v",
			e.TargetAppID, e.Method, e.Cause)
	default:
		return fmt.Sprintf("dapr invoke %s/%s: kind=%s", e.TargetAppID, e.Method, e.Kind)
	}
}

// Unwrap 让 errors.Is 能匹配到底层 transport error。
func (e *InvokeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// IsTimeout 便捷判断。
func (e *InvokeError) IsTimeout() bool {
	return e != nil && e.Kind == ErrTimeout
}

// IsConnErr 便捷判断。
func (e *InvokeError) IsConnErr() bool {
	return e != nil && e.Kind == ErrConnFailure
}

// IsHTTPStatus 便捷判断。
func (e *InvokeError) IsHTTPStatus() bool {
	return e != nil && e.Kind == ErrHTTPStatus
}

// truncateBytes 是本文件内部 helper(避免在 http.go 里再写一份)。
func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// isDeadlineLike 判断 err 是否属于 ctx deadline / http.Client 超时这一族。
//
// 包括:
//   - context.DeadlineExceeded / context.Canceled(ctx 取消也算 timeout,因为
//     客户端代码常用 ctx.WithTimeout 触发取消)
//   - net.Error.Timeout()(dial / TLS handshake / response headers 阶段的超时)
func isDeadlineLike(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// classifyTransportErr 把 transport 层错误归类为 ErrTimeout 或 ErrConnFailure。
// 调用方负责提供 TargetAppID/Method 元数据。
func classifyTransportErr(target, method string, err error) *InvokeError {
	if isDeadlineLike(err) {
		return &InvokeError{
			TargetAppID: target,
			Method:      method,
			Kind:        ErrTimeout,
			Cause:       err,
		}
	}
	return &InvokeError{
		TargetAppID: target,
		Method:      method,
		Kind:        ErrConnFailure,
		Cause:       err,
	}
}
