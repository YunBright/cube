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
//  2. ./config.yaml(DSN + 源表名:4 张 duck 表 + 6 张 live 表)
//  3. 内嵌 mapping-*.yaml(go:embed,不再读 ./mapping/ 目录)
//  4. 内嵌 sixun-models schema(go:embed,不再读磁盘目录)
//  5. hbposv7.Connector 打开 SQL Server
//  6. cfg.ResolveDuckDBPath() 打开 DuckDB
//  7. storage: duck 的 4 个 model:supplier / product / category / sale_detail
//     → Fetch → mapping → LoadFrom → preagg.Build
//     storage: live 的 6 个 model(settlement / settlement_line / purchase_sheet /
//     purchase_sheet_line / sale_day / stock)**不拉任何数据**,查询时透传源库。
//  8. goroutine: cfg.RegisterBody → POST /register
//  9. gin engine:/query /healthz
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/boot"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/dataloader"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/freshness"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/meta"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/refreshschedule"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/hbposv7"

	models "github.com/YunBright/cube/sixun-models"
	categorymodel "github.com/YunBright/cube/sixun-models/category"
	productmodel "github.com/YunBright/cube/sixun-models/product"
	salemodel "github.com/YunBright/cube/sixun-models/sale_detail"
	suppliermodel "github.com/YunBright/cube/sixun-models/supplier"

	"github.com/gin-gonic/gin"
)

// mappingAssets 是本 family 的字段映射资产。
//
// 好店 posv7 的源字段名和云商x 不同(hbposv7.Goods_amt ↔ ysx.sheet_amt),
// 所以 mapping 按 binary 各带一份。嵌入后部署只推一个二进制。
//
//go:embed mapping/*.yaml
var mappingAssets embed.FS

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
		// 结算审批场景(2026-10-09)。表名在两个 family 同名同表,
		// 单据类型不同:hbposv7 的结款单是 DP(供应商对帐单)、ysx 是 CP。
		TableSettlement      string `yaml:"table_settlement"`
		TableSettlementLine  string `yaml:"table_settlement_line"`
		TablePurchaseSheet   string `yaml:"table_purchase_sheet"`
		TablePurchaseSheetLn string `yaml:"table_purchase_sheet_line"`
		TableSaleDay         string `yaml:"table_sale_day"`
		// RowLimit 所有角色共用的拉取行数上限;<=0 回落到 source.DefaultRowLimit。
		RowLimit int `yaml:"row_limit"`
		// RowLimits 按角色覆盖 RowLimit,键取 source.Role*(supplier/product/category/sale/stock)。
		RowLimits map[string]int `yaml:"row_limits"`
	} `yaml:"source"`
}

// schemaMeta 是 dataloader.Meta 的本地别名 —— 真正的定义在共用包里,
// 两个 family 用同一个类型,加载与重拉逻辑才不可能漂移。
type schemaMeta = dataloader.Meta

// 子码常量(emit 到 4xx JSON 的 "code" 字段,供 gateway 映射到 apierror.Code)。
const (
	subCodeModelNotFound      = "MODEL_NOT_FOUND"
	subCodeVersionUnsupported = "VERSION_UNSUPPORTED"
	subCodeQueryParseError    = "QUERY_PARSE_ERROR"
	subCodeQueryInvalid       = "QUERY_INVALID"
	subCodeInternalError      = "INTERNAL_ERROR"
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

	// 2. 实例配置(config.local.yaml 优先,其次 config.yaml —— 见 internal/appcfg)
	appCfg, usedCfg, err := appcfg.Load[appConfig](".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if appCfg.Source.DSN == "" {
		fmt.Fprintln(os.Stderr, "config.source.dsn is empty")
		os.Exit(1)
	}
	lg.Info("config loaded", "file", usedCfg, "version", appCfg.Source.Version,
		"dsn_host", hostOfDSN(appCfg.Source.DSN))

	// live model 的源表名按 model 名挂起来(定义见文件末尾 liveTables)。
	live := liveTables(&appCfg)

	// 3. mapping:编译进二进制,不再依赖 ./mapping/ 目录
	mappingFS, err := fs.Sub(mappingAssets, "mapping")
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve embedded mappings:", err)
		os.Exit(1)
	}
	loader, err := fieldmapping.NewLoaderFS(mappingFS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load mappings:", err)
		os.Exit(1)
	}
	lg.Info("mappings loaded", "models", loader.Models())

	// 4. schemas:family 级共享,同样编译进二进制(sixun-models 模块)
	modelNames, err := models.ModelNames()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list embedded schemas:", err)
		os.Exit(1)
	}
	lg.Info("loading schemas", "origin", "embedded", "models", modelNames)
	schemas := map[string]*schemaMeta{}
	for _, name := range modelNames {
		schemaBytes, err := models.ReadSchema(name)
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
		sm := &schemaMeta{Schema: schema, Columns: mapper.TargetsWithType(), Mapper: mapper}
		if schema.EffectiveStorage() == cubeschema.StorageLive {
			src := live[name]
			if src == "" {
				// 漏配表名必须显式失败:否则该 model 会以 MODEL_NOT_FOUND 静默消失。
				lg.Info("live model 缺 source.table_*,该 model 不可查询", "model", name)
				continue
			}
			sm.Live = true
			sm.SourceTable = src
		}
		schemas[name] = sm
	}
	lg.Info("schemas loaded", "count", len(schemas), "models", keysOf(schemas))

	// 5. SQL Server
	conn, err := hbposv7.New(hbposv7.Options{
		DSN:           appCfg.Source.DSN,
		TableSupplier: appCfg.Source.TableSupplier,
		TableProduct:  appCfg.Source.TableProduct,
		TableCategory: appCfg.Source.TableCategory,
		TableSale:     appCfg.Source.TableSale,
		RowLimits:     source.NewRowLimits(appCfg.Source.RowLimit, appCfg.Source.RowLimits),
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

	// 只有 storage: duck 的 model 才物化进 DuckDB。
	// live 的(settlement / settlement_line / purchase_sheet / purchase_sheet_line /
	// sale_day / stock)走 queryHandler 实时透传,**不做任何原始数据同步**。
	specs := []dataloader.Spec{
		{Name: "supplier", Fetch: conn.FetchSupplier, Build: suppliermodel.Build},
		{Name: "product", Fetch: conn.FetchProduct, Build: productmodel.Build},
		{Name: "category", Fetch: conn.FetchCategory, Build: categorymodel.Build},
		{Name: "sale_detail", Fetch: conn.FetchSaleDetail, Build: salemodel.Build},
	}
	// fresh 记录每次物化的成败,喂给 /healthz。
	// duck model 是快照,不是实时透传 —— 所以"这份数据有多旧"是必须能被外部看见的事实。
	fresh := freshness.New(cfg.RefreshInterval())
	reload := func() []string {
		return dataloader.Load(ctx, db, schemas, specs, lg, fresh)
	}
	registeredModels := reload()

	// live model 不拉数据,但必须注册:它们是可查询的 model。
	for name, m := range schemas {
		if m.Live {
			lg.Info("live model, 查询时透传源库", "model", name, "source_table", m.SourceTable)
			registeredModels = append(registeredModels, name)
		}
	}
	sort.Strings(registeredModels)

	// 定时刷新:dapr Scheduler 优先,登记失败自动回退进程内 ticker。
	// 两种模式同时可用 —— 在途守卫保证不会重叠写 DuckDB。
	sched := refreshschedule.Setup(ctx, cfg.SidecarHTTPURL, cfg.RefreshInterval(), reload, lg)
	sched.Start(ctx, cfg.RefreshInterval(), lg)
	lg.Info("refresh driver ready", "mode", string(sched.Mode()))

	// 8. 注册到 gateway
	go registerToGateway(cfg, registeredModels, lg)
	go watchShutdown(cfg, lg)

	// 9. HTTP server
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())
	engine.POST("/query", queryHandler(db, conn, schemas, registeredModels, cfg.AppID, lg))
	engine.GET("/healthz", cfg.HealthHandler(registeredModels, fresh, sched.StatsAny))
	// dapr Scheduler 在到点时 POST /job/<name> 触发刷新。
	// **两种模式下都要注册**:job 记录存活于 Scheduler 的 etcd,跨重启仍在;
	// 若这次启动登记失败,旧 job 仍会来触发,没有这个端点会持续 404。
	engine.POST("/job/:jobName", sched.JobHandler())
	engine.GET("/meta", metaHandler(schemas, cfg.AppID, lg))

	lg.Info("starting HTTP server", "addr", cfg.Port, "models", registeredModels)
	if err := engine.Run(cfg.Port); err != nil {
		lg.Info("exit", "err", err.Error())
	}
}

// queryHandler 与 sixun-ysx 共用 cubequery.Build(见 pkg/cubequery/build.go)。
//
// ⚠️ 2026-10-09 修复:此前这里是**另一套残缺实现** —— 只取 Measures[0] /
// Dimensions[0]、**完全忽略 Filters**、LIMIT 硬编码 1000、没有 RTRIM。
//
// 后果不是"功能少"而是**返回错误的数据**:带 filter 的查询被静默丢弃后,
// SQL 退化成"取前 1000 行",而 supertrade 的 GetProduct 直接取 data[0]
// → 扫一个条码可能拿到**另一个商品**的库存,200 OK、零报错。
// 另外本 family 的 t_bd_item_info.id 是 char 定长补空格("6922303199721       "),
// 缺 RTRIM 让该门店的条码搜索永远匹配不上。
//
// 两个 family 共用 sixun-models 下的同一份 schema,查询语义必须一致,
// 所以这段逻辑现在只在共用包里存在一份。
//
// 4xx 响应带子码 —— gateway 据此映射到 apierror.Code(MODEL_NOT_FOUND_IN_SOURCE /
// VERSION_UNSUPPORTED / QUERY_PARSE_ERROR / QUERY_INVALID / UPSTREAM_ERROR)。
// metaHandler 暴露本实例的 schema 清单(GET /meta)。
//
// 薄壳:真正的组装在 internal/meta(两个 family 共用),这里只把各自的
// schemaMeta 翻译成共用包要的形状 —— 这层翻译必须薄,否则"两个 family 的
// /meta 内容不一致"这种分叉会重新长出来。
func metaHandler(schemas map[string]*schemaMeta, appID string, lg *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		sources := make(map[string]meta.ModelSource, len(schemas))
		for name, m := range schemas {
			if m == nil || m.Schema == nil {
				continue
			}
			sources[name] = meta.ModelSource{
				Schema:      m.Schema,
				Live:        m.Live,
				SourceTable: m.SourceTable,
			}
		}
		models := meta.Build(sources)
		lg.Info("meta", "models", len(models), "app_id", appID)
		c.JSON(http.StatusOK, gin.H{
			"source": appID,
			"models": models,
		})
	}
}

// registryOf 把本实例的全部 model 打包成编译器的 registry。
//
// 只暴露 **live** model:duck model 的表里存的是 canonical 列名(编译时不做列名重写),
// 与 live 的别名限定是两套列名空间,混进来会生成半对的 SQL。duck 侧需要 join 时,
// 编译器会明确报 storage 不匹配,而不是猜。
func registryOf(schemas map[string]*schemaMeta, conn interface{ LegacyTSQL() bool }) cubequery.ModelRegistry {
	entries := map[string]cubequery.ModelEntry{}
	legacy := conn.LegacyTSQL()
	for name, m := range schemas {
		if m == nil || m.Schema == nil || !m.Live {
			continue
		}
		entries[name] = cubequery.ModelEntry{
			Schema:      m.Schema,
			SourceTable: m.SourceTable,
			Mapper:      m.Mapper,
			LegacyTSQL:  legacy,
		}
	}
	return cubequery.NewRegistry(entries)
}

// liveQuerier 是 storage: live model 的查询通道,由具体 connector 实现。
type liveQuerier interface {
	QueryLive(ctx context.Context, query string, args ...any) ([]map[string]any, error)
	// LegacyTSQL 报告源库是否为 SQL Server 2008 及更早(决定日期截断与分页写法)
	LegacyTSQL() bool
}

// storageOf 给日志用的小标签。
func storageOf(m *schemaMeta) string {
	if m.Live {
		return "live"
	}
	return "duck"
}

func queryHandler(db *duckdb.Engine, conn liveQuerier, schemas map[string]*schemaMeta, supportedModels []string, source string, lg *log.Logger) gin.HandlerFunc {
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

		// SQL 拼装走共用包,按 model 的 storage 分两条路径:
		//   duck → cubequery.Build     查 DuckDB(枚举/档案类)
		//   live → cubequery.BuildTSQL 编译成 T-SQL 直接打源库(明细/实时)
		// 两条路径共用同一份 schema、同一套 filter 语义与 RTRIM 规则。
		var built *cubequery.Result
		if meta.Live {
			// 传 registry 是为了支持跨 model 的 joins:查询里出现
			// `settlement.document_type` 这样的成员时,编译器要能查到
			// settlement 的 schema / 源表名 / mapping,才能把 JOIN 拼出来。
			built, err = cubequery.BuildTSQLJoin(q, meta.Schema, meta.SourceTable, meta.Mapper,
				registryOf(schemas, conn))
		} else {
			built, err = cubequery.Build(q, meta.Schema)
		}
		if err != nil {
			var be *cubequery.BuildError
			if errors.As(err, &be) {
				// 必须逐 Kind 分派。2026-10-10 生产现场:
				// dimension / measure 过去兜底成 404 MODEL_NOT_FOUND,
				// gateway 扫到该子码后翻成 "model not exposed by source",
				// 于是「字段名写错」被报成「这个门店没有这个 model」。
				// 两者语义相反 —— model 是好的,是**调用方的查询写错了** ——
				// 报成后者会把排查引向 model 暴露 / 注册,而真正要改的只是查询。
				//
				// 真·「该 source 不暴露这个 model」在上面 schemas[modelName]
				// 那个分支里已经用 404 MODEL_NOT_FOUND 报掉了,不会流到这里。
				switch be.Kind {
				case "filter":
					writeErr(c, http.StatusBadRequest, subCodeQueryInvalid, be.Msg,
						map[string]any{"source": source, "filter_member": be.Ref})
					return
				case "order":
					writeErr(c, http.StatusBadRequest, subCodeQueryInvalid, be.Msg,
						map[string]any{"source": source, "order_member": be.Ref})
					return
				case "query":
					writeErr(c, http.StatusBadRequest, subCodeQueryInvalid, be.Msg,
						map[string]any{"source": source})
					return
				case "dimension", "measure":
					// ref 一并回给调用方,它要靠这个字段名才能改对查询。
					writeErr(c, http.StatusBadRequest, subCodeQueryInvalid, be.Msg,
						map[string]any{"source": source, "model": modelName, "ref": be.Ref})
					return
				case "model":
					// 同样属于调用方写错(query 里没有可推断的 model 前缀),
					// 不是"这个 model 不存在"。
					writeErr(c, http.StatusBadRequest, subCodeQueryInvalid, be.Msg,
						map[string]any{"source": source})
					return
				}
			}
			// 非 BuildError = 编译器内部未知失败,**不是业务结论**。
			// 兜底成 404 会把"我们的 bug"说成"这个 source 没有这个 model",
			// 让排查从查询本身跑偏到部署 / 注册上。
			writeErr(c, http.StatusInternalServerError, subCodeInternalError,
				"build query: "+err.Error(),
				map[string]any{"source": source, "model": modelName})
			return
		}

		var rows []map[string]any
		if meta.Live {
			rows, err = conn.QueryLive(c.Request.Context(), built.SQL, built.Args...)
			if err != nil {
				lg.Info("source query failed", "model", modelName, "sql", built.SQL, "err", err.Error())
				writeErr(c, http.StatusInternalServerError, subCodeInternalError,
					"source: "+err.Error(), map[string]any{"sql": built.SQL})
				return
			}
		} else {
			rows, err = db.QueryMap(built.SQL, built.Args...)
			if err != nil {
				lg.Info("duckdb query failed", "sql", built.SQL, "err", err.Error())
				writeErr(c, http.StatusInternalServerError, subCodeInternalError,
					"duckdb: "+err.Error(), map[string]any{"sql": built.SQL})
				return
			}
		}

		lg.Info("query ok", "model", modelName, "storage", storageOf(meta), "sql", built.SQL, "rows", len(rows))
		total, rows := cubequery.ExtractTotal(rows, q.Total)
		c.JSON(http.StatusOK, gin.H{
			"source":     source,
			"model":      modelName,
			"sql":        built.SQL,
			"data":       rows,
			"measures":   built.MeasureRefs,
			"dimensions": built.DimRefs,
			"total":      total,
		})
	}
}

// liveTables 把 config.yaml 的源表名按 model 名挂起来。
//
// storage: live 的 model 不落 DuckDB,查询时把 schema 编译成 T-SQL 直接打源库。
// 表名只可能来自 config —— 这是 family 私有事实,共享 schema 里不能有。
func liveTables(cfg *appConfig) map[string]string {
	return map[string]string{
		"settlement":          cfg.Source.TableSettlement,
		"settlement_line":     cfg.Source.TableSettlementLine,
		"purchase_sheet":      cfg.Source.TablePurchaseSheet,
		"purchase_sheet_line": cfg.Source.TablePurchaseSheetLn,
		"sale_day":            cfg.Source.TableSaleDay,
		"stock":               cfg.Source.TableStock,
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
