// Command sixun-hbposv7 是思迅 7pro 数据源 dapr cube app 实例。
//
// 启动流程(第三轮·DuckDB 接通):
//  1. 加载 ./config.yaml(DSN + 5 张表名 + DuckDB 路径)
//  2. 加载 ./mapping-*.yaml(5 个 model 的字段映射)
//  3. 加载 ../../sixun-models/<model>/schema.yaml(5 个 cube schema)
//  4. 打开 SQL Server(hbposv7.Connector)
//  5. 打开本地 DuckDB(go-pduckdb,纯 Go,无需 gcc)
//  6. 对 5 个 model:Fetch 原始数据 → mapping → LoadFrom → preagg.Build
//  7. 调 cube-gateway /register 上报 metadata
//  8. 启 HTTP server(/query /health),由 gateway 通过 dapr invocation 调用
//
// /query 支持简化的 cube query(gin-cached):
//   - 单 measure + 单 dimension(无 filter / timeDim / join,后续 P2)
//   - SELECT <dim_sql>, <measure_sql> FROM <sql_table> GROUP BY <dim_sql>
//   - LIMIT 1000
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/hbposv7"

	categorymodel "github.com/YunBright/cube/sixun-models/category"
	productmodel "github.com/YunBright/cube/sixun-models/product"
	salemodel "github.com/YunBright/cube/sixun-models/sale_detail"
	stockmodel "github.com/YunBright/cube/sixun-models/stock"
	suppliermodel "github.com/YunBright/cube/sixun-models/supplier"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

const (
	appID   = "sixun-hbposv7"
	family  = "sixun"
	version = "hbposv7"
)

// appConfig 是本实例关心的 config.yaml 子集。
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
	Server struct {
		HTTPAddr string `yaml:"http_addr"`
	} `yaml:"server"`
	Storage struct {
		DuckDBPath string `yaml:"duckdb_path"`
	} `yaml:"storage"`
}

// schemaMeta 把 schema 和它的 target 列绑在一起(LoadFrom 用)。
type schemaMeta struct {
	Schema  *cubeschema.Model
	Columns []fieldmapping.FieldDef // 从 mapper.TargetsWithType() 来的 target + type
}

// modelFetcher 是每个 model 的:fetcher + 预聚合 build 函数。
type modelFetcher struct {
	name  string
	fetch func(context.Context) ([]map[string]any, error)
	build func(ctx context.Context, db *duckdb.Engine, rawSQL string) error
}

func main() {
	ctx := context.Background()
	lg := log.New(appID).WithComponent("main")

	// 1. 加载 config.yaml
	cfgData, err := os.ReadFile("./config.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read config:", err)
		os.Exit(1)
	}
	var cfg appConfig
	if err := yaml.Unmarshal(cfgData, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "parse config:", err)
		os.Exit(1)
	}
	if cfg.Source.DSN == "" {
		fmt.Fprintln(os.Stderr, "config.source.dsn is empty")
		os.Exit(1)
	}
	lg.Info("config loaded", "version", cfg.Source.Version, "dsn_host", hostOfDSN(cfg.Source.DSN))

	// 2. 加载 mapping/*.yaml
	loader, err := fieldmapping.NewLoader("./mapping")
	if err != nil {
		fmt.Fprintln(os.Stderr, "load mappings:", err)
		os.Exit(1)
	}
	lg.Info("mappings loaded", "models", loader.Models())

	// 3. 加载 5 个 cube schema
	// 路径解析优先级:
	//   1. 环境变量 CUBE_MODELS_DIR(推荐)
	//   2. 相对 cwd: ../../../../sixun-models(从 cmd/<instance>/ 启动)
	//   3. 当前工作目录下的 ./sixun-models(flat 部署)
	schemaDir := os.Getenv("CUBE_MODELS_DIR")
	if schemaDir == "" {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "..", "sixun-models")); err == nil {
			schemaDir = filepath.Join("..", "..", "..", "..", "sixun-models")
		} else if _, err := os.Stat("./sixun-models"); err == nil {
			schemaDir = "./sixun-models"
		} else {
			schemaDir = filepath.Join("..", "..", "..", "..", "sixun-models") // 默认值,启动后会报错
		}
	}
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

	// 4. 打开 SQL Server
	conn, err := hbposv7.New(hbposv7.Options{
		DSN:           cfg.Source.DSN,
		TableSupplier: cfg.Source.TableSupplier,
		TableProduct:  cfg.Source.TableProduct,
		TableCategory: cfg.Source.TableCategory,
		TableSale:     cfg.Source.TableSale,
		TableStock:    cfg.Source.TableStock,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open SQL Server:", err)
		os.Exit(1)
	}
	defer conn.Close()
	lg.Info("SQL Server connected")

	// 5. 打开 DuckDB
	duckPath := cfg.Storage.DuckDBPath
	if duckPath == "" {
		duckPath = "./data/sixun-hbposv7.duckdb"
	}
	if err := os.MkdirAll(filepath.Dir(duckPath), 0o755); err != nil {
		lg.Info("mkdir data dir failed", "err", err.Error())
	}
	db, err := duckdb.Open(duckPath)
	if err != nil {
		// DuckDB 启动失败:libduckdb 没装
		fmt.Fprintln(os.Stderr, "open DuckDB:", err)
		fmt.Fprintln(os.Stderr, "hint: install libduckdb and set DUCKDB_LIBRARY_PATH")
		os.Exit(1)
	}
	defer db.Close()
	lg.Info("DuckDB opened", "path", duckPath)

	// 6. 拉数据 + 灌 DuckDB
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
		// 6a. 灌临时表 <model>_raw
		rawTable := f.name + "_raw"
		if err := db.LoadFrom(rawTable, meta.Columns, toRows(mapped, meta.Columns)); err != nil {
			lg.Info("LoadFrom failed", "model", f.name, "err", err.Error())
			continue
		}
		// 6b. 调 sixun-models Build,生成 <model> 预聚合表
		if err := f.build(ctx, db, "SELECT * FROM "+rawTable); err != nil {
			lg.Info("preagg.Build failed", "model", f.name, "err", err.Error())
			continue
		}
		lg.Info("loaded to DuckDB", "model", f.name, "rows", len(mapped))
		registeredModels = append(registeredModels, f.name)
	}

	// 7. 注册到 cube-gateway
	go registerToGateway(lg, registeredModels)

	// 8. HTTP server(gin)
	addr := cfg.Server.HTTPAddr
	if addr == "" {
		addr = ":8082"
	}
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())
	engine.POST("/query", queryHandler(db, schemas, registeredModels, lg))
	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
			"app_id": appID,
			"models": registeredModels,
		})
	})

	lg.Info("starting HTTP server", "addr", addr, "models", registeredModels)
	if err := engine.Run(addr); err != nil {
		lg.Info("exit", "err", err.Error())
	}
}

// queryHandler 处理 cube-gateway 转发的 /query 请求,真查 DuckDB。
//
// MVP SQL 生成(简化):
//   - 取 measures[0] 和 dimensions[0]
//   - SELECT <dim_sql>, <measure_sql> FROM <sql_table> GROUP BY <dim_sql> LIMIT 1000
//   - 不支持 filter / timeDim / join / 多 measure(P2)
func queryHandler(db *duckdb.Engine, schemas map[string]*schemaMeta, supportedModels []string, lg *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			return
		}

		q, err := cubequery.Parse(body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "parse cube query: " + err.Error()})
			return
		}
		modelName := q.Model()
		if modelName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot infer model"})
			return
		}
		meta, ok := schemas[modelName]
		if !ok || meta == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "model not registered: " + modelName})
			return
		}
		if len(q.Measures) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "at least one measure required"})
			return
		}
		// 取第一个 measure(简化)
		measureRef := strings.TrimPrefix(q.Measures[0], modelName+".")
		ms, ok := meta.Schema.FindMeasure(measureRef)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "measure not found: " + measureRef})
			return
		}
		var dimSQL string
		if len(q.Dimensions) > 0 {
			dimRef := strings.TrimPrefix(q.Dimensions[0], modelName+".")
			d, ok := meta.Schema.FindDimension(dimRef)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "dimension not found: " + dimRef})
				return
			}
			dimSQL = d.SQL
		}

		// 生成 SQL
		var sql string
		if dimSQL != "" {
			sql = fmt.Sprintf(
				"SELECT %s AS dimension, %s AS measure FROM %s GROUP BY %s LIMIT 1000",
				dimSQL, ms.SQL, meta.Schema.SQLTable, dimSQL)
		} else {
			sql = fmt.Sprintf("SELECT %s AS measure FROM %s LIMIT 1000", ms.SQL, meta.Schema.SQLTable)
		}

		// 真查 DuckDB
		rows, err := db.QueryMap(sql)
		if err != nil {
			lg.Info("duckdb query failed", "sql", sql, "err", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "duckdb: " + err.Error()})
			return
		}

		lg.Info("query ok", "model", modelName, "sql", sql, "rows", len(rows))
		c.JSON(http.StatusOK, gin.H{
			"app_id":  appID,
			"model":   modelName,
			"sql":     sql,
			"data":    rows,
			"measure": measureRef,
			"dim":     dimSQL,
		})
	}
}

// toRows 把 []map[string]any 转为 [][]any,按 columns 顺序。
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

// keysOf 返回 map 的所有 key(用于日志)。
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// registerToGateway P0-1 注册协议。
func registerToGateway(lg *log.Logger, models []string) {
	gatewayURL := os.Getenv("CUBE_GATEWAY_URL")
	if gatewayURL == "" {
		gatewayURL = "http://localhost:8080"
	}
	body := []byte(fmt.Sprintf(`{
		"app_id": "%s",
		"family": "%s",
		"version": "%s",
		"models": %s,
		"capabilities": ["query", "preagg", "cache_l2"],
		"health_url": "/health"
	}`, appID, family, version, jsonArray(models)))

	resp, err := http.Post(gatewayURL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		lg.Info("register attempt failed", "err", err.Error(), "url", gatewayURL)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		lg.Info("registered to cube-gateway", "url", gatewayURL)
	} else {
		lg.Info("register returned non-204", "status", resp.Status, "url", gatewayURL)
	}
}

// hostOfDSN 从 DSN 抠 host(日志用)。
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

func jsonArray(ss []string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}
