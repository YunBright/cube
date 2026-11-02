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
//  2. ./config.yaml(DSN + 源表名:table_<model> 一一对应)
//  3. 内嵌 mapping-*.yaml(go:embed,不再读 ./mapping/ 目录)
//  4. 内嵌 sixun-models schema(go:embed,不再读磁盘目录)
//  5. hbposv7.Connector 打开 SQL Server
//  6. 全部 24 个 model 都是 storage: live —— **不拉任何数据、不落本地副本**,
//     查询时把 schema 编译成 T-SQL 直打源库。
//     (2026-10-23 以前这里还有第 6~7 步:打开 .duckdb 文件 → 拉全量 →
//      mapping → 物化 → 登记刷新 job → 夜间全量校准。整套机制已删除,
//      理由见 AGENTS.md §4.1。)
//  7. goroutine: cfg.RegisterBody → POST /register
//  8. gin engine:/query /healthz
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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/boot"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/meta"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/hbposv7"

	models "github.com/YunBright/cube/sixun-models"

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
// 本地副本路径已删除 —— 2026-10-23 起实例不持有任何数据副本)。
type appConfig struct {
	Source struct {
		DSN           string `yaml:"dsn"`
		Version       string `yaml:"version"`
		TableSupplier string `yaml:"table_supplier"`
		TableProduct  string `yaml:"table_product"`
		TableCategory string `yaml:"table_category"`
		// TableCustType 是供应商/客户类别字典表(t_bd_cust_type)。
		// supplier.cust_clsno 指向它;实测 ysx 4 行 / hbposv7 0 行。
		TableCustType string `yaml:"table_cust_type"`
		// TableSaleWay 是「销售方式」字典表(t_bd_base_code)。
		// supplier.supply_way(源列 sale_way)指向它;
		// 实测两个库的供应商全部有值(ysx 219 / hbposv7 300)。
		TableSaleWay string `yaml:"table_sale_way"`
		// TableSaleDetail 是**逐笔销售流水**表(t_rm_saleflow,两个 family 同名同表)。
		//
		// ⚠️ 2026-10-22:旧的 `table_sale` 键**退役**了 ——
		//   它配的是旧的(副本)sale_detail 路径:ysx t_mp_saleflow 实测 **0 行**,
		//   hbposv7 t_rm_order_saleflow 实测**表根本不存在** ⇒ 两个库都取不到数据,
		//   而症状是"查询 200 OK 返回空数组",零报错。
		//   sale_detail 现在是 live model,源表名按 `table_<model>` 约定与 model 名对齐
		//   (键名与 model 名不一致 = 少接一条,该 model 静默不可查)。
		TableSaleDetail string `yaml:"table_sale_detail"`
		// SaleDetailReturnQtyCol 是"本行被退掉的数量"列名。
		//
		// ⚠️ **hbposv7 故意留空**:实测它的 t_rm_saleflow **没有**这一列
		// (2026-10-23 真库:ret_qnty 无效),而该库的逐笔流水里
		// sale_qnty / sale_money / sale_price 的负值行都是 0 行
		// ⇒ 根本没有退货行 ⇒ 片段按"不扣退货"生成(净额 = 原销售)。
		//
		// ⚠️ 这是 **family 私有事实**,所以走 config 而不是 mapping
		// (mapping 的 target 集合两个 family 必须逐字一致)。
		SaleDetailReturnQtyCol string `yaml:"sale_detail_return_qty_col"`
		TableStock             string `yaml:"table_stock"`
		// ---- 2026-10-22 新增的 live model 源表 ----
		// 收银流水 / 促销方案头 / 商品供货信息 / 商品档案价格。
		// 表名是 family 私有事实,所以只在这里配,不进共享的 sixun-models/
		// (见 AGENTS.md §4「live 表名来源」)。
		TablePayflow       string `yaml:"table_payflow"`
		// TableOperator 是收银员档案表(t_sys_operator)。
		// payflow 只存 oper_id，姓名在这一张表；资金对账按收银员展示时按工号回查。
		TableOperator      string `yaml:"table_operator"`
		TablePromotionPlan string `yaml:"table_promotion_plan"`
		TableSupplierItem  string `yaml:"table_supplier_item"`
		TableBranchPrice   string `yaml:"table_branch_price"`
		// TableMember 是会员卡档案表(t_rm_vip_info)。
		// 实测 hbposv7/hbposepro = 2642 行 / ysx/hbposv10 = 4128 行。
		TableMember string `yaml:"table_member"`
		// TableMemberType 是会员类别字典表(t_rm_vip_type)。
		// member.card_type 指向它;实测两个库各 3 行。
		TableMemberType string `yaml:"table_member_type"`
		// 会员模型的派生列来源表(余额 / 累计消费 / 消费次数)。
		//
		// ⚠️ 这三列的真值**不在** t_rm_vip_info 里:
		//   余额(residual_amt)是脏字符串、累计消费 / 消费次数主表里几乎全空。
		//   ⇒ 取数 SQL 走 source.MemberFromFragment(带 JOIN 的 live 片段),口径见 internal/source/member_sql.go。
		TableMemberConsume    string `yaml:"table_member_consume"`
		TableMemberPaylist    string `yaml:"table_member_paylist"`
		TableMemberSavingPlus string `yaml:"table_member_savingplus"`
		// MemberStoredPayType 是 paylist.card_type 里表示"储值支付"的取值
		// (空 = source.DefaultStoredPayType = '2')。
		MemberStoredPayType string `yaml:"member_stored_pay_type"`
		// MemberPointsAccumulatedCol / MemberPointsUsedCol 是积分列名(派生剩余积分用)。
		// ⚠️ 本库没有 acc_num / dec_num ⇒ 不配 ⇒ 不派生该列。
		MemberPointsAccumulatedCol string `yaml:"member_points_accumulated_col"`
		MemberPointsUsedCol        string `yaml:"member_points_used_col"`
		// 结算审批场景(2026-10-09)。表名在两个 family 同名同表,
		// 单据类型不同:hbposv7 的结款单是 DP(供应商对帐单)、ysx 是 CP。
		TableSettlement     string `yaml:"table_settlement"`
		TableSettlementLine string `yaml:"table_settlement_line"`
		// 联营帐款(2026-10-31)。联营单在**独立表** t_fm_recpay_ly_master /
		// t_fm_recpay_ly_detail(实测 gx_master 里只有 DP/CP,LY = 0 张)。
		TableSettlementLY     string `yaml:"table_settlement_ly"`
		TableSettlementLYLine string `yaml:"table_settlement_ly_line"`
		// 代销帐款(2026-11-01)。同样在**独立表** t_fm_recpay_dx_master /
		// t_fm_recpay_dx_detail_v4(gx_master 里没有 DX)。
		TableSettlementDX     string `yaml:"table_settlement_dx"`
		TableSettlementDXLine string `yaml:"table_settlement_dx_line"`
		TablePurchaseSheet    string `yaml:"table_purchase_sheet"`
		TablePurchaseSheetLn  string `yaml:"table_purchase_sheet_line"`
		TableSaleDay          string `yaml:"table_sale_day"`
		// 盘损(2026-10-31)。库存类单据头(JO 报损 / OO 库存调整)与明细行。
		// 这两张 table_* 是"进货偏离销量"判据的**解释源**:有报损时按 销量+盘损 判定。
		// ⚠️ 采购单不在 t_im_sheet_master,别把它和 t_pm_sheet_master 搞混。
		TableStockLoss     string `yaml:"table_stock_loss"`
		TableStockLossLine string `yaml:"table_stock_loss_line"`
		// 销售报表(2026-10-15)。日汇总流水 + 商品档案 + 类别字典。
		//
		// ⚠️ TableItemClsParent 与 TableItemCls 指向**同一张物理表**(t_bd_item_cls),
		//   靠自关联的 cls_parent 取父类别。同表两个 model 是 pkg/cubequery/join.go
		//   明确给出的机制(planJoins 的 visited 不允许自连接同一 model 名)。
		// ⚠️ 数据源的门店区域(branch_no)刻意不接进来:业务口径已确认忽略门店区域,
		//   接入它等于让"按门店过滤"重新变成一条没人验证过 WHERE。
		TableSalesReport   string `yaml:"table_sales_report"`
		TableItem          string `yaml:"table_item"`
		TableItemCls       string `yaml:"table_item_cls"`
		TableItemClsParent string `yaml:"table_item_cls_parent"`
		// 促销(2026-10-15)。POP 价签打印要"当前生效特价",时效性极强,
		// 所以 promotion 是 live model(不落本地副本),漏配表名该 model 直接不可查。
		TablePromotion string `yaml:"table_promotion"`
		// 商品条码补充表(2026-10-21)。一品多码:一个货号可有 1~7 个条码。
		// 实测两库都有该表(ysx 153 行 / hbposv7 同表同列)。
		TableItemBarcode string `yaml:"table_item_barcode"`
		// TableItemCombo 是组合/拆分关系表(t_bd_item_combsplit,2026-11-01)。
		//
		// 为什么要它:整箱进货、拆散零售的商品(大米/饮料/啤酒),销售日结只记
		// **子商品**的销量,而进货记在**父商品**上 ⇒ 不归并就会得出
		// "进货 1 包、卖出 0"的假事实,把正常进货判成超量。
		//
		// ⚠️ 实测(ysx/hbposv10)556 行:other1='1' 520 行是**拆分**
		//   (comb_item_no → item_no × item_qty,item_qty > 0);
		//   other1='6' 36 行是**组合**(打包销售,item_qty 恒 0 ⇒ 不可换算)。
		TableItemCombo string `yaml:"table_item_combo"`
	} `yaml:"source"`
}

// schemaMeta 是本实例的 model 元信息。
//
// ⚠️ 2026-10-23:去掉本地副本之后**没有"物化出来的表"了** ⇒ 不再有列清单
// (Columns)与预聚合物化器 —— 只剩 schema / 列名映射 / live 源。
type schemaMeta struct {
	Schema *cubeschema.Model
	// Mapper 把 schema 的 canonical 列名反查成源库列名(live 透传时用)。
	Mapper *fieldmapping.Mapper
	// Live 为真表示该 model 不落盘,查询时透传源库。
	Live bool
	// SourceTable 是 live model 的源(表名 **或 SQL 片段**),来自实例 config。
	SourceTable string
}

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
	live, err := liveTables(&appCfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live tables:", err)
		os.Exit(1)
	}

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
		sm := &schemaMeta{Schema: schema, Mapper: mapper}
		if schema.Storage == cubeschema.StorageLive {
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
	// ⚠️ 2026-10-23:connector 只剩 **DSN** 一项。
	//
	// 以前这里传了一长串 Table*(源表名)与 RowLimits(行数上限)——
	// 它们**只**服务于"把 model 拉下来物化进本地副本"。
	// 副本层删除后:源表名由 liveTables() 交给编译器,行数由查询里的 limit 决定。
	conn, err := hbposv7.New(hbposv7.Options{
		DSN: appCfg.Source.DSN,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open SQL Server:", err)
		os.Exit(1)
	}
	defer conn.Close()
	lg.Info("SQL Server connected")

	// 6. ⚠️ 2026-10-23:**全仓去掉本地副本** —— 全部 model 都是 live(查询时透传源库),
	// **不保留任何本地副本**,也不做任何原始数据同步(见 AGENTS.md §4.1)。
	//
	// 以前(2026-10-23 之前)这里是:开 .duckdb 文件 → 按 model 拉数物化 → 登记刷新 job → 夜间全量校准。
	// 那些机制一个都不需要了,而且每一个都会"悄悄坏掉"
	// (副本过期了而报告照样正常 —— 这是缓存最贵的一种失败)。
	registeredModels := make([]string, 0, len(schemas))
	for name, m := range schemas {
		// live model 不拉数据,但必须注册:它们是可查询的 model,
		// 漏注册会让 gateway /healthz 与 MODEL_NOT_FOUND 响应暴露成"这个 source 没有该能力"。
		lg.Info("live model, 查询时透传源库", "model", name, "source_table", m.SourceTable)
		registeredModels = append(registeredModels, name)
	}
	sort.Strings(registeredModels)

	// 8. 注册到 gateway
	go registerToGateway(cfg, registeredModels, lg)
	go watchShutdown(cfg, lg)

	// 9. HTTP server
	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())
	engine.POST("/query", queryHandler(conn, schemas, registeredModels, cfg.AppID, lg))
	engine.GET("/healthz", cfg.HealthHandler(registeredModels))
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
// 只暴露 **live** model —— 现在**所有** model 都是 live(本地副本层已于
// 2026-10-23 删除)⇒ 这个过滤实际上恒真;保留它是为了防止有人手工塞一个
// 非 live 的 Model 进 schemas,那样编译器会生成半对的 SQL。
func registryOf(schemas map[string]*schemaMeta, conn interface{ LegacyTSQL() bool }) cubequery.ModelRegistry {
	entries := map[string]cubequery.ModelEntry{}
	// ⚠️ conn 允许为 nil:错误路径的单测只验证"错误分类不依赖任何 IO 成功",
	// 而 registry 是在**拼 SQL 之前**就要构造的 ⇒ 这里直接解引用会 panic,
	// 症状是连接被掐断(EOF)而不是一个可断言的 4xx。
	legacy := false
	if conn != nil {
		legacy = conn.LegacyTSQL()
	}
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

func queryHandler(conn liveQuerier, schemas map[string]*schemaMeta, supportedModels []string, source string, lg *log.Logger) gin.HandlerFunc {
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

		// SQL 拼装走共用包:live model 编译成 T-SQL 直接打源库。
		//
		// ⚠️ 2026-10-23 去掉本地副本之后**只有这一条路**了 ——
		// 以前是两条(副本 → cubequery.Build 查本地库,live → BuildTSQL 打源库),
		// 现在统一走 BuildTSQLJoin。
		//
		// 传 registry 是为了支持跨 model 的 joins:查询里出现
		// `settlement.document_type` 这样的成员时,编译器要能查到
		// settlement 的 schema / 源表名 / mapping,才能把 JOIN 拼出来。
		built, err := cubequery.BuildTSQLJoin(q, meta.Schema, meta.SourceTable, meta.Mapper,
			registryOf(schemas, conn))
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

		rows, err := conn.QueryLive(c.Request.Context(), built.SQL, built.Args...)
		if err != nil {
			lg.Info("source query failed", "model", modelName, "sql", built.SQL, "err", err.Error())
			writeErr(c, http.StatusInternalServerError, subCodeInternalError,
				"source: "+err.Error(), map[string]any{"sql": built.SQL})
			return
		}

		lg.Info("query ok", "model", modelName, "storage", "live", "sql", built.SQL, "rows", len(rows))
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
// storage: live 的 model 不落本地副本,查询时把 schema 编译成 T-SQL 直接打源库。
// 表名只可能来自 config —— 这是 family 私有事实,共享 schema 里不能有。
//
// # ⚠️ 两种"源"形态
//
//   - **普通表名**(category / supplier / member_type / cust_type …):直接用。
//   - **SQL 片段**(member / sale_way):整段 SQL,当**派生表**用。
//     片段的列名必须是**源列名**(含 mapping 里的 memb_\* 派生别名),
//     编译器会把 canonical 名重写成它们 ⇒ 列名对得上。
func liveTables(cfg *appConfig) (map[string]string, error) {
	// 会员模型:卡内余额 / 累计消费 / 消费次数是"主表 JOIN 三张流水表"算出来的,
	// 源库里**没有这几列** ⇒ live 路必须用片段在源库侧算。
	memberFragment, err := source.MemberFromFragment(source.MemberTables{
		Member:               cfg.Source.TableMember,
		Consume:              cfg.Source.TableMemberConsume,
		Paylist:              cfg.Source.TableMemberPaylist,
		SavingPlus:           cfg.Source.TableMemberSavingPlus,
		StoredPayType:        cfg.Source.MemberStoredPayType,
		PointsAccumulatedCol: cfg.Source.MemberPointsAccumulatedCol,
		PointsUsedCol:        cfg.Source.MemberPointsUsedCol,
	})
	if err != nil {
		return nil, fmt.Errorf("member live 片段: %w", err)
	}
	// sale_way:t_bd_base_code 是**通用**基础代码字典(30+ 行,code_id 跨类型重复)
	// ⇒ 必须只取 type_no='SW' 的子集,否则 code_id→code_name 的对应关系是错的。
	//
	// ⚠️ 用 `SELECT *` 而不是写死列名:code_id / code_name 是**源列名**,
	// 两个 family 可能不同 ⇒ 由 mapping 去重写更稳。
	saleWayFragment := ""
	if t := strings.TrimSpace(cfg.Source.TableSaleWay); t != "" {
		saleWayFragment = "(SELECT * FROM " + t + " WHERE type_no = 'SW')"
	}
	// sale_detail:逐笔流水的**退货不是独立行**,而是同一行里的 ret_qnty
	// (ysx 独有列)⇒ "净额"只能由 family 私有的片段算。
	// 本库没有该列 ⇒ 片段按"不扣退货"生成(见 internal/source/sale_detail_sql.go)。
	saleDetailFragment, err := source.SaleDetailFragment(source.SaleDetailTables{
		Table:        cfg.Source.TableSaleDetail,
		ReturnQtyCol: cfg.Source.SaleDetailReturnQtyCol,
	})
	if err != nil {
		return nil, fmt.Errorf("sale_detail live 片段: %w", err)
	}
	return map[string]string{
		"settlement":          cfg.Source.TableSettlement,
		"settlement_line":     cfg.Source.TableSettlementLine,
		"settlement_ly":       cfg.Source.TableSettlementLY,
		"settlement_ly_line":  cfg.Source.TableSettlementLYLine,
		"settlement_dx":       cfg.Source.TableSettlementDX,
		"settlement_dx_line":  cfg.Source.TableSettlementDXLine,
		"purchase_sheet":      cfg.Source.TablePurchaseSheet,
		"purchase_sheet_line": cfg.Source.TablePurchaseSheetLn,
		"sale_day":            cfg.Source.TableSaleDay,
		// 盘损(2026-10-31)。漏配 ⇒ 该 model 在启动期被跳过,盘损读取会 400 MODEL_NOT_FOUND。
		"stock_loss":      cfg.Source.TableStockLoss,
		"stock_loss_line": cfg.Source.TableStockLossLine,
		"stock":           cfg.Source.TableStock,
		// 销售报表四件套。item / item_cls / item_cls_parent 同理各要一张表名:
		// 少配一个,对应 model 会在 main() 里被跳过并只留一行日志。
		"sales_report":    cfg.Source.TableSalesReport,
		"item":            cfg.Source.TableItem,
		"item_cls":        cfg.Source.TableItemCls,
		"item_cls_parent": cfg.Source.TableItemClsParent,
		"promotion":       cfg.Source.TablePromotion,
		// 商品条码补充表(2026-10-21)。live model 不落本地副本,查询时透传源库。
		"item_barcode": cfg.Source.TableItemBarcode,
		// 组合/拆分关系(2026-11-01)。漏配 ⇒ 该 model 启动期被跳过,
		// 上层归并查询会 400 MODEL_NOT_FOUND。
		"item_combo": cfg.Source.TableItemCombo,
		// 商品档案(2026-10-22 改成 live)。
		//
		// ⚠️ 为什么必须 live:pkg/cubequery/join.go 的 checkStorage 拒绝任何
		// 非 live 的 join 两侧,而后台的「商品销售流水」查询**同时**要
		// sale_detail.* 与 product.* ⇒ sale_detail → product 必须 live → live。
		// 顺带把 stock → product 这条 join 从「必然被拒」变成可用。
		"product": cfg.Source.TableProduct,
		// ---- 2026-10-22 新增的五个 live model ----
		// 逐笔销售流水 / 收银流水 / 促销方案头 / 商品供货信息 / 商品档案价格。
		//
		// ⚠️ 键名必须与 model 名一致:少一条就是「该 model 静默不可查」
		// (main.go 只打一行 Info 日志然后 continue,healthz 照样全绿)。
		//
		// ⚠️ sale_detail 是**片段**不是表名:净额要把"本行被退掉的数量"扣掉,
		// 而那一列是 family 私有事实(本库没有 ⇒ 不扣)。
		"sale_detail":    saleDetailFragment,
		"payflow":        cfg.Source.TablePayflow,
		"operator":       cfg.Source.TableOperator,
		"promotion_plan": cfg.Source.TablePromotionPlan,
		"supplier_item":  cfg.Source.TableSupplierItem,
		"branch_price":   cfg.Source.TableBranchPrice,
		// ---- 2026-10-23:最后 6 个 model 也改成 live ⇒ **一个都不落本地副本** ----
		// 档案/字典类(供应商/分类/客户类别/会员类别)体量小(219~4128 行),
		// 之前留副本只是因为"档案变化少"—— 而"变化少"恰恰是**最该实时**的
		// (新建的商品/供应商要立刻能查到,不该等一次同步)。
		"supplier":    cfg.Source.TableSupplier,
		"category":    cfg.Source.TableCategory,
		"cust_type":   cfg.Source.TableCustType,
		"member_type": cfg.Source.TableMemberType,
		"sale_way":    saleWayFragment,
		// ⚠️ member 是**片段**不是表名(派生列要 JOIN 三张流水表)。
		"member": memberFragment,
	}, nil
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
