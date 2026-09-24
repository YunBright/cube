// Command sixun-hbposv7 是思迅 7pro 数据源 dapr cube app 实例。
//
// v2 改动(实例由环境变量驱动):
//   - CUBE_APP_ID 必填,例 sixun-hbposv7-jiale
//   - family / version 从 CUBE_APP_ID 拆分得到(不需单独环境变量)
//   - /healthz 返回 source/family/version/models/uptime
//   - /query 4xx 响应带 "code" 子码(MODEL_NOT_FOUND / VERSION_UNSUPPORTED 等)
//   - 注册协议用 cfg.RegisterBody(supportedModels)
//
// 启动流程:
//  1. boot.Load() → cfg(env 加载 + 校验)
//  2. ./config.yaml(DSN + 5 张表名)
//  3. cfg.MappingDir 加载 mapping-*.yaml
//  4. cfg.ResolveModelsDir() 加载 sixun-models
//  5. hbposv7.Connector 打开 SQL Server
//  6. cfg.ResolveDuckDBPath() 打开 DuckDB
//  7. 5 model:Fetch → mapping → LoadFrom → preagg.Build
//  8. goroutine: cfg.RegisterBody → POST /register
//  9. gin engine:/query /healthz
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/boot"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/hbposv7"

	categorymodel "github.com/YunBright/cube/sixun-models/category"
	productmodel "github.com/YunBright/cube/sixun-models/product"
	salemodel "github.com/YunBright/cube/sixun-models/sale_detail"
	stockmodel "github.com/YunBright/cube/sixun-models/stock"
	suppliermodel "github.com/YunBright/cube/sixun-models/supplier"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// appConfig 是本实例关心的 config.yaml 子集(只保留 DSN / 表名;server.http_addr /
// storage.duckdb_path 改由 CUBE_PORT / CUBE_DUCKDB_PATH 环境变量控制)。
type appConfig struct {
	Source struct {
		DSN           string `yaml:"dsn"`
		Version       string `yaml:"version"`
		TableSupplier string `yaml:"table_supplier"`
		TableProduct  string `yaml:"table_product"`
		TableCategory string `yaml:"table_category"`
		TableSale     string `yaml:"table_sale"`
		TableStock    string `yaml:"table_stock"`
	} `yaml:"source"`
}

type schemaMeta struct {
	Schema  *cubeschema.Model
	Columns []fieldmapping.FieldDef
}

type modelFetcher struct {
	name  string
	fetch func(context.Context) ([]map[string]any, error)
	build func(ctx context.Context, db *duckdb.Engine, rawSQL string) error
}

// 子码常量(emit 到 4xx JSON 的 "code" 字段,供 gateway 映射到 apierror.Code)。
const (
	subCodeModelNotFound     = "MODEL_NOT_FOUND"
	subCodeVersionUnsupported = "VERSION_UNSUPPORTED"
	subCodeQueryParseError   = "QUERY_PARSE_ERROR"
	subCodeQueryInvalid      = "QUERY_INVALID"
	subCodeInternalError     = "INTERNAL_ERROR"
)

// errBody 是 /query 4xx 响应的统一形状。
type errBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeErr 写 4xx 响应(带子码)。
func writeErr(c *gin.Context, status int, code, msg string, details map[string]any) {
	c.JSON(status, errBody{Code: code, Message: msg, Details: details})
}

func main() {
	ctx := context.Background()

	// 1. 环境变量加载
	cfg, err := boot.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lg := log.New(cfg.AppID).WithComponent("main")
	lg.Info("boot loaded", "family", cfg.Family, "version", cfg.Version, "instance", cfg.Instance)

	// 2. config.yaml(DSN + 表名)
	cfgData, err := os.ReadFile("./config.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read config:", err)
		os.Exit(1)
	}
	var appCfg appConfig
	if err := yaml.Unmarshal(cfgData, &appCfg); err != nil {
		fmt.Fprintln(os.Stderr, "parse config:", err)
		os.Exit(1)
	}
	if appCfg.Source.DSN == "" {
		fmt.Fprintln(os.Stderr, "config.source.dsn is empty")
		os.Exit(1)
	}
	lg.Info("config loaded", "version", appCfg.Source.Version, "dsn_host", hostOfDSN(appCfg.Source.DSN))

	// 3. mapping
	loader, err := fieldmapping.NewLoader(cfg.MappingDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load mappings:", err)
		os.Exit(1)
	}
	lg.Info("mappings loaded", "models", loader.Models())

	// 4. schemas
	schemaDir := cfg.ResolveModelsDir()
	lg.Info("loading schemas", "dir", schemaDir)
	schemas := map[string]*schemaMeta{}
	for _, name := range []string{"supplier", "product", "category", "sale_detail", "stock"} {
		schemaBytes, err := os.ReadFile(filepath.Join(schemaDir, name, "schema.yaml"))
		if err != nil {
			lg.Info("schema read failed", "model", name, "err", err.Error())
			continue
		}
		schema, err := cubeschema.Load(schemaBytes)
		if err != nil {
			lg.Info("schema parse failed", "model", name, "err", err.Error())
			continue
		}
		mapper, ok := loader.Get(name)
		if !ok {
			lg.Info("mapping missing", "model", name)
			continue
		}
		schemas[name] = &schemaMeta{Schema: schema, Columns: mapper.TargetsWithType()}
	}
	lg.Info("schemas loaded", "count", len(schemas), "models", keysOf(schemas))

	// 5. SQL Server
	conn, err := hbposv7.New(hbposv7.Options{
		DSN:           appCfg.Source.DSN,
		TableSupplier: appCfg.Source.TableSupplier,
		TableProduct:  appCfg.Source.TableProduct,
		TableCategory: appCfg.Source.TableCategory,
		TableSale:     appCfg.Source.TableSale,
		TableStock:    appCfg.Source.TableStock,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open SQL Server:", err)
		os.Exit(1)
	}
	defer conn.Close()
	lg.Info("SQL Server connected")

	// 6. DuckDB
	duckPath := cfg.ResolveDuckDBPath()
	if err := os.MkdirAll(filepath.Dir(duckPath), 0o755); err != nil {
		lg.Info("mkdir data dir failed", "err", err.Error())
	}
	db, err := duckdb.Open(duckPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open DuckDB:", err)
		fmt.Fprintln(os.Stderr, "hint: install libduckdb and set DUCKDB_LIBRARY_PATH")
		os.Exit(1)
	}
	defer db.Close()
	lg.Info("DuckDB opened", "path", duckPath)

	// 7. 拉数据 + 灌 DuckDB
	fetchers := []modelFetcher{
		{"supplier", conn.FetchSupplier, suppliermodel.Build},
		{"product", conn.FetchProduct, productmodel.Build},
		{"category", conn.FetchCategory, categorymodel.Build},
		{"sale_detail", conn.FetchSaleDetail, salemodel.Build},
		{"stock", conn.FetchStock, stockmodel.Build},
	}
	registeredModels := []string{}
	for _, f := range fetchers {
		raw, err := f.fetch(ctx)
		if err != nil {
			lg.Info("fetch failed", "model", f.name, "err", err.Error())
			continue
		}
		mapper, _ := loader.Get(f.name)
		if mapper == nil {
			lg.Info("mapping missing, skip DuckDB load", "model", f.name)
			continue
		}
		mapped := make([]map[string]any, 0, len(raw))
		for _, row := range raw {
			mr, err := mapper.Apply(row)
			if err != nil {
				continue
			}
			mapped = append(mapped, mr)
		}
		meta, ok := schemas[f.name]
		if !ok {
			lg.Info("schema missing, skip DuckDB load", "model", f.name)
			continue
		}
		rawTable := f.name + "_raw"
		if err := db.LoadFrom(rawTable, meta.Columns, toRows(mapped, meta.Columns)); err != nil {
			lg.Info("LoadFrom failed", "model", f.name, "err", err.Error())
			continue
		}
		if err := f.build(ctx, db, "SELECT * FROM "+rawTable); err != nil {
			lg.Info("preagg.Build failed", "model", f.name, "err", err.Error())
			continue
		}
		lg.Info("loaded to DuckDB", "model", f.name, "rows", len(mapped))
		registeredModels = append(registeredModels, f.name)
	}

	// 8. 注册到 gateway
	go registerToGateway(cfg, registeredModels, lg)
	go watchShutdown(cfg, lg)

	// 9. HTTP server
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())
	engine.POST("/query", queryHandler(db, schemas, registeredModels, cfg.AppID, lg))
	engine.GET("/healthz", cfg.HealthHandler(registeredModels))

	lg.Info("starting HTTP server", "addr", cfg.Port, "models", registeredModels)
	if err := engine.Run(cfg.Port); err != nil {
		lg.Info("exit", "err", err.Error())
	}
}

// queryHandler MVP 实现:单 measure + 单 dimension。
//
// 与 cube/semantic-layers/sixun/cmd/sixun-ysx/main.go::queryHandler 对齐 wire 形状:
//   - 响应顶层 measures (array of bare ref) + dimensions (array of bare ref),
//     不再用单数 measure / dim(plan B 阶段家族内所有 cube app 应输出相同结构)
//   - SQL 列别名用 "<model>.<ref>" 扁平命名(如 "supplier.count"),
//     DuckDB 原样回传 → wire 端 data[0] 键就是扁平别名
//   - 不再 emit app_id 字段(plan B 解耦后,wire 端用 source 单字段即可)
//
// 4xx 响应带子码 —— gateway 据此映射到 apierror.Code(MODEL_NOT_FOUND_IN_SOURCE /
// VERSION_UNSUPPORTED / QUERY_PARSE_ERROR / QUERY_INVALID / UPSTREAM_ERROR)。
//
// MVP 仍只取 q.Measures[0] / q.Dimensions[0];展开为 for-range 数组的演进见 ysx 的
// measCols/dimCols 模式 — 留作后续 cube app 端 queryHandler 进一步重构。
func queryHandler(db *duckdb.Engine, schemas map[string]*schemaMeta, supportedModels []string, source string, lg *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := c.GetRawData()
		if err != nil {
			writeErr(c, http.StatusBadRequest, subCodeQueryParseError,
				"read body: "+err.Error(), nil)
			return
		}

		q, err := cubequery.Parse(body)
		if err != nil {
			writeErr(c, http.StatusBadRequest, subCodeQueryParseError,
				"parse cube query: "+err.Error(), nil)
			return
		}
		modelName := q.Model()
		if modelName == "" {
			writeErr(c, http.StatusBadRequest, subCodeQueryInvalid,
				"cannot infer model", nil)
			return
		}
		meta, ok := schemas[modelName]
		if !ok || meta == nil {
			writeErr(c, http.StatusNotFound, subCodeModelNotFound,
				"model not exposed by source: "+modelName,
				map[string]any{"source": source, "model": modelName, "supported": supportedModels})
			return
		}
		if len(q.Measures) == 0 {
			writeErr(c, http.StatusBadRequest, subCodeQueryInvalid,
				"at least one measure required", nil)
			return
		}
		measureRef := strings.TrimPrefix(q.Measures[0], modelName+".")
		ms, ok := meta.Schema.FindMeasure(measureRef)
		if !ok {
			writeErr(c, http.StatusNotFound, subCodeModelNotFound,
				"measure not found: "+measureRef,
				map[string]any{"source": source, "model": modelName, "measure": measureRef})
			return
		}

		// dimRef 提到外层,供 SQL 拼装使用(<model>.<dimRef> 扁平别名)。
		var dimRef string
		var dimSQL string
		if len(q.Dimensions) > 0 {
			dimRef = strings.TrimPrefix(q.Dimensions[0], modelName+".")
			d, ok := meta.Schema.FindDimension(dimRef)
			if !ok {
				writeErr(c, http.StatusNotFound, subCodeModelNotFound,
					"dimension not found: "+dimRef,
					map[string]any{"source": source, "model": modelName, "dimension": dimRef})
				return
			}
			dimSQL = d.SQL
		}

		// SQL 列别名扁平命名,与 ysx 对齐。
		// DuckDB 原样回传双引号包裹的别名,所以 data[0] 键就是 "<model>.<ref>"。
		fullMeasure := modelName + "." + measureRef
		var sql string
		var dimRefs []string
		if dimSQL != "" {
			fullDim := modelName + "." + dimRef
			sql = fmt.Sprintf(
				"SELECT %s AS %q, %s AS %q FROM %s GROUP BY %s LIMIT 1000",
				dimSQL, fullDim, ms.SQL, fullMeasure, meta.Schema.SQLTable, dimSQL)
			dimRefs = []string{dimRef}
		} else {
			sql = fmt.Sprintf(
				"SELECT %s AS %q FROM %s LIMIT 1000",
				ms.SQL, fullMeasure, meta.Schema.SQLTable)
		}

		rows, err := db.QueryMap(sql)
		if err != nil {
			lg.Info("duckdb query failed", "sql", sql, "err", err.Error())
			writeErr(c, http.StatusInternalServerError, subCodeInternalError,
				"duckdb: "+err.Error(), map[string]any{"sql": sql})
			return
		}

		lg.Info("query ok", "model", modelName, "sql", sql, "rows", len(rows))
		c.JSON(http.StatusOK, gin.H{
			"source":     source,
			"model":      modelName,
			"sql":        sql,
			"data":       rows,
			"measures":   []string{measureRef}, // bare ref 数组,与 ysx 对齐
			"dimensions": dimRefs,               // 空数组 或 [bare dim ref]
		})
	}
}

func toRows(mapped []map[string]any, cols []fieldmapping.FieldDef) [][]any {
	out := make([][]any, 0, len(mapped))
	for _, row := range mapped {
		one := make([]any, 0, len(cols))
		for _, c := range cols {
			one = append(one, row[c.Target])
		}
		out = append(out, one)
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// registerToGateway 用 cfg.RegisterBody 构造注册体。
func registerToGateway(cfg *boot.Config, models []string, lg *log.Logger) {
	body := cfg.RegisterBody(models)
	deadline := time.Now().Add(10 * time.Second)

	for {
		resp, err := http.Post(cfg.GatewayURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				lg.Info("registered to cube-gateway", "url", cfg.GatewayURL)
			} else {
				lg.Info("register returned non-204", "status", resp.Status, "url", cfg.GatewayURL)
			}
			return
		}
		if time.Now().After(deadline) {
			lg.Info("register attempt failed (giving up)", "err", err.Error(), "url", cfg.GatewayURL)
			return
		}
		lg.Info("register attempt failed, retrying", "err", err.Error(), "url", cfg.GatewayURL)
		time.Sleep(2 * time.Second)
	}
}

// watchShutdown 监听 SIGTERM / SIGINT,触发时调用 unregisterFromGateway 后退出。
//
// 设计:见 cmd/sixun-ysx/main.go 的同函数注释(共用同一份语义)。
func watchShutdown(cfg *boot.Config, lg *log.Logger) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	lg.Info("shutdown signal received", "sig", sig.String())
	unregisterFromGateway(cfg, lg)
	os.Exit(0)
}

// unregisterFromGateway 调 cube-gateway /unregister,2s deadline,失败仅记日志。
func unregisterFromGateway(cfg *boot.Config, lg *log.Logger) {
	body := cfg.UnregisterBody()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.GatewayURL+"/unregister", bytes.NewReader(body))
	if err != nil {
		lg.Info("unregister: build request failed", "err", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		lg.Info("unregister failed (best-effort)", "err", err.Error(), "url", cfg.GatewayURL)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		lg.Info("unregistered from cube-gateway", "url", cfg.GatewayURL)
		return
	}
	lg.Info("unregister returned non-204", "status", resp.Status, "url", cfg.GatewayURL)
}

func hostOfDSN(dsn string) string {
	at := -1
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return "(unknown)"
	}
	rest := dsn[at+1:]
	q := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '?' || rest[i] == '/' {
			q = i
			break
		}
	}
	if q < 0 {
		return rest
	}
	return rest[:q]
}

// 防止 json 包 unused 警告(boot.RegisterBody 内部已经用)。
var _ = json.Marshal
