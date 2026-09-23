// Command sixun-ysx 是思迅云商 x 数据源 dapr cube app 实例。
//
// 与 sixun-hbposv7 同结构,差别:
//   - 数据源 connector:ysx.Connector(云商x)
//   - mapping.yaml:云商x 原始字段名
//   - 独立 .duckdb 文件(按 CUBE_APP_ID 区分)
//
// v2 改动:实例由 CUBE_APP_ID 驱动(本文件其余流程与 sixun-hbposv7 同步)。
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
	"time"

	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/boot"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/ysx"

	categorymodel "github.com/YunBright/cube/sixun-models/category"
	productmodel "github.com/YunBright/cube/sixun-models/product"
	salemodel "github.com/YunBright/cube/sixun-models/sale_detail"
	stockmodel "github.com/YunBright/cube/sixun-models/stock"
	suppliermodel "github.com/YunBright/cube/sixun-models/supplier"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// appConfig 只保留 DSN / 表名(server.http_addr 与 storage.duckdb_path 改 env)。
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

// /query 4xx 子码常量 —— gateway 据此映射到 apierror.Code。
const (
	subCodeModelNotFound     = "MODEL_NOT_FOUND"
	subCodeVersionUnsupported = "VERSION_UNSUPPORTED"
	subCodeQueryParseError   = "QUERY_PARSE_ERROR"
	subCodeQueryInvalid      = "QUERY_INVALID"
	subCodeInternalError     = "INTERNAL_ERROR"
)

type errBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeErr(c *gin.Context, status int, code, msg string, details map[string]any) {
	c.JSON(status, errBody{Code: code, Message: msg, Details: details})
}

func main() {
	ctx := context.Background()

	cfg, err := boot.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lg := log.New(cfg.AppID).WithComponent("main")
	lg.Info("boot loaded", "family", cfg.Family, "version", cfg.Version, "instance", cfg.Instance)

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

	loader, err := fieldmapping.NewLoader(cfg.MappingDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load mappings:", err)
		os.Exit(1)
	}
	lg.Info("mappings loaded", "models", loader.Models())

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

	conn, err := ysx.New(ysx.Options{
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

	go registerToGateway(cfg, registeredModels, lg)

	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())
	engine.POST("/query", queryHandler(db, schemas, registeredModels, cfg.AppID, lg))
	engine.GET("/healthz", cfg.HealthHandler(registeredModels))

	lg.Info("starting HTTP server", "addr", cfg.Port, "models", registeredModels)
	if err := engine.Run(cfg.Port); err != nil {
		lg.Info("exit", "err", err.Error())
	}
}

// queryHandler 完整版(支持多 measure / dimension / filter)。
//
// 4xx 响应一律带 "code" 子码,供 gateway 映射到 apierror.Code。
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

		// dimensions
		var dimCols []string
		var dimGroup []string
		var dimRefs []string
		for _, rawDim := range q.Dimensions {
			dimRef := strings.TrimPrefix(rawDim, modelName+".")
			d, ok := meta.Schema.FindDimension(dimRef)
			if !ok {
				writeErr(c, http.StatusNotFound, subCodeModelNotFound,
					"dimension not found: "+dimRef,
					map[string]any{"source": source, "model": modelName, "dimension": dimRef})
				return
			}
			alias := modelName + "." + dimRef
			expr := d.SQL
			if isSimpleColumn(expr) {
				expr = "RTRIM(" + expr + ")"
			}
			dimCols = append(dimCols, fmt.Sprintf("%s AS %q", expr, alias))
			dimGroup = append(dimGroup, d.SQL)
			dimRefs = append(dimRefs, dimRef)
		}

		// measures
		var measCols []string
		var measRefs []string
		for _, rawMs := range q.Measures {
			measRef := strings.TrimPrefix(rawMs, modelName+".")
			ms, ok := meta.Schema.FindMeasure(measRef)
			if !ok {
				writeErr(c, http.StatusNotFound, subCodeModelNotFound,
					"measure not found: "+measRef,
					map[string]any{"source": source, "model": modelName, "measure": measRef})
				return
			}
			alias := modelName + "." + measRef
			measCols = append(measCols, fmt.Sprintf("%s AS %q", ms.SQL, alias))
			measRefs = append(measRefs, measRef)
		}

		// filters
		var whereParts []string
		var whereArgs []any
		for _, f := range q.Filters {
			memberRef := strings.TrimPrefix(f.Member, modelName+".")
			dim, ok := meta.Schema.FindDimension(memberRef)
			if !ok {
				writeErr(c, http.StatusBadRequest, subCodeQueryInvalid,
					"filter dimension not found: "+memberRef,
					map[string]any{"source": source, "model": modelName, "filter_member": memberRef})
				return
			}
			expr, args, err := buildFilterExpr(dim.SQL, f.Operator, f.Values)
			if err != nil {
				writeErr(c, http.StatusBadRequest, subCodeQueryInvalid,
					"filter: "+err.Error(),
					map[string]any{"source": source, "filter_member": memberRef, "operator": f.Operator})
				return
			}
			whereParts = append(whereParts, expr)
			whereArgs = append(whereArgs, args...)
		}

		// SQL 拼装
		var selectList []string
		selectList = append(selectList, dimCols...)
		selectList = append(selectList, measCols...)
		sql := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectList, ", "), meta.Schema.SQLTable)
		if len(whereParts) > 0 {
			sql += " WHERE " + strings.Join(whereParts, " AND ")
		}
		if len(dimGroup) > 0 {
			sql += " GROUP BY " + strings.Join(dimGroup, ", ")
		}
		limit := 1000
		if q.Limit != nil && *q.Limit > 0 {
			limit = *q.Limit
		}
		sql += fmt.Sprintf(" LIMIT %d", limit)

		rows, err := db.QueryMap(sql, whereArgs...)
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
			"measures":   measRefs,
			"dimensions": dimRefs,
		})
	}
}

// buildFilterExpr 把 cube.js filter operator 翻译成 SQL WHERE 片段。
func buildFilterExpr(columnSQL, op string, values []any) (string, []any, error) {
	trimmed := isSimpleColumn(columnSQL)
	col := columnSQL
	if trimmed {
		col = "RTRIM(" + col + ")"
	}
	switch op {
	case "equals":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("equals needs 1 value, got %d", len(values))
		}
		return col + " = ?", values, nil
	case "notEquals":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("notEquals needs 1 value, got %d", len(values))
		}
		return col + " != ?", values, nil
	case "contains":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("contains needs 1 value, got %d", len(values))
		}
		return col + " LIKE ?", []any{"%" + fmt.Sprint(values[0]) + "%"}, nil
	case "notContains":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("notContains needs 1 value, got %d", len(values))
		}
		return col + " NOT LIKE ?", []any{"%" + fmt.Sprint(values[0]) + "%"}, nil
	case "startsWith":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("startsWith needs 1 value, got %d", len(values))
		}
		return col + " LIKE ?", []any{fmt.Sprint(values[0]) + "%"}, nil
	case "in":
		if len(values) == 0 {
			return "", nil, fmt.Errorf("in needs >=1 value")
		}
		placeholders := strings.Repeat("?,", len(values))
		placeholders = strings.TrimRight(placeholders, ",")
		return col + " IN (" + placeholders + ")", values, nil
	case "notIn":
		if len(values) == 0 {
			return "", nil, fmt.Errorf("notIn needs >=1 value")
		}
		placeholders := strings.Repeat("?,", len(values))
		placeholders = strings.TrimRight(placeholders, ",")
		return col + " NOT IN (" + placeholders + ")", values, nil
	case "gt":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("gt needs 1 value, got %d", len(values))
		}
		return col + " > ?", values, nil
	case "gte":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("gte needs 1 value, got %d", len(values))
		}
		return col + " >= ?", values, nil
	case "lt":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("lt needs 1 value, got %d", len(values))
		}
		return col + " < ?", values, nil
	case "lte":
		if len(values) != 1 {
			return "", nil, fmt.Errorf("lte needs 1 value, got %d", len(values))
		}
		return col + " <= ?", values, nil
	default:
		return "", nil, fmt.Errorf("unsupported operator: %s", op)
	}
}

// isSimpleColumn 判断 expr 是不是简单列名(可安全包 RTRIM)。
func isSimpleColumn(expr string) bool {
	if expr == "" {
		return false
	}
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
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

// 防止 json 包 unused 警告。
var _ = json.Marshal
