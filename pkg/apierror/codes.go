// Package apierror 定义跨 gateway / cube-app 边界的统一错误模型。
//
// 设计要点:
//   - 机器可读的 Code 枚举(13 个,见 codes.go)
//   - 每个 Code 对应一个 HTTP status(HTTPStatus(c Code) int)
//   - Error 结构携带 Code / Message / Details(可选 k-v) / Cause(底层错误)
//   - Write 适配(gateway/internal/handler/apierror_writer.go)把 Error 序列化为
//     gin 响应,并在 ctx 含 request_id 时回填 Details.request_id 与 X-Request-Id 头
//
// 调用方典型用法:
//
//	if err != nil {
//	    apierror.Write(c, apierror.New(apierror.SOURCE_NOT_REGISTERED,
//	        "source not registered: "+source).
//	        WithDetails(map[string]any{"source": source}))
//	    return
//	}
package apierror

import "net/http"

// Code 是机器可读的错误分类。值用 SCREAMING_SNAKE_CASE 字符串,便于日志聚合。
type Code string

const (
	SOURCE_FORMAT_INVALID     Code = "SOURCE_FORMAT_INVALID"     // 400 — URL 段不匹配 3 段格式
	SOURCE_NOT_REGISTERED     Code = "SOURCE_NOT_REGISTERED"     // 404 — gateway 从未收到该 source 的 /register
	SOURCE_OFFLINE            Code = "SOURCE_OFFLINE"            // 503 — 已注册但 LastSeen 过期 / dapr 无法连 / upstream 404
	UPSTREAM_TIMEOUT          Code = "UPSTREAM_TIMEOUT"          // 504 — ctx deadline exceeded
	VERSION_UNSUPPORTED       Code = "VERSION_UNSUPPORTED"       // 400 — cube app 返回 VERSION_UNSUPPORTED 子码
	MODEL_NOT_FOUND_IN_SOURCE Code = "MODEL_NOT_FOUND_IN_SOURCE" // 404 — cube app 返回 MODEL_NOT_FOUND 子码
	QUERY_PARSE_ERROR         Code = "QUERY_PARSE_ERROR"         // 400 — body 不可读 / JSON 解析失败
	QUERY_INVALID             Code = "QUERY_INVALID"             // 400 — JSON 合法但语义不合法(空 model / 空 measures)
	NO_PRINCIPAL              Code = "NO_PRINCIPAL"              // 401 — AuthRequired 且无 Authorization
	FORBIDDEN                 Code = "FORBIDDEN"                 // 403 — Authorizer.Allow 返回 false
	RATE_LIMITED              Code = "RATE_LIMITED"              // 429 — 预留,MVP 不接入
	INTERNAL_ERROR            Code = "INTERNAL_ERROR"            // 500 — gateway panic / 未预期错误
	UPSTREAM_ERROR            Code = "UPSTREAM_ERROR"            // 502 — 上游返回非 2xx 但不属于上面分类
)

// codeHTTPStatus 是 Code → HTTP status 的单一映射表。
//
// 新增 Code 时必须在此登记,否则 HTTPStatus 退化为 500 并记日志。
var codeHTTPStatus = map[Code]int{
	SOURCE_FORMAT_INVALID:     http.StatusBadRequest,
	SOURCE_NOT_REGISTERED:     http.StatusNotFound,
	SOURCE_OFFLINE:            http.StatusServiceUnavailable,
	UPSTREAM_TIMEOUT:          http.StatusGatewayTimeout,
	VERSION_UNSUPPORTED:       http.StatusBadRequest,
	MODEL_NOT_FOUND_IN_SOURCE: http.StatusNotFound,
	QUERY_PARSE_ERROR:         http.StatusBadRequest,
	QUERY_INVALID:             http.StatusBadRequest,
	NO_PRINCIPAL:              http.StatusUnauthorized,
	FORBIDDEN:                 http.StatusForbidden,
	RATE_LIMITED:              http.StatusTooManyRequests,
	INTERNAL_ERROR:            http.StatusInternalServerError,
	UPSTREAM_ERROR:            http.StatusBadGateway,
}

// HTTPStatus 返回 c 对应的 HTTP 状态码。未知 Code 退化为 500。
func HTTPStatus(c Code) int {
	if s, ok := codeHTTPStatus[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// AllCodes 返回所有已登记的 Code(测试用)。
func AllCodes() []Code {
	out := make([]Code, 0, len(codeHTTPStatus))
	for c := range codeHTTPStatus {
		out = append(out, c)
	}
	return out
}
