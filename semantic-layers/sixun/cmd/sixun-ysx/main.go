// Command sixun-ysx 是思迅云商x 数据源 dapr cube app 实例。
//
// 与 sixun-hbposv7 同结构,差别:
//   - 数据源 connector:ysx.Connector(云商x)
//   - mapping.yaml:云商x 原始字段名
//   - 独立 .duckdb 文件(P0-3)
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

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/ysx"

	categorymodel "github.com/YunBright/cube/sixun-models/category"
	productmodel "github.com/YunBright/cube/sixun-models/product"
	salemodel "github.com/YunBright/cube/sixun-models/sale_detail"
	stockmodel "github.com/YunBright/cube/sixun-models/stock"
	suppliermodel "github.com/YunBright/cube/sixun-models/supplier"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

const (
	appID   = "sixun-ysx"
	family  = "sixun"
	version = "ysx"
)

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

type schemaMeta struct {
	Schema  *cubeschema.Model
	Columns []fieldmapping.FieldDef
}

type modelFetcher struct {
	name  string
	fetch func(context.Context) ([]map[string]any, error)
	build func(ctx context.Context, db *duckdb.Engine, rawSQL string) error
}

func main() {
	ctx := context.Background()
	lg := log.New(appID).WithComponent("main")

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

	loader, err := fieldmapping.NewLoader("./mapping")
	if err != nil {
		fmt.Fprintln(os.Stderr, "load mappings:", err)
		os.Exit(1)
	}
	lg.Info("mappings loaded", "models", loader.Models())

	schemaDir := os.Getenv("CUBE_MODELS_DIR")
	if schemaDir == "" {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "..", "sixun-models")); err == nil {
			schemaDir = filepath.Join("..", "..", "..", "..", "sixun-models")
		} else if _, err := os.Stat("./sixun-models"); err == nil {
			schemaDir = "./sixun-models"
		} else {
			schemaDir = filepath.Join("..", "..", "..", "..", "sixun-models")
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

	conn, err := ysx.New(ysx.Options{
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

	duckPath := cfg.Storage.DuckDBPath
	if duckPath == "" {
		duckPath = "./data/sixun-ysx.duckdb"
	}
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

	go registerToGateway(lg, registeredModels)

	// gin engine
	addr := cfg.Server.HTTPAddr
	if addr == "" {
		addr = ":8083"
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

		var sql string
		if dimSQL != "" {
			sql = fmt.Sprintf(
				"SELECT %s AS dimension, %s AS measure FROM %s GROUP BY %s LIMIT 1000",
				dimSQL, ms.SQL, meta.Schema.SQLTable, dimSQL)
		} else {
			sql = fmt.Sprintf("SELECT %s AS measure FROM %s LIMIT 1000", ms.SQL, meta.Schema.SQLTable)
		}

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
