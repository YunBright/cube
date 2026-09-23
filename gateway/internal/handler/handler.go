// Package handler 是 cube-gateway 的 HTTP handlers(基于 gin-gonic/gin)。
//
// 入口:
//   - Register   POST /register            — dapr cube app 启动注册
//   - SourceLoad POST /v1/source/:s/load   — cube-compatible 查询(取代旧的 /v1/load)
//   - ListSources GET /v1/sources          — 列出已注册 source 及其状态(取代旧的 /v1/meta)
//
// 错误模型:全部走 apierror 包,见 apierror_writer.go 与 pkg/apierror/codes.go。
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/YunBright/cube/gateway/internal/l1cache"
	"github.com/YunBright/cube/gateway/internal/middleware"
	"github.com/YunBright/cube/gateway/internal/registry"
	"github.com/YunBright/cube/pkg/apierror"
	"github.com/YunBright/cube/pkg/cache"
	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"

	"github.com/gin-gonic/gin"
)

// WriteError 是 middleware.WriteError 的本地别名(handler 包内统一调用方式)。
func WriteError(c *gin.Context, e *apierror.Error) { middleware.WriteError(c, e) }

// WriteErrorWithSource 同上。
func WriteErrorWithSource(c *gin.Context, code apierror.Code, msg, source string, extra map[string]any) {
	middleware.WriteErrorWithSource(c, code, msg, source, extra)
}

// AuthChecker 可选鉴权接口(handler 在 MVP 阶段允许 nil)。
//
// 实现由调用方注入;nil → 跳过 NO_PRINCIPAL/FORBIDDEN 检查。
type AuthChecker interface {
	// Allow 返回 principal 是否被允许访问 source + model。
	// principal="" 表示未认证(与 ctx 中无 principal 一致)。
	Allow(principal, source, model string) bool
}

// Deps 是 handler 的依赖注入。
type Deps struct {
	Logger  *log.Logger
	Dapr    daprclient.Client
	Registry *registry.Registry
	L1Cache  *l1cache.Cache
	Auth    AuthChecker // 可选;nil → 不做鉴权
}

// Handler 持有依赖,提供注册方法(handler 签名为 gin 的 func(*gin.Context))。
type Handler struct{ Deps }

// New 构造 Handler。
func New(d Deps) *Handler { return &Handler{Deps: d} }

// Register 处理 dapr cube app 的启动注册。
//
// 请求体:见 registry.AppInfo。校验 AppID 3 段格式 → 写注册表 → 持久化。
func (h *Handler) Register(c *gin.Context) {
	var info registry.AppInfo
	if err := c.ShouldBindJSON(&info); err != nil {
		WriteError(c, apierror.New(apierror.QUERY_PARSE_ERROR, "invalid register body: "+err.Error()))
		return
	}
	if err := h.Registry.Register(c.Request.Context(), &info); err != nil {
		if errors.Is(err, registry.ErrSourceFormatInvalid) {
			WriteError(c, apierror.New(apierror.SOURCE_FORMAT_INVALID,
				"app_id must be 3 hyphen-delimited segments").
				WithDetails(map[string]any{"app_id": info.AppID}))
			return
		}
		WriteError(c, apierror.New(apierror.INTERNAL_ERROR, "register failed: "+err.Error()))
		return
	}
	h.Logger.Info("cube app registered",
		"registered_app_id", info.AppID,
		"family", info.Family,
		"version", info.Version,
		"models", len(info.Models),
	)
	c.Status(http.StatusNoContent)
}

// SourceLoad 处理 cube-compatible 查询(POST /v1/source/:source/load)。
//
// 流程:
//  1. 解析 path param + 格式校验
//  2. (可选)鉴权:principal / source / model
//  3. 读 body → cubequery.Parse
//  4. registry 查 source + 在线判定
//  5. L1 缓存命中直接返回
//  6. 上游调用:ctx.WithTimeout(10s) → dapr.InvokeMethod
//  7. 错误映射:InvokeError.Kind / 业务子码 → apierror.Code
//  8. 成功:MarkSeen + 写 L1 + 200 + X-Cube-Cache: L1-MISS
func (h *Handler) SourceLoad(c *gin.Context) {
	source := c.Param("source")
	if !sourceFormatRE.MatchString(source) {
		WriteErrorWithSource(c, apierror.SOURCE_FORMAT_INVALID,
			"source must be 3 hyphen-delimited segments", source, nil)
		return
	}

	// ---- 鉴权(可选)----
	principal := defaultPrincipal
	if h.Auth != nil {
		authHeader := c.Request.Header.Get("Authorization")
		if authHeader == "" {
			WriteErrorWithSource(c, apierror.NO_PRINCIPAL,
				"Authorization header required", source, nil)
			return
		}
		// MVP:直接把整段 Authorization 当 principal token。
		// 真实实现应该解析 JWT 并提取 principal id。
		principal = authHeader
	}

	// ---- body 读取 & cube query 解析 ----
	body, err := c.GetRawData()
	if err != nil {
		WriteErrorWithSource(c, apierror.QUERY_PARSE_ERROR,
			"read body: "+err.Error(), source, nil)
		return
	}
	q, err := cubequery.Parse(body)
	if err != nil {
		WriteErrorWithSource(c, apierror.QUERY_PARSE_ERROR,
			"parse cube query: "+err.Error(), source, nil)
		return
	}
	model := q.Model()
	if model == "" {
		WriteErrorWithSource(c, apierror.QUERY_INVALID,
			"cannot infer model from query (measures/dimensions empty)", source,
			map[string]any{"model": ""})
		return
	}

	// (可选)鉴权:model 级
	if h.Auth != nil {
		if !h.Auth.Allow(principal, source, model) {
			WriteErrorWithSource(c, apierror.FORBIDDEN,
				"principal not allowed for source/model", source,
				map[string]any{"model": model})
			return
		}
	}

	// ---- 注册表查 source ----
	info, ok := h.Registry.LookupByID(source)
	if !ok {
		WriteErrorWithSource(c, apierror.SOURCE_NOT_REGISTERED,
			"source not registered: "+source, source, nil)
		return
	}
	if !info.IsOnline(h.Registry.StaleAfter()) {
		WriteErrorWithSource(c, apierror.SOURCE_OFFLINE,
			"source offline: "+source, source,
			map[string]any{"last_seen": info.LastSeen.Format(time.RFC3339)})
		return
	}

	// ---- L1 缓存 ----
	keyBuilder := &cache.KeyBuilder{}
	cacheKey := keyBuilder.Build(source, body, principal, defaultTenant, defaultFreshness)
	if cached, hit := h.L1Cache.Get(cacheKey); hit {
		c.Header("X-Cube-Cache", "L1-HIT")
		c.Data(http.StatusOK, "application/json", cached)
		return
	}

	// ---- 上游调用 ----
	ctx, cancel := context.WithTimeout(c.Request.Context(), upstreamCallTimeout)
	defer cancel()

	extra := map[string]string{
		daprclient.MetadataKeyPrincipal: principal,
		daprclient.MetadataKeyTenant:    defaultTenant,
		"Authorization":                 c.Request.Header.Get("Authorization"),
	}
	result, err := h.Dapr.InvokeMethod(ctx, source, "/query", body, extra)
	if err != nil {
		writeUpstreamError(c, source, model, err)
		return
	}

	// ---- 成功:刷新 LastSeen + 写缓存 + 返回 ----
	h.Registry.MarkSeen(source)
	h.L1Cache.Set(cacheKey, result, 0)
	c.Header("X-Cube-Cache", "L1-MISS")
	c.Data(http.StatusOK, "application/json", result)
}

// ListSources 列出已注册 source 及其状态(GET /v1/sources)。
//
// 取代旧的 /v1/meta。
func (h *Handler) ListSources(c *gin.Context) {
	staleAfter := h.Registry.StaleAfter()
	infos := h.Registry.All()
	type entry struct {
		Source       string    `json:"source"`
		Family       string    `json:"family"`
		Version      string    `json:"version"`
		Models       []string  `json:"models"`
		Capabilities []string  `json:"capabilities"`
		Status       string    `json:"status"`
		LastSeen     time.Time `json:"last_seen"`
	}
	out := make([]entry, 0, len(infos))
	for _, info := range infos {
		out = append(out, entry{
			Source:       info.Source,
			Family:       info.Family,
			Version:      info.Version,
			Models:       info.Models,
			Capabilities: info.Capabilities,
			Status:       info.Status(staleAfter),
			LastSeen:     info.LastSeen,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"cube_version": "1.0",
		"sources":      out,
	})
}

// writeUpstreamError 把 dapr InvokeError / 业务子码映射到 apierror.Code。
//
// 业务流程:
//  1. errors.As 拿 *InvokeError
//  2. 按 Kind 分支:
//     - ErrTimeout → UPSTREAM_TIMEOUT (504)
//     - ErrConnFailure → SOURCE_OFFLINE (503)
//     - ErrHTTPStatus:
//       - 404 → SOURCE_OFFLINE (dapr sidecar 找不到 app)
//       - 400 + body 含 "MODEL_NOT_FOUND" → MODEL_NOT_FOUND_IN_SOURCE
//       - 400 + body 含 "VERSION_UNSUPPORTED" → VERSION_UNSUPPORTED
//       - 5xx → UPSTREAM_ERROR with upstream_status
//       - 其他 4xx → UPSTREAM_ERROR with upstream_status + truncated body
//  3. 兜底:若 err 自身是 context.DeadlineExceeded / context.Canceled
//     (fake / 自定义 dapr client 直接返回 ctx.Err()) → UPSTREAM_TIMEOUT
//  4. 未匹配 → UPSTREAM_ERROR(generic)
func writeUpstreamError(c *gin.Context, source, model string, err error) {
	var invErr *daprclient.InvokeError
	if errors.As(err, &invErr) {
		switch invErr.Kind {
		case daprclient.ErrTimeout:
			WriteErrorWithSource(c, apierror.UPSTREAM_TIMEOUT,
				"upstream timeout: "+source, source, nil)
			return
		case daprclient.ErrConnFailure:
			WriteErrorWithSource(c, apierror.SOURCE_OFFLINE,
				"source connection failure: "+source, source, nil)
			return
		case daprclient.ErrHTTPStatus:
			code := mapHTTPStatusToCode(invErr.StatusCode, invErr.Body, source, model)
			WriteError(c, code)
			return
		}
	}
	// 兜底:ctx.Err() 直接漏到调用方(fake / 自定义 client 可能这样)。
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		WriteErrorWithSource(c, apierror.UPSTREAM_TIMEOUT,
			"upstream timeout: "+source, source, nil)
		return
	}
	// 非 InvokeError / 未识别 Kind —— 兜底为 UPSTREAM_ERROR
	WriteErrorWithSource(c, apierror.UPSTREAM_ERROR,
		"upstream failed: "+err.Error(), source, nil)
}

// mapHTTPStatusToCode 把上游 HTTP 状态 + body 翻译成 apierror.Code。
func mapHTTPStatusToCode(status int, body []byte, source, model string) *apierror.Error {
	details := map[string]any{
		"source":          source,
		"model":           model,
		"upstream_status": status,
	}
	switch {
	case status == http.StatusNotFound:
		// dapr sidecar 找不到目标 app(进程没起 / sidecar 没注册)
		return apierror.New(apierror.SOURCE_OFFLINE,
			"source not found by dapr (app may be down)").WithDetails(details)
	case status >= 500:
		return apierror.New(apierror.UPSTREAM_ERROR,
			"upstream 5xx").WithDetails(details)
	case status == http.StatusBadRequest:
		// 看 body 里的业务子码
		bodyStr := string(body)
		switch {
		case strings.Contains(bodyStr, `"code":"MODEL_NOT_FOUND"`):
			details["model"] = model
			return apierror.New(apierror.MODEL_NOT_FOUND_IN_SOURCE,
				"model not exposed by source: "+model).WithDetails(details)
		case strings.Contains(bodyStr, `"code":"VERSION_UNSUPPORTED"`):
			return apierror.New(apierror.VERSION_UNSUPPORTED,
				"source version too old").WithDetails(details)
		case strings.Contains(bodyStr, `"code":"QUERY_INVALID"`):
			return apierror.New(apierror.QUERY_INVALID,
				"source rejected query as invalid").WithDetails(details)
		}
		// 兜底 → 仍走 UPSTREAM_ERROR(带 body)
		details["upstream_body"] = truncateBytes(body, upstreamBodyTruncate)
		return apierror.New(apierror.UPSTREAM_ERROR,
			"upstream 400").WithDetails(details)
	case status >= 400:
		details["upstream_body"] = truncateBytes(body, upstreamBodyTruncate)
		return apierror.New(apierror.UPSTREAM_ERROR,
			"upstream 4xx").WithDetails(details)
	default:
		// 2xx/3xx 不会进入此函数;防御性兜底
		return apierror.New(apierror.UPSTREAM_ERROR,
			"upstream unexpected status").WithDetails(details)
	}
}
