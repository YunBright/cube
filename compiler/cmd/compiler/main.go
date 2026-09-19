// Command cube-compiler 是固定 dapr app id 的编译守护进程。
//
// 职责(P0):
//   1. git pull 本项目仓库
//   2. 扫描 semantic-layers/ 下所有 cube app
//   3. 检测文件变化 → go build → 重新启动
//   4. 通过 pubsub 通知 cube-gateway 刷新注册表
//
// 部署:
//   - 本地 dev:直接 go run ./cmd/compiler,用 dapr run 包一层
//   - 生产 hosted:k8s Deployment + RBAC 调 k8s API 重启 Pod(MVP 不实现)
//
// HTTP 端点(gin-gonic/gin):
//   - POST /reload/:app_id  cube-compiler 触发 reload
//   - GET  /health          健康检查
package main

import (
	"context"

	"github.com/YunBright/cube/compiler/internal/daprmanager"
	"github.com/YunBright/cube/pkg/config"
	"github.com/YunBright/cube/pkg/log"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := config.NewDefaultLoader("./config.yaml")
	if err != nil {
		panic(err)
	}
	lg := log.New("cube-compiler")

	// dapr cube app 进程管理器(MVP 只做 dev:本机进程)
	dm := daprmanager.New(daprmanager.Config{
		OutputDir: cfg.String("build.output_dir"),
		LocalRepo: cfg.String("git.local_path"),
	}, lg)

	// gin engine:显式加 logger + recovery(不用 gin.Default())
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())

	// POST /reload/:app_id  cube-compiler 触发 reload
	engine.POST("/reload/:app_id", func(c *gin.Context) {
		appID := c.Param("app_id")
		if err := dm.Reload(c.Request.Context(), appID); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	})

	// GET /health  健康检查
	engine.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok", "app_id": "cube-compiler"})
	})

	addr := cfg.String("server.http_addr")
	if addr == "" {
		addr = ":8081"
	}
	lg.Info("cube-compiler starting", "addr", addr)
	if err := engine.Run(addr); err != nil {
		lg.Info("cube-compiler exit", "err", err.Error())
	}

	// 保留 ctx,避免 unused
	_ = context.Background()
}