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
// 请求体:见 registry.AppInfo。校验 AppID 3 段格式 → 校验 dapr_app_id
// 字符集(空则 fallback 到 AppID)→ 写注册表 → 持久化。
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
		var daprErr *registry.ErrDaprAppIDInvalid
		if errors.As(err, &daprErr) {
			WriteError(c, apierror.New(apierror.DAPR_APP_ID_INVALID,
				"dapr_app_id must match ^[a-z0-9][a-z0-9-]*[a-z0-9]$").
				WithDetails(map[string]any{
					"app_id":      info.AppID,
					"dapr_app_id": daprErr.Value,
				}))
			return
		}
		WriteError(c, apierror.New(apierror.INTERNAL_ERROR, "register failed: "+err.Error()))
		return
	}
	h.Logger.Info("cube app registered",
		"registered_app_id", info.AppID,
		"dapr_app_id", info.DaprAppID,
		"family", info.Family,
		"version", info.Version,
		"models", len(info.Models),
	)
	c.Status(http.StatusNoContent)
}

// Unregister 处理 cube app 优雅关闭时的 unregister(POST /unregister)。
//
// 请求体:{"app_id": "<family>-<version>-<instance>"}
//
// 行为:
//   - app_id 格式校验失败 → 400
//   - 调 registry.Unregister —— **幂等**:无论 source 是否已注册都返 204
//     (cube app 在 SIGTERM/SIGINT 路径上调用,gateway 重启 / 已 unregister 都不应阻塞退出)
//   - 持久化层失败仅记日志,不阻塞响应
//
// 协议说明见 cube/docs/dapr-app-contract.md §2。
func (h *Handler) Unregister(c *gin.Context) {
	var body struct {
		AppID string `json:"app_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		WriteError(c, apierror.New(apierror.QUERY_PARSE_ERROR, "invalid unregister body: "+err.Error()))
		return
	}
	if body.AppID == "" || !sourceFormatRE.MatchString(body.AppID) {
		WriteError(c, apierror.New(apierror.SOURCE_FORMAT_INVALID,
			"app_id must be 3 hyphen-delimited segments").
			WithDetails(map[string]any{"app_id": body.AppID}))
		return
	}
	deleted := h.Registry.Unregister(c.Request.Context(), body.AppID)
	h.Logger.Info("cube app unregistered",
		"app_id", body.AppID,
		"was_registered", deleted,
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
	// 不再前置 IsOnline 判定 —— 改为被动:任何已注册 source 都尝试 invoke,
	// 让 dapr 真实失败信号(ErrConnFailure / ErrHTTPStatus)决定最终错误码。
	// 这避免了"注册后无人调 → 90s 后假 offline"的伪信号。

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

	// 寻址:dapr_app_id(由 cube app 注册时上报) — 不是 URL 上的 source。
	// plan B 解耦后,wire source id 与 dapr app-id 可以不同
	// (例:sixun-hbposv7-jiale 寻址 cube-sixun-hbposv7-jiale)。
	daprTarget := info.DaprAppID
	if daprTarget == "" {
		// 极端兜底:registry 应保证非空。空了 fallback 到 source,日志告警。
		h.Logger.Warn("registry entry has empty dapr_app_id, fallback to source",
			"source", source)
		daprTarget = source
	}

	extra := map[string]string{
		daprclient.MetadataKeyPrincipal: principal,
		daprclient.MetadataKeyTenant:    defaultTenant,
		"Authorization":                 c.Request.Header.Get("Authorization"),
	}
	result, err := h.Dapr.InvokeMethod(ctx, daprTarget, "/query", body, extra)
	if err != nil {
		writeUpstreamError(c, source, daprTarget, model, err)
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
// 取代旧的 /v1/meta。每条 entry 同时暴露 wire source id 与 dapr_app_id
// (plan B:两者可不同),运维可一眼看清"URL 上写的 id 实际寻址到哪个 dapr app"。
func (h *Handler) ListSources(c *gin.Context) {
	infos := h.Registry.All()
	type entry struct {
		Source       string    `json:"source"`       // wire id(URL 用)
		DaprAppID    string    `json:"dapr_app_id"`  // dapr sidecar app-id(寻址用)
		Family       string    `json:"family"`
		Version      string    `json:"version"`
		Models       []string  `json:"models"`
		Capabilities []string  `json:"capabilities"`
		Status       string    `json:"status"`       // 总是 "online" —— 被动验证,真实可用性看实际 invoke
		LastSeen     time.Time `json:"last_seen"`     // 仅供运维排查;不做离线判定
	}
	out := make([]entry, 0, len(infos))
	for _, info := range infos {
		daprID := info.DaprAppID
		if daprID == "" {
			daprID = info.AppID // 旧条目 fallback,新条目注册时已兜底
		}
		out = append(out, entry{
			Source:       info.Source,
			DaprAppID:    daprID,
			Family:       info.Family,
			Version:      info.Version,
			Models:       info.Models,
			Capabilities: info.Capabilities,
			Status:       "online",
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
// 参数:
//   - source  = URL 上的 wire id(给响应 details.source 用,人对人友好)
//   - daprApp = 实际寻址的 dapr app-id(给 SourceOfMessage 等诊断字段用,
//                让运维知道是哪个 dapr app 出的问题)
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
func writeUpstreamError(c *gin.Context, source, daprApp, model string, err error) {
	var invErr *daprclient.InvokeError
	if errors.As(err, &invErr) {
		switch invErr.Kind {
		case daprclient.ErrTimeout:
			WriteErrorWithSource(c, apierror.UPSTREAM_TIMEOUT,
				"upstream timeout: "+daprApp, source, nil)
			return
		case daprclient.ErrConnFailure:
			WriteErrorWithSource(c, apierror.SOURCE_OFFLINE,
				"source connection failure: "+daprApp, source, nil)
			return
		case daprclient.ErrHTTPStatus:
			code := mapHTTPStatusToCode(invErr.StatusCode, invErr.Body, source, daprApp, model)
			WriteError(c, code)
			return
		}
	}
	// 兜底:ctx.Err() 直接漏到调用方(fake / 自定义 client 可能这样)。
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		WriteErrorWithSource(c, apierror.UPSTREAM_TIMEOUT,
			"upstream timeout: "+daprApp, source, nil)
		return
	}
	// 非 InvokeError / 未识别 Kind —— 兜底为 UPSTREAM_ERROR
	WriteErrorWithSource(c, apierror.UPSTREAM_ERROR,
		"upstream failed: "+err.Error(), source, nil)
}

// mapHTTPStatusToCode 把上游 HTTP 状态 + body 翻译成 apierror.Code。
func mapHTTPStatusToCode(status int, body []byte, source, daprApp, model string) *apierror.Error {
	details := map[string]any{
		"source":          source,
		"dapr_app_id":     daprApp,
		"model":           model,
		"upstream_status": status,
	}
	switch {
	case status == http.StatusNotFound:
		// 404 双重含义,必须按 body 子码区分:
		//   1) cube app 返 404 + code=MODEL_NOT_FOUND → MODEL_NOT_FOUND_IN_SOURCE (404)
		//      cube app 按协议对未知 model 返这个组合(sixun-hbposv7 main.go)
		//   2) cube app 返 404 但 body 没 code → dapr sidecar 找不到目标 app,
		//      或是 gin router 没匹配上 → SOURCE_OFFLINE (503)
		// body 扫描顺序:先业务子码(MODEL_NOT_FOUND),再兜底离线。
		bodyStr := string(body)
		if strings.Contains(bodyStr, `"code":"MODEL_NOT_FOUND"`) {
			details["model"] = model
			return apierror.New(apierror.MODEL_NOT_FOUND_IN_SOURCE,
				"model not exposed by source: "+model).WithDetails(details)
		}
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
