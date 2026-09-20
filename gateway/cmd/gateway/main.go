// Command cube-gateway 是固定 dapr app id 的语义层网关入口。
//
// 端点(基于 gin-gonic/gin):
//
//	POST /register    dapr cube app 启动注册
//	POST /v1/load     cube 兼容查询(P1-8 选 B,不含 /v1/sql)
//	GET  /v1/meta     cube 兼容元信息(从注册表聚合)
//	GET  /health      健康检查
//
// 流程(BI → gateway → cube app):
//  1. /v1/load  → 解析 cube query → 查注册表得到 target app_id
//  2. 查 L1 缓存 → 命中直接返回(P1-7 选 A)
//  3. 未命中 → 经 dapr invocation 转发到 cube app(透传 principal)
//  4. 写 L1 缓存 → 返回 BI
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/YunBright/cube/gateway/internal/handler"
	"github.com/YunBright/cube/gateway/internal/l1cache"
	"github.com/YunBright/cube/gateway/internal/registry"
	"github.com/YunBright/cube/gateway/internal/router"
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

	// 路由:model → app_id(优先查注册表,fallback 到 config.yaml)
	rtr := router.New(reg, parseStaticRouting(cfg))

	// L1 缓存
	cch := l1cache.New(cfg.Int("l1_cache.ttl_seconds"))

	// HTTP handlers
	h := handler.New(handler.Deps{
		Cfg:      cfg,
		Logger:   lg,
		Dapr:     dapr,
		Registry: reg,
		Router:   rtr,
		L1Cache:  cch,
	})

	// gin engine:显式加 logger + recovery(不用 gin.Default())
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())

	engine.POST("/register", h.Register)
	engine.POST("/v1/load", h.Load)
	engine.GET("/v1/meta", h.Meta)
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

// parseStaticRouting 解析 config.yaml 的 routing 段(dev fallback)。
func parseStaticRouting(_ config.Loader) []router.StaticRoute {
	// TODO: 解析 yaml
	return nil
}

// debug helper:把 body 打印到 stderr,辅助早期调试。
func dumpBody(body []byte, tag string) {
	if len(body) > 0 {
		_ = tag
		fmt.Println(tag, string(body))
	}
}

// jsonMustMarshal 调试用。
func jsonMustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
