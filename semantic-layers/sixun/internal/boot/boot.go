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
//	engine.GET("/healthz", cfg.HealthHandler(supportedModels, fresh))
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

	"github.com/YunBright/cube/semantic-layers/sixun/internal/freshness"
)

// appIDRE 限制 dapr app-id / CUBE_APP_ID 的字符集。
//
// gateway 端的 sourceFormatRE 接受 \w-(\w = 字母数字下划线)。
// dapr 的 app-id 实际上不允许下划线,因此这里收紧到 [a-z0-9-]。
var appIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

// Config 是从环境变量 + config.yaml 读出来的启动配置。
//
// v2 关键不变量(解耦后):
//
//	wire source id (AppID)        = "<family>-<version>-<instance>"
//	dapr app-id  (DaprAppID)       = "cube-<family>-<version>-<instance>"  (默认推导)
//
// 两者字符集都受 dapr 限制(无下划线 / 数字 / 小写字母 / 短横线),CUBE_DAPR_APP_ID
// env 可显式覆盖(罕见)。
type Config struct {
	// AppID 是 wire source id(注册到 gateway 时上报 + URL /v1/source/{source}/load 段)。
	// 例:"sixun-hbposv7-jiale"(不带 cube- 前缀)。
	AppID string
	// DaprAppID 是 dapr sidecar 用的 app-id(`dapr run --app-id <DaprAppID>`),
	// gateway 转发时按它寻址 cube app。默认 "cube-" + AppID;env CUBE_DAPR_APP_ID 覆盖。
	DaprAppID string
	// Family 从 AppID 拆分得到(第 1 段)。
	Family string
	// Version 从 AppID 拆分得到(第 2 段)。
	Version string
	// Instance 从 AppID 拆分得到(第 3 段)—— 门店 / 租户别名。
	Instance string

	// Port 是 gin engine 监听端口。默认 ":8080"。
	Port string
	// DuckDBPath 是本地 DuckDB 文件路径(env CUBE_DUCKDB_PATH);空时按
	// DefaultDuckDBPath() 退到 ./data/<AppID>.duckdb。
	DuckDBPath string
	// GatewayURL 是 cube-gateway 的 HTTP base URL(env CUBE_GATEWAY_URL);默认 http://localhost:8080。
	GatewayURL string
	// SidecarHTTPURL 是本机 dapr sidecar 的 HTTP base(env DAPR_HTTP_PORT)。
	// 登记 dapr job 时要用 —— sidecar 的 jobs API 在它上面。
	//
	// 注意:这个端口**不是**应用端口。cube 的应用在 :8083,
	// sidecar 的 jobs API 在 :3003。打错会拿到 connection refused。
	SidecarHTTPURL string
	// RefreshEvery 是 storage: duck 模型的定时重拉间隔(env CUBE_REFRESH_EVERY)。
	//
	// duck model 是**启动时的快照**,不重拉就一直是旧值(门店改了商品价、
	// 改了供应商名都要重启才生效)。live model 查询时透传源库,本来就没有快照,
	// 不参与这个间隔。
	//
	// 默认 30m;设 0 关闭定时重拉(退化成"只在启动时拉一次")。
	RefreshEvery time.Duration
}

// RefreshInterval 返回重拉间隔;未配置时给一个合理的默认。
func (c *Config) RefreshInterval() time.Duration {
	if c.RefreshEvery == 0 {
		return 30 * time.Minute
	}
	return c.RefreshEvery
}

// Load 读环境变量,校验,派生 Family/Version/Instance / DaprAppID。
//
// 必填: CUBE_APP_ID
// 选填:
//   - DAPR_APP_ID       — dapr sidecar 注入,作为 daprAppID 首选源(本进程在 dapr
//     下启动时 dapr 自动 set;非 dapr 环境下为空)
//   - CUBE_DAPR_APP_ID  — 手动覆盖,优先级低于 DAPR_APP_ID,空字符串视为未设
//   - CUBE_PORT=":8080"
//   - CUBE_DUCKDB_PATH
//   - CUBE_GATEWAY_URL="http://localhost:8080"
//
// 注意:schema.yaml 与 mapping.yaml 都用 go:embed 编译进二进制,不再有
// CUBE_MODELS_DIR / CUBE_MAPPING_DIR。读盘路径曾经是真实故障源 ——
// 路径对不上时 schema 一个都加载不到,但启动不报错,要到查询时才炸,
// 而且现场看起来完全正常。这里刻意不留 env 兜底:留了兜底就会有人配它,
// 配错时又是同样的静默失败。
//
// DaprAppID 解析顺序(首个非空胜出):
//  1. DAPR_APP_ID      — dapr sidecar 注入的"事实源"(最权威)
//  2. CUBE_DAPR_APP_ID — 运维手动覆盖(罕见;主要是裸起 / 调试时用)
//  3. "cube-" + CUBE_APP_ID — 推导默认值
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

	// DaprAppID 三档优先级。
	daprAppID := strings.TrimSpace(os.Getenv("DAPR_APP_ID"))
	if daprAppID == "" {
		daprAppID = strings.TrimSpace(os.Getenv("CUBE_DAPR_APP_ID"))
	}
	if daprAppID == "" {
		daprAppID = "cube-" + appID
	}
	if !appIDRE.MatchString(daprAppID) {
		return nil, fmt.Errorf("boot: dapr app-id %q must match ^[a-z0-9-]+$ (source: %s)", daprAppID, daprAppIDSource())
	}

	port := os.Getenv("CUBE_PORT")
	if port == "" {
		port = ":8080"
	}

	gatewayURL := os.Getenv("CUBE_GATEWAY_URL")
	if gatewayURL == "" {
		gatewayURL = "http://localhost:8080"
	}

	// dapr sidecar 的 HTTP 端口。dapr run 会注入 DAPR_HTTP_PORT;
	// 非 dapr 环境(本地裸跑)没这个变量,留空 —— 刷新会自动退回进程内 ticker。
	sidecarHTTPURL := ""
	if p := strings.TrimSpace(os.Getenv("DAPR_HTTP_PORT")); p != "" {
		sidecarHTTPURL = "http://127.0.0.1:" + p
	}

	// 重拉间隔。写成 duration("30m" / "1h");**配错必须报错**而不是退回默认 ——
	// "我以为设了 1 小时,其实拼错了被忽略"会让人一直看着旧数据排查半天。
	refreshEvery := 30 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("CUBE_REFRESH_EVERY")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("boot: CUBE_REFRESH_EVERY %q 不是合法 duration(如 30m / 1h / 0): %w", raw, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("boot: CUBE_REFRESH_EVERY %q 不能为负(要关闭请显式写 0)", raw)
		}
		refreshEvery = d
	}

	return &Config{
		AppID:      appID,
		DaprAppID:  daprAppID,
		Family:     parts[0],
		Version:    parts[1],
		Instance:   parts[2],
		Port:       port,
		DuckDBPath: os.Getenv("CUBE_DUCKDB_PATH"),
		GatewayURL: gatewayURL,

		SidecarHTTPURL: sidecarHTTPURL,

		RefreshEvery: refreshEvery,
	}, nil
}

// daprAppIDSource 返回 daprAppID 来源的诊断标签(DAPR_APP_ID / CUBE_DAPR_APP_ID / 推导),
// 用于错误日志和 HealthHandler 调试输出。
func daprAppIDSource() string {
	if v := strings.TrimSpace(os.Getenv("DAPR_APP_ID")); v != "" {
		return "DAPR_APP_ID"
	}
	if v := strings.TrimSpace(os.Getenv("CUBE_DAPR_APP_ID")); v != "" {
		return "CUBE_DAPR_APP_ID"
	}
	return "derived"
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

// HealthHandler 返回 gin GET /healthz handler,JSON 包含 source / family /
// version / models / status,方便外部探活 + 调试。
//
// fresh 为 nil 时不输出 data 段(便于不依赖它的调用方 / 测试)。
// refresh 是一个**每请求现取**的 provider(通常是 sched.Stats 方法值),
// 返回 nil 时不输出 refresh 段。
//
// 为什么必须是 func 而不是值:写成 `HealthHandler(..., sched.Stats())` 时,
// Stats() 在 handler 构造那一刻就求值了,之后 healthz 里的
// triggered / skipped / data_age 会**永久冻结在启动时的值** ——
// 看起来完全正常,只是再也不反映运行期。2026-10-10 实测踩过:
// 日志里明明有 "dapr job triggered",healthz 却显示 triggered:0。
// 取 any 是为了让 boot 不反向依赖 internal/refreshschedule ——
// boot 是最底层的启动辅助,不该被拖进 dapr 依赖树。
//
// status 由 fresh 决定:数据从没成功加载过、最近一次刷新有 model 失败、
// 或数据年龄超过重拉间隔两倍,都是 "degraded"。
// **degraded 不改 HTTP 状态码**(仍 200)—— 数据旧 ≠ 服务不可用,
// 返 503 会让 dapr / gateway 把它当宕机从而拒绝查询,把降级问题升级成不可用问题。
//
// wire / dapr 两套 id 同时回(运维需要):
//   - source      == c.AppID    (URL / /register 用)
//   - dapr_app_id == c.DaprAppID (dapr sidecar 寻址用)
//
// 想要"过期就硬失败"(拒绝对不健康 app 的 invoke),用 dapr 的
// `--enable-app-health-check --app-health-check-path /healthz`,
// 那是显式的运维选择,不是这里的默认行为。
func (c *Config) HealthHandler(registeredModels []string, fresh *freshness.Tracker, refresh func() any) gin.HandlerFunc {
	startedAt := time.Now()
	return func(ctx *gin.Context) {
		body := gin.H{
			"source":      c.AppID,
			"dapr_app_id": c.DaprAppID,
			"family":      c.Family,
			"version":     c.Version,
			"models":      registeredModels,
			"uptime":      time.Since(startedAt).String(),
			"status":      "ok",
		}
		if fresh != nil {
			snap := fresh.Snapshot()
			body["status"] = snap.Status
			if snap.Reason != "" {
				body["reason"] = snap.Reason
			}
			body["data"] = snap
		}
		if refresh != nil {
			if v := refresh(); v != nil {
				body["refresh"] = v
			}
		}
		ctx.JSON(200, body)
	}
}

// RegisterBody 构造 /register 请求体。
//
// 注册体里 family / version 是从 AppID 派生的(冗余写入,
// gateway 不重新解析);source 字段 == AppID,保持 wire 一致。
//
// v2 关键: dapr_app_id 显式上报 — gateway 用它寻址 dapr app,
// 不是直接拿 source 当 app-id。
//
// 协议说明见 cube/docs/dapr-app-contract.md §2。
func (c *Config) RegisterBody(registeredModels []string) []byte {
	body := map[string]any{
		"app_id":       c.AppID,     // wire source id
		"dapr_app_id":  c.DaprAppID, // dapr sidecar app-id(寻址用)
		"family":       c.Family,
		"version":      c.Version,
		"source":       c.AppID, // == app_id(冗余,wire 兼容)
		"models":       registeredModels,
		"capabilities": []string{"query", "preagg", "cache_l2"},
		"health_url":   "/healthz",
	}
	b, _ := json.Marshal(body)
	return b
}

// UnregisterBody 构造 /unregister 请求体。
//
// 对称 RegisterBody:cube app 在 SIGTERM/SIGINT 时 POST 给 cube-gateway,
// gateway 立即从 registry 删条目;幂等,gateway 重启 / 已被 unregister 也返 204。
//
// 协议说明见 cube/docs/dapr-app-contract.md §2。
func (c *Config) UnregisterBody() []byte {
	body := map[string]any{
		"app_id": c.AppID,
	}
	b, _ := json.Marshal(body)
	return b
}
