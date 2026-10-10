// Command incrementalcheck 是**增量刷新的真库验证工具**。
//
// # 为什么需要它
//
// 增量刷新的风险**全部是静默的**:水位推错一步 → 漏一批商品;upsert 退化成
// 追加 → DuckDB 悄悄膨胀;增量空结果误清表 → 早上所有商品都查不到。
// 三种都没有任何报错,healthz 全绿,用户看到的只是"某些条码扫不到"。
//
// 而单元测试在本机跑不了:libduckdb 的 dll 在 Windows 上加载失败(缺 VC++ runtime),
// 所有集成测试都 skip —— 而 **skip 不是通过**。本工具在装有 libduckdb 的机器上
// (即真正跑 cube app 的那台)用**真实源库 + 真实 DuckDB** 把这些语义验一遍。
//
// # 它验证什么(全部只读源库)
//
//  1. 全量基线     : 全量加载后 _raw 行数 = N
//  2. 增量真的更小 : 回拨水位 30 天,增量拉回的���数 0 < M < N(这才是优化本身)
//  3. upsert 幂等  : 增量跑完 _raw 行数**仍是 N**(不是 N+M)
//  4. 空增量不删数据: 水位设到未来 → 增量拉回 0 行 → _raw 行数仍是 N
//  5. 无水位会自愈 : 清掉水位 → 增量自动 bootstrap 全量 → 行数仍是 N
//  6. 回落全量     : category(无增量列)始终走全量
//
// 用法:
//
//	go run ./cmd/incrementalcheck -family ysx
//	go run ./cmd/incrementalcheck -family ysx -dsn-host-port 127.0.0.1:11433
//	go run ./cmd/incrementalcheck -family hbposv7 -days 30
//
// 安全:
//   - **源库全程只读**:只发 SELECT / GETDATE()
//   - **不碰生产的 .duckdb 文件**:默认建在临时目录
//   - DSN 只从 config 读进内存,**永不打印**
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/dataloader"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/hbposv7"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/ysx"

	categorymodel "github.com/YunBright/cube/sixun-models/category"
	productmodel "github.com/YunBright/cube/sixun-models/product"
	suppliermodel "github.com/YunBright/cube/sixun-models/supplier"

	models "github.com/YunBright/cube/sixun-models"
)

// config 是实例 config.yaml 的最小子集(与 cmd/sqlcheck 同构)。
type config struct {
	Source struct {
		DSN           string         `yaml:"dsn"`
		Version       string         `yaml:"version"`
		TableSupplier string         `yaml:"table_supplier"`
		TableProduct  string         `yaml:"table_product"`
		TableCategory string         `yaml:"table_category"`
		TableSale     string         `yaml:"table_sale"`
		RowLimit      int            `yaml:"row_limit"`
		RowLimits     map[string]int `yaml:"row_limits"`
	} `yaml:"source"`
	Refresh struct {
		Incremental         map[string]string `yaml:"incremental"`
		IncrementalFallback map[string]string `yaml:"incremental_fallback"`
		FullSchedule        string            `yaml:"full_schedule"`
	} `yaml:"refresh"`
}

var failures int

func fail(format string, args ...any) {
	failures++
	fmt.Printf("  ✗ FAIL  "+format+"\n", args...)
}

func pass(format string, args ...any) {
	fmt.Printf("  ✓      "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	fmt.Printf("FATAL  "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	cfgPath := flag.String("config", "", "实例配置路径(默认 cmd/sixun-<family>/ 下的 config.local.yaml,其次 config.yaml)")
	family := flag.String("family", "hbposv7", "ysx | hbposv7")
	hostPort := flag.String("dsn-host-port", "", "覆盖 DSN 的 host:port(ssh 隧道用)")
	days := flag.Int("days", 30, "回拨水位的天数(决定增量窗口拉回多少行)")
	duckPath := flag.String("duckdb", "", "验证用的 DuckDB 路径(默认在临时目录新建,绝不碰生产文件)")
	flag.Parse()

	cmdDir := map[string]string{
		"ysx":     "sixun-ysx",
		"hbposv7": "sixun-hbposv7",
	}[*family]
	if cmdDir == "" {
		fatal("unknown family %q (want ysx | hbposv7)", *family)
	}
	instanceDir := filepath.Join("cmd", cmdDir)

	if *cfgPath == "" {
		*cfgPath = appcfg.Resolve(instanceDir)
	}
	cfg, usedCfg, err := appcfg.LoadFrom[config](*cfgPath)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("=== incrementalcheck: family=%s config=%s ===\n", *family, usedCfg)
	if cfg.Source.DSN == "" {
		fatal("config.source.dsn is empty")
	}
	if len(cfg.Refresh.Incremental) == 0 {
		fatal("config.refresh.incremental 为空 —— 没有任何 model 配了增量列,没东西可验。"+
			"(示例见 cmd/%s/config.example.yaml)", cmdDir)
	}

	dsn := cfg.Source.DSN
	if *hostPort != "" {
		dsn = rewriteHostPort(dsn, *hostPort)
	}

	schemas, err := loadSchemas(instanceDir, &cfg)
	if err != nil {
		fatal("load schemas: %v", err)
	}

	dbPath := *duckPath
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "incrementalcheck-*")
		if err != nil {
			fatal("建临时目录: %v", err)
		}
		dbPath = filepath.Join(dir, "verify.duckdb")
		defer os.RemoveAll(dir) //nolint:errcheck
		fmt.Printf("=== duckdb: %s (临时,用完删除) ===\n", dbPath)
	}

	db, err := duckdb.Open(dbPath)
	if err != nil {
		fatal("打开 DuckDB 失败:%v\n"+
			"  这个工具必须在**装有 libduckdb 的机器**上跑(生产机上即可)。\n"+
			"  本机跑不了时,单测会静默 skip —— skip 不是通过。", err)
	}
	defer db.Close()

	conn, closeConn, err := connect(*family, dsn, &cfg)
	if err != nil {
		fatal("连源库: %v", err)
	}
	defer closeConn()

	lg := log.New("incrementalcheck")

	// 只验证 duck 的 model(只有它们有增量/水位这回事)。
	specs := []dataloader.Spec{
		{Name: "supplier", Fetch: conn.FetchSupplier, Build: suppliermodel.Build},
		{Name: "product", Fetch: conn.FetchProduct, Build: productmodel.Build},
		{Name: "category", Fetch: conn.FetchCategory, Build: categorymodel.Build},
	}
	liveTables := map[string]string{}
	for name, m := range schemas {
		if m.Live {
			liveTables[name] = m.SourceTable
		}
	}

	incrCols := source.IncrColsFromConfig(cfg.Refresh.Incremental, cfg.Refresh.IncrementalFallback)
	loader := dataloader.NewLoader(db, schemas, specs, conn, incrCols, lg, nil)

	fmt.Printf("\n########## 1. 全量基线 ##########\n")
	loaded := loader.Run(context.Background(), dataloader.ModeFull)
	fmt.Printf("  全量成功: %v\n", loaded)
	if len(loaded) == 0 {
		fatal("全量一个 model 都没成功,后面的验证没有意义")
	}
	baseline := map[string]int{}
	for _, s := range specs {
		n := countRows(db, s.Name+"_raw")
		baseline[s.Name] = n
		fmt.Printf("  %-10s 全量 %d 行,水位=%q\n", s.Name, n, mustWatermark(loader, s.Name))
	}

	for _, model := range []string{"product", "supplier"} {
		if cfg.Refresh.Incremental[model] == "" {
			continue
		}
		verifyIncremental(loader, conn, db, &cfg, model, *days, baseline[model])
	}

	verifyCategoryFallback(loader, db, schemas, "category", baseline["category"])

	if failures > 0 {
		fmt.Printf("\n=== %d 项验证失败 ===\n", failures)
		os.Exit(1)
	}
	fmt.Printf("\n=== 全部通过 ===\n")
}

// verifyIncremental 验一个 model 的增量语义。
func verifyIncremental(loader *dataloader.Loader, conn dataloader.IncrementalSource, db *duckdb.Engine,
	cfg *config, model string, days int, baseline int) {

	fmt.Printf("\n########## %s: 增量语义(回拨 %d 天)##########\n", model, days)

	col := cfg.Refresh.Incremental[model]
	// 回退列与生产用同一份合成逻辑(app 走 source.IncrColsFromConfig)。
	// 这里自己拼一遍的话,验的就不是线上真正在跑的那条 SQL。
	fallback := cfg.Refresh.IncrementalFallback[model]
	cols := source.IncrementalCols{Primary: col, Fallback: fallback}
	// 回拨水位。格式必须与 source.SQLServerTimeLayout 一致(style 121),
	// 免得比较时因格式不匹配而 CAST 失败或静默返回空集。
	since := time.Now().AddDate(0, 0, -days).Format(source.SQLServerTimeLayout)
	old, err := loader.SetWatermark(context.Background(), model, since)
	if err != nil {
		fail("回拨水位: %v", err)
		return
	}
	fmt.Printf("  水位 %q → %q\n", old, since)

	// 2. 探测:这一窗口到底有多少变更。只读,不动 DuckDB。
	now, err := conn.SourceNow(context.Background())
	if err != nil {
		fail("读源库时间: %v", err)
		return
	}
	rows, err := conn.FetchSince(context.Background(), model, cols, since)
	if err != nil {
		fail("增量拉取失败: %v", err)
		return
	}
	m := len(rows)
	fmt.Printf("  源库时间=%s,窗口内变更 %d 行(全量是 %d 行)\n", now, m, baseline)
	if m == 0 {
		fail("回拨 %d 天仍拉不到任何变更 —— 增量列 %q 可能用错了", days, col)
		return
	}
	if m >= baseline {
		fail("增量拉回 %d 行 >= 全量 %d 行,优化没有意义(检查增量列是否真的在过滤)", m, baseline)
	} else {
		pass("增量只拉回 %d/%d 行(%.1f%%)", m, baseline, float64(m)*100/float64(baseline))
	}

	// 3. 真跑一次增量,断言 upsert 幂等。
	loader.Run(context.Background(), dataloader.ModeIncremental)
	after := countRows(db, model+"_raw")
	if after != baseline {
		fail("增量跑完行数应仍是 %d(upsert 覆盖同主键),实际 %d", baseline, after)
	} else {
		pass("upsert 幂等:行数仍是 %d", after)
	}

	// 4. 水位必须推进到源库时间。
	newWM := mustWatermark(loader, model)
	if newWM == since {
		fail("增量后水位没有推进(仍是 %q) —— 下一轮会无限重复拉同一批", newWM)
	} else {
		pass("水位推进 %q → %q", since, newWM)
	}

	// 5. 空增量绝不能清表。
	future := time.Now().AddDate(0, 0, 1).Format(source.SQLServerTimeLayout)
	if _, err := loader.SetWatermark(context.Background(), model, future); err != nil {
		fail("设置未来水位: %v", err)
		return
	}
	loader.Run(context.Background(), dataloader.ModeIncremental)
	if got := countRows(db, model+"_raw"); got != baseline {
		fail("空增量把表清空了:%d → %d。这是纯粹的数据丢失", baseline, got)
	} else {
		pass("空增量保持数据不变(仍是 %d 行)", got)
	}

	// 6. 清掉水位后,增量必须自愈成全量 bootstrap。
	if _, err := loader.SetWatermark(context.Background(), model, ""); err != nil {
		fail("清空水位: %v", err)
		return
	}
	loader.Run(context.Background(), dataloader.ModeIncremental)
	if got := countRows(db, model+"_raw"); got != baseline {
		fail("无水位时应 bootstrap 全量,行数应仍是 %d,实际 %d", baseline, got)
	} else {
		pass("无水位自动 bootstrap 全量,行数 %d", got)
	}
	if wm := mustWatermark(loader, model); wm == "" {
		fail("bootstrap 之后必须写回水位,否则下一轮又会走全量")
	} else {
		pass("bootstrap 后写回水位 %q", wm)
	}
}

// verifyCategoryFallback 验没有增量列的 model 始终走全量。
func verifyCategoryFallback(loader *dataloader.Loader, db *duckdb.Engine,
	schemas map[string]*dataloader.Meta, model string, baseline int) {

	if _, ok := schemas[model]; !ok {
		return
	}
	fmt.Printf("\n########## %s: 无增量列 → 应回落全量 ##########\n", model)

	loader.Run(context.Background(), dataloader.ModeIncremental)
	if got := countRows(db, model+"_raw"); got != baseline {
		fail("回落全量后行数应仍是 %d,实际 %d", baseline, got)
	} else {
		pass("无增量列的 model 走全量,行数 %d", got)
	}
}

// ---- helpers ----

func countRows(db *duckdb.Engine, table string) int {
	rows, err := db.Query("SELECT COUNT(*) FROM " + table)
	if err != nil {
		return -1
	}
	defer rows.Close()
	if !rows.Next() {
		return -1
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		return -1
	}
	return n
}

func mustWatermark(l *dataloader.Loader, model string) string {
	wm, err := l.Watermark(context.Background(), model)
	if err != nil {
		return "<err:" + err.Error() + ">"
	}
	if wm == "" {
		return "<空>"
	}
	return wm
}

// loadSchemas 从 go:embed 的 schema + 磁盘 mapping 组装 dataloader.Meta。
func loadSchemas(instanceDir string, cfg *config) (map[string]*dataloader.Meta, error) {
	mappingFS := os.DirFS(filepath.Join(instanceDir, "mapping"))
	loader, err := fieldmapping.NewLoaderFS(mappingFS)
	if err != nil {
		return nil, fmt.Errorf("加载 mapping: %w", err)
	}

	liveTables := map[string]string{
		"settlement": cfg.Source.TableSupplier, // 占位,下面按 schema 的 storage 决定
	}
	_ = liveTables

	out := map[string]*dataloader.Meta{}
	names, err := models.ModelNames()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		raw, err := models.ReadSchema(name)
		if err != nil {
			return nil, fmt.Errorf("读 schema %s: %w", name, err)
		}
		sch, err := cubeschema.Load(raw)
		if err != nil {
			return nil, fmt.Errorf("解析 schema %s: %w", name, err)
		}
		mapper, ok := loader.Get(name)
		if !ok || mapper == nil {
			continue // live model 不需要 mapping,这里只组装 duck 的
		}
		out[name] = &dataloader.Meta{
			Schema:  sch,
			Columns: mapper.TargetsWithType(),
			Mapper:  mapper,
			Live:    sch.EffectiveStorage() == cubeschema.StorageLive,
		}
	}
	return out, nil
}

// connect 按 family 构造对应 connector。
//
// 返回 source.Connector 而不是更窄的接口:它同时含全量的 Fetch* 与增量的
// FetchSince/SourceNow,而 dataloader.NewLoader 收的是 IncrementalSource
// (source.Connector 的方法集是它的超集,满足)。
func connect(family, dsn string, cfg *config) (source.Connector, func() error, error) {
	limits := source.NewRowLimits(cfg.Source.RowLimit, cfg.Source.RowLimits)
	switch family {
	case "ysx":
		c, err := ysx.New(ysx.Options{
			DSN: dsn, TableSupplier: cfg.Source.TableSupplier, TableProduct: cfg.Source.TableProduct,
			TableCategory: cfg.Source.TableCategory, TableSale: cfg.Source.TableSale,
			RowLimits: limits,
		})
		if err != nil {
			return nil, nil, err
		}
		return c, c.Close, nil
	case "hbposv7":
		c, err := hbposv7.New(hbposv7.Options{
			DSN: dsn, TableSupplier: cfg.Source.TableSupplier, TableProduct: cfg.Source.TableProduct,
			TableCategory: cfg.Source.TableCategory, TableSale: cfg.Source.TableSale,
			RowLimits: limits,
		})
		if err != nil {
			return nil, nil, err
		}
		return c, c.Close, nil
	}
	return nil, nil, fmt.Errorf("unknown family %q", family)
}

// rewriteHostPort 覆盖 DSN 里的 host:port(ssh 隧道用)。
func rewriteHostPort(dsn, hostPort string) string {
	lower := strings.ToLower(dsn)
	idx := strings.LastIndex(lower, "@")
	if idx < 0 {
		fatal("DSN 里没有 @,无法覆盖 host:port")
	}
	tail := dsn[idx+1:]
	slash := strings.Index(tail, "/")
	if slash < 0 {
		fatal("DSN 的 host 部分没有 /,无法覆盖 host:port")
	}
	return dsn[:idx+1] + hostPort + tail[slash:]
}
