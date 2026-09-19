// Package handler 是 cube-gateway 的 HTTP handlers(基于 gin-gonic/gin)。
package handler

import (
	"github.com/YunBright/cube/gateway/internal/l1cache"
	"github.com/YunBright/cube/gateway/internal/registry"
	"github.com/YunBright/cube/gateway/internal/router"
	"github.com/YunBright/cube/pkg/cache"
	"github.com/YunBright/cube/pkg/config"
	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"

	"github.com/gin-gonic/gin"
)

// Deps 是 handler 的依赖注入。
type Deps struct {
	Cfg      config.Loader
	Logger   *log.Logger
	Dapr     daprclient.Client
	Registry *registry.Registry
	Router   *router.Router
	L1Cache  *l1cache.Cache
}

// Handler 持有依赖,提供注册方法(handler 签名为 gin 的 func(*gin.Context))。
type Handler struct{ Deps }

// New 构造 Handler。
func New(d Deps) *Handler { return &Handler{Deps: d} }

// Register 处理 dapr cube app 的启动注册。
//
// 请求体:见 registry.AppInfo
// 流程:解析 → 写注册表(in-memory + 持久化到 dapr state store)
func (h *Handler) Register(c *gin.Context) {
	var info registry.AppInfo
	if err := c.ShouldBindJSON(&info); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err := h.Registry.Register(c.Request.Context(), &info); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	h.Logger.Info("cube app registered", "registered_app_id", info.AppID, "family", info.Family, "version", info.Version)
	c.Status(204)
}

// Load 是 /v1/load cube 兼容查询入口(P1-8 选 B)。
//
// 流程:
//   1. 解析 cube query → 提取 model
//   2. router.Route(model) → target app_id
//   3. L1 缓存命中直接返回(P1-7 选 A)
//   4. 未命中 → dapr invocation 调 target app(query + metadata)
//   5. 写 L1 缓存 → 返回
func (h *Handler) Load(c *gin.Context) {
	// 用 c.GetRawData() 一次性读取 body,后续 Parse + cache key 共用
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	q, err := cubequery.Parse(body)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	model := q.Model()
	if model == "" {
		c.JSON(400, gin.H{"error": "cannot infer model from query"})
		return
	}

	appID, ok := h.Router.Route(model)
	if !ok {
		c.JSON(404, gin.H{"error": "no app registered for model " + model})
		return
	}

	// TODO: P1-6 粗粒度权限校验(谁能访问 model)
	// p := auth.PrincipalFromMetadata(md); if !h.Authorizer.Allow(p, model) { 403 }

	// L1 缓存 key(model + query + principal + tenant + freshness)
	// TODO: 实际从 invocation metadata 取 principal
	keyBuilder := &cache.KeyBuilder{}
	cacheKey := keyBuilder.Build(model, body, "anon", "default", "v1")

	if cached, hit := h.L1Cache.Get(cacheKey); hit {
		c.Header("X-Cube-Cache", "L1-HIT")
		c.Data(200, "application/json", cached)
		return
	}

	// L1 miss → dapr invocation 调 cube app
	result, err := h.Dapr.InvokeMethod(c.Request.Context(), appID, "/query", body, nil)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}

	// 写 L1 缓存
	h.L1Cache.Set(cacheKey, result, 0)

	c.Header("X-Cube-Cache", "L1-MISS")
	c.Data(200, "application/json", result)
}

// Meta 是 /v1/meta cube 兼容元信息入口(P1-8 选 B)。
//
// 聚合所有注册 cube app 的 models,返回给 BI 工具自助发现 schema。
func (h *Handler) Meta(c *gin.Context) {
	apps := h.Registry.All()
	resp := gin.H{
		"cube_version": "1.0",
		"apps":         apps,
	}
	c.JSON(200, resp)
}