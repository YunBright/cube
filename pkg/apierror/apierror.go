package apierror

import (
	"context"
	"fmt"
)

// ctxKey 是非导出的 context key 类型,防止外部包误用。
type ctxKey int

const (
	// CtxKeyRequestID 是 gateway/internal/middleware/request_id 注入 ctx 的 request_id key。
	// gin 适配层(gateway/internal/handler/apierror_writer.go)从这里读 request_id
	// 回填到 Details 与响应头。
	CtxKeyRequestID ctxKey = iota
)

// Error 是可序列化的 wire 错误。
//
// JSON 形状:
//
//	{
//	  "code":    "SOURCE_NOT_REGISTERED",
//	  "message": "source not registered: sixun-ysx-99",
//	  "details": { "source": "sixun-ysx-99", "request_id": "abc" }
//	}
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	// Cause 是底层错误(如有)。不会序列化到 wire,只用于 errors.Unwrap / errors.Is。
	Cause error `json:"-"`
}

// New 构造 Error。
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Newf 构造 Error,msg 用 fmt.Sprintf 格式化。
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithDetails 追加 details(返回新对象;原 Error 不变)。
//
// 多次调用会 merge,后写覆盖先写。
func (e *Error) WithDetails(d map[string]any) *Error {
	cp := *e
	if cp.Details == nil {
		cp.Details = map[string]any{}
	} else {
		merged := make(map[string]any, len(cp.Details)+len(d))
		for k, v := range cp.Details {
			merged[k] = v
		}
		cp.Details = merged
	}
	for k, v := range d {
		cp.Details[k] = v
	}
	return &cp
}

// WithDetail 单 key 追加。
func (e *Error) WithDetail(key string, value any) *Error {
	return e.WithDetails(map[string]any{key: value})
}

// WithCause 记录底层错误(不进入 wire,仅 errors.Unwrap 链路用)。
func (e *Error) WithCause(cause error) *Error {
	cp := *e
	cp.Cause = cause
	return &cp
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e == nil {
		return "<nil apierror>"
	}
	if e.Cause != nil {
		return string(e.Code) + ": " + e.Message + ": " + e.Cause.Error()
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap 让 errors.Is / errors.As 能穿透到 Cause。
func (e *Error) Unwrap() error { return e.Cause }

// HTTPStatus 返回本 Error 对应的 HTTP 状态码(等于 HTTPStatus(e.Code))。
func (e *Error) HTTPStatus() int { return HTTPStatus(e.Code) }

// RequestIDFromCtx 读取 ctx 中由 middleware 注入的 request_id。
//
// 返回 ("", false) 表示 ctx 中无 request_id。
func RequestIDFromCtx(ctx context.Context) (string, bool) {
	v := ctx.Value(CtxKeyRequestID)
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
