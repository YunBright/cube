// Command sqlcheck 是**上线前的真实源库验证工具**。
//
// # 它验证什么
//
// 单元测试只能证明"我们生成了我们以为会生成的 SQL"。但 T-SQL 的真实风险
// 全在生成之后:
//   - 列名映射错了 → column not found
//   - 别名限定漏了 → "多部分标识符无法解析"
//   - T-SQL 语法细节(DATEFROMPARTS / OFFSET-FETCH / RTRIM 套 char 列)
//   - **结果数字对不对**(扇出、字符补空格、日期窗口)
//
// 这些只有把编译出来的 SQL 原样打到思迅源库才知道。
//
// # 它怎么验证(关键是走**完整链路**,不是手工拼 SQL)
//
//	config.yaml → mapping-*.yaml(磁盘)→ sixun-models schema(go:embed)
//	            → cubequery.BuildTSQLJoin → Connector.QueryLive → 真实结果集
//
// 手写 SQL 去查只能验证"源库能跑",验证不了"我们的编译器产出的 SQL 能跑"。
//
// 用法:
//
//	go run ./cmd/sqlcheck -family ysx|hbposv7
//	go run ./cmd/sqlcheck -family ysx -dsn-host-port 127.0.0.1:11433
//
// 不带 -config 时自动读 cmd/sixun-<family>/ 下的配置:
// config.local.yaml 优先(本地验证用,可指向 ssh 隧道端口或测试库),
// 其次 config.yaml。要读别的文件再显式 -config。
//
// 安全:
//   - **只发 SELECT**:每条语句都过 source.AssertReadOnly,源库是生产库
//   - DSN 只从 config 读进内存,**永不打印**
//   - 每个用例都带 limit,不打无界查询
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/YunBright/cube/pkg/cubequery"
	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"

	models "github.com/YunBright/cube/sixun-models"
)

// config 是实例 config.yaml 的最小子集(与 cmd/sqlq 保持同构)。
type config struct {
	Source struct {
		DSN                string `yaml:"dsn"`
		Version            string `yaml:"version"`
		TableSupplier      string `yaml:"table_supplier"`
		TableProduct       string `yaml:"table_product"`
		TableCategory      string `yaml:"table_category"`
		TableSale          string `yaml:"table_sale"`
		TableStock         string `yaml:"table_stock"`
		TableSettlement    string `yaml:"table_settlement"`
		TableSettlementLn  string `yaml:"table_settlement_line"`
		TablePurchaseSheet string `yaml:"table_purchase_sheet"`
		TablePurchaseLn    string `yaml:"table_purchase_sheet_line"`
		TableSaleDay       string `yaml:"table_sale_day"`
	} `yaml:"source"`
}

func main() {
	cfgPath := flag.String("config", "",
		"实例配置路径(默认 cmd/sixun-<family>/ 下的 config.local.yaml,其次 config.yaml)")
	family := flag.String("family", "hbposv7", "ysx | hbposv7")
	hostPort := flag.String("dsn-host-port", "", "覆盖 DSN 的 host:port(ssh 隧道用)")
	only := flag.String("only", "", "只跑名字含该子串的用例")
	showSQL := flag.Bool("sql", true, "打印每条生成的 SQL")
	flag.Parse()

	// cmd 目录名带 sixun- 前缀(cmd/sixun-hbposv7),family 只是后半段。
	// 早先我直接用 family 当目录名,结果 mapping 一份都没加载 ——
	// 列名一个都没重写,17 条用例全挂在这上面。
	cmdDir := map[string]string{
		"ysx":     "sixun-ysx",
		"hbposv7": "sixun-hbposv7",
	}[*family]
	if cmdDir == "" {
		fail("unknown family %q (want ysx | hbposv7)", *family)
	}
	instanceDir := filepath.Join("cmd", cmdDir)

	// -config 省略时按 appcfg 规则自动选:config.local.yaml 优先。
	// 这样"本地连真实源库验证"就是一条命令,不必每次复制粘贴路径 ——
	// 而路径复制粘贴正是最容易出错、又最难发现的地方。
	if *cfgPath == "" {
		*cfgPath = appcfg.Resolve(instanceDir)
	}
	cfg, usedCfg, err := appcfg.LoadFrom[config](*cfgPath)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("=== config: %s ===\n", usedCfg)
	if cfg.Source.DSN == "" {
		fail("config.source.dsn is empty")
	}
	dsn := cfg.Source.DSN
	if *hostPort != "" {
		dsn = rewriteHostPort(dsn, *hostPort)
	}

	// schema 走 go:embed —— 和 app 二进制里那份是同一份编译产物,
	// 不会出现"验证的 schema 和部署的 schema 不是同一个版本"。
	// mapping 必须读盘:sqlcheck 是**一个**二进制同时服务 ysx / hbposv7,
	// 没法像各 app 那样把各自的 mapping embed 进来。
	// 这两份读盘 mapping 就是各 app embed 的源文件,构建时进二进制。
	mappingDir := filepath.Join(instanceDir, "mapping")

	entries, mappers, err := loadSchemas(mappingDir, &cfg)
	if err != nil {
		fail("load schemas: %v", err)
	}
	liveOnly := map[string]cubequery.ModelEntry{}
	for name, e := range entries {
		if e.Schema != nil && e.Schema.EffectiveStorage() == cubeschema.StorageLive {
			liveOnly[name] = e
		}
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		fail("open sqlserver: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fail("ping source (DSN wrong?): %v", err)
	}
	// 版本探测:两个生产库实测都是 SQL Server 2008 R2(ProductMajorVersion=10),
	// DATEFROMPARTS / OFFSET-FETCH 在那里都是语法错。
	// 探测仍然按能力走而不是写死 —— 换库/升版后不需要改代码。
	legacy, probeErr := source.DetectLegacyTSQL(ctx, db)
	if probeErr != nil {
		fmt.Printf("WARN  版本探测失败,按 SQL Server 2008 方言编译: %v\n", probeErr)
	}
	fmt.Printf("=== connected: family=%s  live models=%d  legacyTSQL=%v  schema=embedded ===\n\n",
		*family, len(liveOnly), legacy)

	// 把探测结果灌进 registry —— 编译期据此选日期截断与分页写法。
	for name, e := range liveOnly {
		e.LegacyTSQL = legacy
		liveOnly[name] = e
	}
	registry := cubequery.NewRegistry(liveOnly)

	cases := buildCases()
	pass, failN := 0, 0
	for _, c := range cases {
		if *only != "" && !strings.Contains(c.name, *only) {
			continue
		}
		runCase(ctx, db, registry, c, mappers, *showSQL, &pass, &failN)
	}
	fmt.Printf("\n=== %d passed, %d failed ===\n", pass, failN)
	if failN > 0 {
		os.Exit(1)
	}
}

type checkCase struct {
	name string
	desc string
	// wantErr 为 true 时期望**编译期**报错(不打到源库)。
	wantErr  bool
	errMatch string
	query    *cubequery.Query
	base     string
}

func runCase(ctx context.Context, db *sql.DB, reg cubequery.ModelRegistry, c checkCase,
	mappers map[string]*fieldmapping.Mapper, showSQL bool, pass, failN *int) {

	entry, ok := reg.Get(c.base)
	if !ok || entry.Schema == nil {
		fmt.Printf("SKIP  %-34s (base model %q 不在 live 集合)\n", c.name, c.base)
		return
	}
	res, err := cubequery.BuildTSQLJoin(c.query, entry.Schema, entry.SourceTable,
		mappers[c.base], reg)

	if c.wantErr {
		if err == nil {
			fmt.Printf("FAIL  %-34s 期望编译期报错,但编译成功了\n", c.name)
			*failN++
			return
		}
		if c.errMatch != "" && !strings.Contains(err.Error(), c.errMatch) {
			fmt.Printf("FAIL  %-34s 报错内容不含 %q\n      got: %v\n", c.name, c.errMatch, err)
			*failN++
			return
		}
		fmt.Printf("ok    %-34s %s\n      按预期被拒: %s\n", c.name, c.desc, truncate(err.Error(), 110))
		*pass++
		return
	}
	if err != nil {
		fmt.Printf("FAIL  %-34s 编译失败: %v\n", c.name, err)
		*failN++
		return
	}
	if showSQL {
		fmt.Printf("SQL   %s\n      %s\n", c.name, truncate(res.SQL, 300))
	}

	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rows, err := db.QueryContext(qctx, res.SQL, res.Args...)
	if err != nil {
		fmt.Printf("FAIL  %-34s 源库执行失败: %v\n", c.name, truncate(err.Error(), 300))
		*failN++
		return
	}
	cols, err := rows.Columns()
	if err != nil {
		fmt.Printf("FAIL  %-34s columns: %v\n", c.name, err)
		*failN++
		_ = rows.Close()
		*failN++
		return
	}
	n := 0
	first := ""
	for rows.Next() {
		if n == 0 && len(cols) > 0 {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if rows.Scan(ptrs...) == nil {
				parts := make([]string, len(cols))
				for i, v := range vals {
					parts[i] = fmt.Sprintf("%s=%v", cols[i], stringify(v))
				}
				first = strings.Join(parts, " ")
			}
		}
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Printf("FAIL  %-34s rows: %v\n", c.name, err)
		*failN++
		_ = rows.Close()
		return
	}
	_ = rows.Close()
	fmt.Printf("ok    %-34s %s\n      rows=%d  %s\n", c.name, c.desc, n, truncate(first, 160))
	*pass++
}

// buildCases 覆盖编译器生成的每一类 SQL 形态。
//
// 全部带 limit —— 打的是生产库,不该有无界查询。
func buildCases() []checkCase {
	lim := func(n int) *int { return &n }
	return []checkCase{
		{
			name: "单表-维度聚合", base: "settlement",
			desc: "最基础的单表查询",
			query: &cubequery.Query{
				Measures:   []string{"settlement.count"},
				Dimensions: []string{"settlement.document_type"},
				Limit:      lim(20),
			},
		},
		{
			name: "单表-别名+金额", base: "settlement",
			desc: "canonical goods_amount → 真实列名(两 family 不同)",
			query: &cubequery.Query{
				Measures:   []string{"settlement.total_goods_yuan"},
				Dimensions: []string{"settlement.id"},
				Limit:      lim(3),
			},
		},
		{
			name: "filter-equals", base: "settlement_line",
			desc: "参数绑定 + RTRIM",
			query: &cubequery.Query{
				Measures: []string{"settlement_line.count"},
				Filters: []cubequery.Filter{
					{Member: "settlement_line.source_doc_type", Operator: "equals", Values: []any{"PI"}},
				},
				Limit: lim(10),
			},
		},
		{
			name: "filter-in", base: "settlement_line",
			desc: "多值 IN 的占位符编号",
			query: &cubequery.Query{
				Measures: []string{"settlement_line.count"},
				Filters: []cubequery.Filter{
					{Member: "settlement_line.source_doc_type", Operator: "in", Values: []any{"PI", "RO"}},
				},
				Limit: lim(10),
			},
		},
		{
			name: "order-desc", base: "settlement",
			desc: "按 SELECT 别名排序",
			query: &cubequery.Query{
				Measures:   []string{"settlement.total_goods_yuan"},
				Dimensions: []string{"settlement.id", "settlement.settled_at"},
				Order:      []cubequery.Order{{ID: "settlement.settled_at", Order: "desc"}},
				Limit:      lim(5),
			},
		},
		{
			name: "offset-分页", base: "settlement",
			desc: "T-SQL OFFSET/FETCH 形态(不能与 TOP 同时出现)",
			query: &cubequery.Query{
				Measures:   []string{"settlement.count"},
				Dimensions: []string{"settlement.id"},
				Order:      []cubequery.Order{{ID: "settlement.id", Order: "asc"}},
				Limit:      lim(5),
				Offset:     cubequery.Offset{10},
				Total:      true,
			},
		},
		{
			name: "timeDimensions-按月", base: "sale_day",
			desc: "DATEFROMPARTS(T-SQL 粒度,不是 DuckDB 的 DATE_TRUNC)",
			query: &cubequery.Query{
				Measures: []string{"sale_day.net_sale_quantity"},
				TimeDimensions: []cubequery.TimeDim{
					{Dimension: "sale_day.business_date", Granularity: "month"},
				},
				Order: []cubequery.Order{{ID: "sale_day.business_date.month", Order: "desc"}},
				Limit: lim(6),
			},
		},
		{
			name: "timeDimensions-char日期窗", base: "sale_day",
			desc: "char(10) 列走字符串比较(可走索引),且值是参数",
			query: &cubequery.Query{
				Measures: []string{"sale_day.count"},
				TimeDimensions: []cubequery.TimeDim{
					{Dimension: "sale_day.business_date",
						DateRange: []any{"2026-09-01", "2026-09-30"}},
				},
				Limit: lim(5),
			},
		},
		{
			name: "dateOperator-inDateRange", base: "sale_day",
			desc: "日期类 operator",
			query: &cubequery.Query{
				Measures: []string{"sale_day.count"},
				Filters: []cubequery.Filter{{
					Member: "sale_day.business_date", Operator: "inDateRange",
					Values: []any{"2026-09-01", "2026-09-30"},
				}},
				Limit: lim(5),
			},
		},
		{
			name: "dateOperator-onTheDate-datetime列", base: "settlement",
			desc: "真 datetime 列的半开区间 [当天, 次日)",
			query: &cubequery.Query{
				Measures: []string{"settlement.count"},
				Filters: []cubequery.Filter{{
					Member: "settlement.settled_at", Operator: "onTheDate",
					Values: []any{"2026-09-15"},
				}},
				Limit: lim(5),
			},
		},
		{
			name: "segment", base: "settlement_line",
			desc: "预定义片段展开 + 其中列名要解析成源列",
			query: &cubequery.Query{
				Measures: []string{"settlement_line.total_goods_yuan"},
				Segments: []string{"purchase_only"},
				Limit:    lim(5),
			},
		},
		{
			name: "join-many_to_one-查采购单类型", base: "settlement_line",
			desc: "两表 JOIN + 别名限定的列 + LEFT JOIN",
			query: &cubequery.Query{
				Measures: []string{"settlement_line.total_amount_yuan"},
				Dimensions: []string{
					"settlement_line.voucher_id",
					"purchase_sheet.document_type",
				},
				Limit: lim(5),
			},
		},
		{
			name: "join-两级链路", base: "settlement_line",
			desc: "一次查询跨 3 张表",
			query: &cubequery.Query{
				Measures: []string{"settlement_line.total_amount_yuan"},
				Dimensions: []string{
					"settlement.document_type",
					"purchase_sheet.document_type",
				},
				Limit: lim(5),
			},
		},
		{
			name: "join-带filter跨表", base: "settlement_line",
			desc: "filter 落在 joined 表上",
			query: &cubequery.Query{
				Measures: []string{"settlement_line.total_amount_yuan"},
				Dimensions: []string{
					"settlement_line.voucher_id",
					"purchase_sheet.document_type",
				},
				Filters: []cubequery.Filter{{
					Member: "purchase_sheet.document_type", Operator: "equals",
					Values: []any{"PI"},
				}},
				Limit: lim(5),
			},
		},
		{
			name: "join-one_to_many-取多的一侧度量", base: "settlement",
			desc: "扇出的正确用法:度量取自多的一侧",
			query: &cubequery.Query{
				Measures:   []string{"settlement_line.total_amount_yuan"},
				Dimensions: []string{"settlement.id", "settlement_line.voucher_id"},
				Limit:      lim(5),
			},
		},
		{
			name: "扇出防护-应被拒", base: "settlement", wantErr: true,
			errMatch: "multiplied",
			desc:     "one_to_many 会把度量乘 N 倍,必须编译期拦下",
			query: &cubequery.Query{
				Measures:   []string{"settlement.total_goods_yuan"},
				Dimensions: []string{"settlement.id", "settlement_line.voucher_id"},
				Limit:      lim(5),
			},
		},
		{
			name: "未声明join-应被拒", base: "settlement_line", wantErr: true,
			errMatch: "no join chain",
			desc:     "只走 schema 显式声明的关联",
			query: &cubequery.Query{
				Measures:   []string{"settlement_line.total_amount_yuan"},
				Dimensions: []string{"sale_day.business_date"},
				Limit:      lim(5),
			},
		},
	}
}

// ---- schema / mapping 加载 ----

// loadSchemas 从**内嵌** schema 资产 + **磁盘** mapping 组装 model 入口。
//
// schema 走 models 包(go:embed),mapping 走 mappingDir(见 main 里的理由)。
func loadSchemas(mappingDir string, cfg *config) (
	map[string]cubequery.ModelEntry, map[string]*fieldmapping.Mapper, error) {

	entries := map[string]cubequery.ModelEntry{}
	mappers := map[string]*fieldmapping.Mapper{}

	// live model 的源表名来自实例 config(表名是 family 私有事实,不进 schema)。
	srcTables := map[string]string{
		"settlement":          cfg.Source.TableSettlement,
		"settlement_line":     cfg.Source.TableSettlementLn,
		"purchase_sheet":      cfg.Source.TablePurchaseSheet,
		"purchase_sheet_line": cfg.Source.TablePurchaseLn,
		"sale_day":            cfg.Source.TableSaleDay,
		"stock":               cfg.Source.TableStock,
	}

	names, err := models.ModelNames()
	if err != nil {
		return nil, nil, err
	}
	for _, name := range names {
		b, err := models.ReadSchema(name)
		if err != nil {
			return nil, nil, err
		}
		m, err := cubeschema.Load(b)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		mp := &fieldmapping.Mapper{}
		if mb, err := os.ReadFile(filepath.Join(mappingDir, "mapping-"+name+".yaml")); err == nil {
			mp, err = fieldmapping.Load(mb)
			if err != nil {
				return nil, nil, fmt.Errorf("mapping-%s: %w", name, err)
			}
		}
		mappers[name] = mp
		if m.EffectiveStorage() != cubeschema.StorageLive {
			continue
		}
		src := srcTables[name]
		if src == "" {
			fmt.Printf("WARN  live model %q 没有配 source.table_*,跳过\n", name)
			continue
		}
		entries[name] = cubequery.ModelEntry{Schema: m, SourceTable: src, Mapper: mp}
	}
	return entries, mappers, nil
}

// rewriteHostPort 只替换 DSN 里 ? 之前最后一个 @ 之后的 host:port。
func rewriteHostPort(dsn, hostPort string) string {
	idx := strings.LastIndex(dsn, "@")
	if idx < 0 {
		return dsn
	}
	hostStart := idx + 1
	rest := dsn[hostStart:]
	var query string
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
		rest = rest[:q]
	}
	if c := strings.Index(rest, ","); c >= 0 {
		rest = rest[:c]
	}
	return dsn[:hostStart] + hostPort + query
}

func stringify(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case nil:
		return "NULL"
	default:
		return fmt.Sprint(x)
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
