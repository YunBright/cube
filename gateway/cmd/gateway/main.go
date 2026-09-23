// Command cube-gateway 是固定 dapr app id 的语义层网关入口。
//
// 端点(基于 gin-gonic/gin):
//
//	POST /register               dapr cube app 启动注册
//	POST /v1/source/:source/load cube 兼容查询(取代旧的 /v1/load)
//	GET  /v1/sources             列出已注册 source 及其状态(取代旧的 /v1/meta)
//	GET  /healthz                健康检查
//
// 流程(BI → gateway → cube app):
//  1. /v1/source/{source}/load  → 解析 cube query → 查注册表得到 dapr app_id(== source)
//  2. 查 L1 缓存 → 命中直接返回
//  3. 未命中 → 经 dapr invocation 转发到 cube app(透传 principal/tenant)
//  4. 写 L1 缓存 + MarkSeen → 返回 BI
//
// v2 改动:
//   - 不再有 model → app_id 路由,直接 source → app_id(1:1)
//   - 引入中间件:request_id(全链路追踪)+ error_recovery(panic → INTERNAL_ERROR 包络)
//   - 删除 router 包;source 是 URL 段,直接 reg.LookupByID(source)
package main

import (
	"fmt"
	"os"

	"github.com/YunBright/cube/gateway/internal/handler"
	"github.com/YunBright/cube/gateway/internal/l1cache"
	"github.com/YunBright/cube/gateway/internal/middleware"
	"github.com/YunBright/cube/gateway/internal/registry"
	"github.com/YunBright/cube/pkg/config"
	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := config.NewDefaultLoader("./config.yaml")
	if err != nil {
		panic(err)
	}
	lg := log.New("cube-gateway")

	// dapr client:经本机 dapr-sidecar HTTP 端口调其它 dapr app。
	//
	// 端口约定:dapr run 启动时把 DAPR_HTTP_PORT 写入进程 env(本 service 单元 = 3000),
	// 这里直接读 env 拿到 sidecar HTTP URL。fallback localhost:3000。
	sidecarAddr := os.Getenv("DAPR_HTTP_ENDPOINT")
	if sidecarAddr == "" {
		port := os.Getenv("DAPR_HTTP_PORT")
		if port == "" {
			port = "3000"
		}
		sidecarAddr = "http://127.0.0.1:" + port
	}
	lg.Info("dapr sidecar endpoint", "addr", sidecarAddr)
	dapr := daprclient.NewHTTP(sidecarAddr)

	// 注册表:从 dapr state store 读 + 写
	reg := registry.New(dapr, cfg.String("dapr.state_store"), lg)

	// L1 缓存
	cch := l1cache.New(cfg.Int("l1_cache.ttl_seconds"))

	// HTTP handlers(Auth 暂为 nil,MVP 阶段无强制鉴权)
	h := handler.New(handler.Deps{
		Logger:   lg,
		Dapr:     dapr,
		Registry: reg,
		L1Cache:  cch,
		Auth:     nil,
	})

	// gin engine:显式加 request_id + error_recovery(不用 gin.Logger() / gin.Recovery())
	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.Use(middleware.ErrorRecovery(lg))

	engine.POST("/register", h.Register)
	engine.POST("/v1/source/:source/load", h.SourceLoad)
	engine.GET("/v1/sources", h.ListSources)
	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok", "app_id": "cube-gateway"})
	})

	addr := cfg.String("server.http_addr")
	if addr == "" {
		addr = ":8080"
	}
	lg.Info("cube-gateway starting", "addr", addr)
	if err := engine.Run(addr); err != nil {
		lg.Info("cube-gateway exit", "err", err.Error())
	}
}

// 防止 import 被裁掉的占位 —— 保留 dumpBody/jsonMustMarshal 给 dev 用,
// 后面步骤会清理或补全。
var (
	_ = fmt.Println
)
