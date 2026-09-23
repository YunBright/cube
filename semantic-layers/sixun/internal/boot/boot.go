// Package boot 提供六层(sixun) cube-app 的环境变量加载 + 启动辅助。
//
// 设计要点:
//   - 一个二进制(sixun-ysx / sixun-hbposv7)可启动多个实例;
//     每个实例用 CUBE_APP_ID 区分(例:sixun-ysx-00、sixun-ysx-baiyuan1)
//   - family / version 由 app_id 拆分得到(不需单独环境变量)
//     例:sixun-ysx-00 → family="sixun", version="ysx", instance="00"
//   - DSN 通过 ./config.yaml 的 source.dsn 读(保持兼容旧部署);
//     DUCKDB / MAPPING / PORT 全部走环境变量,这样同 binary 多次复用
//
// 启动流程(由 cmd/{sixun-ysx,sixun-hbposv7}/main.go 编排):
//
//	cfg, err := boot.Load()           // 读 env,校验
//	if err != nil { os.Exit(1) }
//	lg := log.New(cfg.AppID)
//	// 加载 mapping / schema / duckdb ...
//	engine.POST("/query", queryHandler(...))
//	engine.GET("/healthz", cfg.HealthHandler(supportedModels))
//	go registerToGateway(cfg.RegisterBody(supportedModels))
//	engine.Run(cfg.Port)
package boot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// appIDRE 限制 dapr app-id / CUBE_APP_ID 的字符集。
//
// gateway 端的 sourceFormatRE 接受 \w-(\w = 字母数字下划线)。
// dapr 的 app-id 实际上不允许下划线,因此这里收紧到 [a-z0-9-]。
var appIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

// Config 是从环境变量 + config.yaml 读出来的启动配置。
type Config struct {
	// AppID = dapr app-id / 数据源 source id(例:"sixun-ysx-00")
	AppID string
	// Family 从 AppID 拆分得到(第 1 段)。
	Family string
	// Version 从 AppID 拆分得到(第 2 段)。
	Version string
	// Instance 从 AppID 拆分得到(第 3 段)—— 门店 / 租户别名。
	Instance string

	// Port 是 gin engine 监听端口。默认 ":8080"。
	Port string
	// MappingDir 是 mapping-*.yaml 所在目录。默认 "./mapping"。
	MappingDir string
	// ModelsDir 是 sixun-models 仓库路径(env CUBE_MODELS_DIR)。
	// 解析策略见 ResolveModelsDir。
	ModelsDir string
	// DuckDBPath 是本地 DuckDB 文件路径(env CUBE_DUCKDB_PATH);空时按
	// DefaultDuckDBPath() 退到 ./data/<AppID>.duckdb。
	DuckDBPath string
	// GatewayURL 是 cube-gateway 的 HTTP base URL(env CUBE_GATEWAY_URL);默认 http://localhost:8080。
	GatewayURL string
}

// Load 读环境变量,校验,派生 Family/Version/Instance。
//
// 必填: CUBE_APP_ID
// 选填: CUBE_PORT=":8080" / CUBE_MAPPING_DIR="./mapping" /
//       CUBE_MODELS_DIR / CUBE_DUCKDB_PATH / CUBE_GATEWAY_URL="http://localhost:8080"
//
// 失败时返回的 err 形如 "boot: CUBE_APP_ID required"。
func Load() (*Config, error) {
	appID := strings.TrimSpace(os.Getenv("CUBE_APP_ID"))
	if appID == "" {
		return nil, fmt.Errorf("boot: CUBE_APP_ID required")
	}
	if !appIDRE.MatchString(appID) {
		return nil, fmt.Errorf("boot: CUBE_APP_ID %q must match ^[a-z0-9-]+$ (no underscore, no leading/trailing hyphen)", appID)
	}
	parts := strings.Split(appID, "-")
	if len(parts) < 3 {
		return nil, fmt.Errorf("boot: CUBE_APP_ID %q must have at least 3 segments (family-version-instance)", appID)
	}
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("boot: CUBE_APP_ID %q has empty segment", appID)
		}
	}

	port := os.Getenv("CUBE_PORT")
	if port == "" {
		port = ":8080"
	}

	mappingDir := os.Getenv("CUBE_MAPPING_DIR")
	if mappingDir == "" {
		mappingDir = "./mapping"
	}

	gatewayURL := os.Getenv("CUBE_GATEWAY_URL")
	if gatewayURL == "" {
		gatewayURL = "http://localhost:8080"
	}

	return &Config{
		AppID:      appID,
		Family:     parts[0],
		Version:    parts[1],
		Instance:   parts[2],
		Port:       port,
		MappingDir: mappingDir,
		ModelsDir:  os.Getenv("CUBE_MODELS_DIR"),
		DuckDBPath: os.Getenv("CUBE_DUCKDB_PATH"),
		GatewayURL: gatewayURL,
	}, nil
}

// DefaultDuckDBPath 返回默认 DuckDB 文件路径。
//
// 约定: ./data/<AppID>.duckdb,目录会在调用方负责创建(本函数只返回路径)。
func (c *Config) DefaultDuckDBPath() string {
	return filepath.Join("./data", c.AppID+".duckdb")
}

// ResolveDuckDBPath 优先用 CUBE_DUCKDB_PATH,空时退回 DefaultDuckDBPath。
func (c *Config) ResolveDuckDBPath() string {
	if c.DuckDBPath != "" {
		return c.DuckDBPath
	}
	return c.DefaultDuckDBPath()
}

// ResolveModelsDir 解析 sixun-models 路径。
//
// 顺序:
//  1. CUBE_MODELS_DIR(env)
//  2. ../../../sixun-models(从 cmd/<family>-<version>/ 走的相对路径)
//  3. ./sixun-models(本地 dev)
//
// 找到第一个存在的路径;都不存在返回第一个 env 值(让 main.go 报错)。
func (c *Config) ResolveModelsDir() string {
	if c.ModelsDir != "" {
		if _, err := os.Stat(c.ModelsDir); err == nil {
			return c.ModelsDir
		}
	}
	candidates := []string{
		filepath.Join("..", "..", "..", "..", "sixun-models"),
		"./sixun-models",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if c.ModelsDir != "" {
		return c.ModelsDir
	}
	return candidates[0]
}

// HealthHandler 返回 gin GET /healthz handler,JSON 包含 source / family /
// version / models / status,方便外部探活 + 调试。
//
// status="ok" / "degraded" 由调用方决定;这里固定 "ok",主流程挂了 gin 自然不响应。
func (c *Config) HealthHandler(registeredModels []string) gin.HandlerFunc {
	startedAt := time.Now()
	return func(ctx *gin.Context) {
		ctx.JSON(200, gin.H{
			"status":  "ok",
			"source":  c.AppID,
			"family":  c.Family,
			"version": c.Version,
			"models":  registeredModels,
			"uptime":  time.Since(startedAt).String(),
		})
	}
}

// RegisterBody 构造 /register 请求体。
//
// 注册体里 family / version 是从 AppID 派生的(冗余写入,
// gateway 不重新解析);source 字段 == AppID,保持 wire 一致。
func (c *Config) RegisterBody(registeredModels []string) []byte {
	body := map[string]any{
		"app_id":       c.AppID,
		"family":       c.Family,
		"version":      c.Version,
		"source":       c.AppID,
		"models":       registeredModels,
		"capabilities": []string{"query", "preagg", "cache_l2"},
		"health_url":   "/healthz",
	}
	b, _ := json.Marshal(body)
	return b
}
