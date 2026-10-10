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
	"strings"
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
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/ysx"

	models "github.com/YunBright/cube/sixun-models"
	categorymodel "github.com/YunBright/cube/sixun-models/category"
	productmodel "github.com/YunBright/cube/sixun-models/product"
	salemodel "github.com/YunBright/cube/sixun-models/sale_detail"
	suppliermodel "github.com/YunBright/cube/sixun-models/supplier"

	"github.com/gin-gonic/gin"
)

// mappingAssets 是本 family 的字段映射资产。
//
// 云商x 的源字段名和 hbposv7 不同,所以 mapping 必须按 binary 各带一份,
// 不能像 schema 那样在 family 层共享。嵌入后部署只需推一个二进制 ——
// 历史上"推了新 mapping 但漏了 mapping/ 目录"会静默加载不到,
// 表现为该 model 查询时列全空。
//
//go:embed mapping/*.yaml
var mappingAssets embed.FS

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
		// 结算审批场景(2026-10-09)。表名在两个 family 同名同表,
		// 单据类型不同:ysx 的结款单是 CP、hbposv7 是 DP(见 mapping-settlement.yaml)。
		TableSettlement      string `yaml:"table_settlement"`
		TableSettlementLine  string `yaml:"table_settlement_line"`
		TablePurchaseSheet   string `yaml:"table_purchase_sheet"`
		TablePurchaseSheetLn string `yaml:"table_purchase_sheet_line"`
		TableSaleDay         string `yaml:"table_sale_day"`
		// RowLimit 是所有角色共用的拉取行数上限;<=0 时回落到 source.DefaultRowLimit。
		RowLimit int `yaml:"row_limit"`
		// RowLimits 按角色覆盖 RowLimit,键取 source.Role*(supplier/product/category/sale/stock)。
		//
		// 商品表必须给足:hbposv10 的 t_bd_item_info 实测 27299 行,
		// 上限过小会静默丢商品,表现为"部分条码扫不出来"且无任何报错。
		RowLimits map[string]int `yaml:"row_limits"`
	} `yaml:"source"`

	// Refresh 是刷新策略(增量为主 + 夜间全量校准)。
	Refresh struct {
		// Incremental 是 model 名 → **源库侧**的增量时间列名(如 modify_date)。
		//
		// 用源库列名而非 canonical 名,因为 WHERE 打在源库上。
		// 未列出的 model 自动走全量(例如 category 根本没有时间列)。
		//
		// 留空 map = 全部走全量,等于回到改造前的行为 —— 这是一个安全的退路,
		// 不是错误配置。
		Incremental map[string]string `yaml:"incremental"`
		// IncrementalFallback 是 model 名 → 增量时间**回退列**(如 build_date),
		// 拼成 `COALESCE(<incremental>, <fallback>) >= 水位`。
		//
		// 为什么需要:modify_date 为 NULL 意味着"建档后从未被修改过",那这条
		// 记录最后一次被写入的时间就是 build_date。实测 ysx 近 7 天新建的 45 个
		// 商品里 36 个 modify_date 是 NULL —— 单靠 modify_date 会漏掉它们。
		// 实测 modify_date < build_date 的行数为 0,所以不会引入时间倒流。
		//
		// 留空 = 退化成单列比较,行为与配置前完全一致。
		// ⚠️ 它**不能替代夜间全量校准**:hbposv7 有 12607 行商品两个时间列
		//    同时为 NULL,那些行任何增量列都拉不到。
		IncrementalFallback map[string]string `yaml:"incremental_fallback"`
		// FullSchedule 是夜间全量校准的调度式,交给 Dapr 原样解析。
		// 默认 "0 0 3 * * *"(6 字段 cron,每天 03:00:00)。
		//
		// ⚠️ 必须 6 个字段:2026-10-10 真机实测,5 字段会被 Dapr 拒绝
		//    (HTTP 500: expected exactly 6 fields),而**启动不会因此失败**——
		//    主 job 不受影响、mode 照样 dapr-job,只是夜间校准再也不会发生,
		//    增量漏掉的数据永远没有兜底。refreshschedule.validateSchedule 会在
		//    登记前拦一道,让这件事至少变成一条明确的启动日志。
		//
		// 为什么不用 "@every 24h":那是"从登记那一刻起每 24 小时",
		// 进程重启就漂移,永远对不上"夜间 3 点"这个语义。
		FullSchedule string `yaml:"full_schedule"`
	} `yaml:"refresh"`
}

// fullSchedule 返回全量校准的调度式,未配置时给默认的每日 3 点。
func (c appConfig) fullSchedule() string {
	if s := strings.TrimSpace(c.Refresh.FullSchedule); s != "" {
		return s
	}
	return refreshschedule.DefaultFullSchedule
}

// schemaMeta 是 dataloader.Meta 的本地别名 —— 真正的定义在共用包里,
// 两个 family 用同一个类型,加载与重拉逻辑才不可能漂移。
type schemaMeta = dataloader.Meta

type modelFetcher struct {
	name  string
	fetch func(context.Context) ([]map[string]any, error)
	build func(ctx context.Context, db *duckdb.Engine, rawSQL string) error
}

// /query 4xx 子码常量 —— gateway 据此映射到 apierror.Code。
const (
	subCodeModelNotFound      = "MODEL_NOT_FOUND"
	subCodeVersionUnsupported = "VERSION_UNSUPPORTED"
	subCodeQueryParseError    = "QUERY_PARSE_ERROR"
	subCodeQueryInvalid       = "QUERY_INVALID"
	subCodeInternalError      = "INTERNAL_ERROR"
)

type errBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeErr(c *gin.Context, status int, code, msg string, details map[string]any) {
	c.JSON(status, errBody{Code: code, Message: msg, Details: details})
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

func main() {
	ctx := context.Background()

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

	// mapping:编译进二进制,不再依赖 ./mapping/ 目录
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

	// schemas:family 级共享,同样编译进二进制(sixun-models 模块)
	modelNames, err := models.ModelNames()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list embedded schemas:", err)
		os.Exit(1)
	}
	lg.Info("loading schemas", "origin", "embedded", "models", modelNames)
	schemas := map[string]*schemaMeta{}
	live := liveTables(&appCfg)
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
				// 漏配表名必须显式失败:否则该 model 会以 MODEL_NOT_FOUND 静默消失,
				// 表现为"这个表查不了",排查成本极高。
				lg.Info("live model 缺 source.table_*,该 model 不可查询", "model", name)
				continue
			}
			sm.Live = true
			sm.SourceTable = src
		}
		schemas[name] = sm
	}
	lg.Info("schemas loaded", "count", len(schemas), "models", keysOf(schemas))

	conn, err := ysx.New(ysx.Options{
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

	// ⚠️ 变量名不能叫 loader:本文件已有一个 fieldmapping.Loader(从 mapping 资产
	// 构造列映射的那个),重名会让其中一个被悄悄换掉 —— 而且它们的方法名不重叠,
	// 编译器不会报错,只会在运行期变成"某个 Fetch 不重命名了"。
	incrCols := source.IncrColsFromConfig(appCfg.Refresh.Incremental, appCfg.Refresh.IncrementalFallback)
	dl := dataloader.NewLoader(db, schemas, specs, conn, incrCols, lg, fresh)
	incrementalReload := func() []string { return dl.Run(ctx, dataloader.ModeIncremental) }
	fullReload := func() []string { return dl.Run(ctx, dataloader.ModeFull) }

	// ⚠️ 启动必须走**全量**,哪怕 DuckDB 文件里已经有数据。
	//
	// 理由有三个,任何一个都足够:
	//   1. 首次运行时连水位表都没有,没有基线可增量;
	//   2. 水位表可能与 _raw 表**不同步** —— 比如上一版代码是全量刷的,
	//      水位却停在很久以前,增量会把几万行重拉一遍;更糟的是水位**超前**,
	//      那段时间的变更会被永久跳过,而且没有任何报错;
	//   3. 部署之后就应该立刻拿到完整数据,而不是"等下一次增量补齐"。
	// 全量成功后会顺带推进水位,后续增量才是快的那个。
	registeredModels := fullReload()

	// live model 不拉数据,但必须注册:它们是可查询的 model,
	// 漏注册会让 gateway /healthz 与 MODEL_NOT_FOUND 响应暴露成"这个 source 没有该能力"。
	for name, m := range schemas {
		if m.Live {
			lg.Info("live model, 查询时透传源库", "model", name, "source_table", m.SourceTable)
			registeredModels = append(registeredModels, name)
		}
	}
	sort.Strings(registeredModels)

	// 定时刷新:dapr Scheduler 优先,登记失败自动回退进程内 ticker。
	// 两种模式同时可用 —— 在途守卫保证不会重叠写 DuckDB。
	//
	// 两个 job:
	//   cube-refresh       @every <间隔>   增量,只拉水位之后的变更行
	//   cube-refresh-full  <full_schedule>  夜间全量校准,兜住增量天然会漏的那些
	//
	// 只有主 job(cube-refresh)决定 mode 与 ticker:校准 job 登记失败不该让
	// 数据停止刷新,那属于 healthz 的 jobs 段要报告的事,不是 mode 的事。
	jobs := []refreshschedule.JobSpec{
		{
			Name:     refreshschedule.DefaultJobName,
			Schedule: "@every " + cfg.RefreshInterval().String(),
			Reload:   incrementalReload,
			Fallback: true,
		},
		{
			Name:     refreshschedule.DefaultFullJobName,
			Schedule: appCfg.fullSchedule(),
			Reload:   fullReload,
		},
	}
	sched := refreshschedule.Setup(ctx, cfg.SidecarHTTPURL, jobs, lg)
	sched.Start(ctx, lg)
	lg.Info("refresh driver ready",
		"mode", string(sched.Mode()),
		"incremental_models", len(appCfg.Refresh.Incremental),
		"full_schedule", appCfg.fullSchedule())

	go registerToGateway(cfg, registeredModels, lg)
	go watchShutdown(cfg, lg)

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

// queryHandler 完整版(支持多 measure / dimension / filter)。
//
// 4xx 响应一律带 "code" 子码,供 gateway 映射到 apierror.Code。
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
		if len(q.Measures) == 0 {
			writeErr(c, http.StatusBadRequest, subCodeQueryInvalid,
				"at least one measure required", nil)
			return
		}

		// SQL 拼装走共用包,按 model 的 storage 分两条路径:
		//   duck → cubequery.Build     查 DuckDB(枚举/档案类)
		//   live → cubequery.BuildTSQL 编译成 T-SQL 直接打源库(明细/实时)
		// 两条路径共用同一份 schema、同一套 filter 语义与 RTRIM 规则 ——
		// 见 pkg/cubequery/build.go 顶部:这段逻辑曾各 family 各写一份,
		// hbposv7 那份残缺实现会**静默丢弃 filters**,导致查询返回错误商品。
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
// 设计:
//   - 与 engine.Run 各自的 signal handler 共存 —— Go 的 net/http 也接 SIGINT/SIGTERM
//     做 graceful shutdown,这里并行触发 unregister,互不阻塞。
//   - unregisterFromGateway 内置 2s deadline,失败仅记日志 —— 不可阻塞进程退出,
//     否则 kubelet 30s 后给 SIGKILL 就麻烦了。
//   - 调 os.Exit(0) 是有意的:engine.Run 在 signal 后会自然返回,但要在它返回前
//     先发完 unregister —— os.Exit 直接走避免 main 自然退出还要等 deferred close
//     (duckdb / SQL Server) 跑完,把 unregister 卡在 deadline 外。
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

// 防止 json 包 unused 警告。
var _ = json.Marshal
