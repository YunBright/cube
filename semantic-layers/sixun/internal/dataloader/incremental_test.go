package dataloader

// 增量刷新的回归锁。
//
// 这套测试咬住的是**数据正确性**,不是实现细节。增量最危险的地方在于:
// 失败方式是"看起来一切正常,但数据悄悄少了一部分"。所以每条断言都问
// "如果这条逻辑错了,用户会看到什么",而不是"函数有没有被调用"。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
)

// ---- 测试脚手架 ----

const testMappingYAML = `
version: 1
model: product
mappings:
  - source: item_no
    target: id
    type: string
  - source: item_name
    target: name
    type: string
`

// testSchemaYAML(withPK) 构造一个最小 schema。
func testSchemaYAML(withPK bool) string {
	pk := ""
	if withPK {
		pk = "primary_key: id\n"
	}
	return "name: product\nsql_table: product\n" + pk +
		"dimensions:\n" +
		"  - name: id\n    sql: id\n    type: string\n" +
		"  - name: name\n    sql: name\n    type: string\n" +
		"measures:\n" +
		"  - name: count\n    sql: COUNT(*)\n    type: number\n"
}

// fakeSource 是可编程的 IncrementalSource。
type fakeSource struct {
	now  string
	rows []map[string]any

	// 记录调用顺序,用来断言"先读时间再拉数"。
	calls []string
	// fetchErr / nowErr 用来模拟源库故障。
	fetchErr error
	nowErr   error
}

func (f *fakeSource) FetchSince(ctx context.Context, model string, cols source.IncrementalCols, since string) ([]map[string]any, error) {
	f.calls = append(f.calls, "fetch:"+since)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.rows, nil
}

func (f *fakeSource) SourceNow(ctx context.Context) (string, error) {
	f.calls = append(f.calls, "now")
	if f.nowErr != nil {
		return "", f.nowErr
	}
	return f.now, nil
}

type harness struct {
	db      *duckdb.Engine
	meta    *Meta
	loader  *Loader
	src     *fakeSource
	schemas map[string]*Meta
	specs   []Spec
}

// newHarness 构造一个带真实 DuckDB 的测试环境。
//
// 用真实 DuckDB 而不是 mock:增量的核心风险全在 SQL 语义上
// (DELETE ... IN 分批、主键类型比较、空结果语义),mock 一个都不会碰到。
func newHarness(t *testing.T, withPK bool) *harness {
	t.Helper()

	dir := t.TempDir()
	eng, err := duckdb.Open(filepath.Join(dir, "test.duckdb"))
	if err != nil {
		t.Skipf("libduckdb 不可用,跳过集成测试: %v", err)
	}
	t.Cleanup(func() { eng.Close() })

	sch, err := cubeschema.Load([]byte(testSchemaYAML(withPK)))
	if err != nil {
		t.Fatalf("加载测试 schema: %v", err)
	}
	mapper, err := fieldmapping.Load([]byte(testMappingYAML))
	if err != nil {
		t.Fatalf("加载测试 mapping: %v", err)
	}

	meta := &Meta{
		Schema:  sch,
		Columns: mapper.TargetsWithType(),
		Mapper:  mapper,
	}
	src := &fakeSource{now: "2026-10-10 03:00:00.000"}
	specs := []Spec{{
		Name:  "product",
		Fetch: func(context.Context) ([]map[string]any, error) { return src.rows, nil },
		Build: func(ctx context.Context, db *duckdb.Engine, rawSQL string) error {
			return db.BuildPreAgg("product", rawSQL)
		},
	}}
	schemas := map[string]*Meta{"product": meta}
	lg := log.New("test")

	return &harness{
		db:      eng,
		meta:    meta,
		src:     src,
		schemas: schemas,
		specs:   specs,
		loader: NewLoader(eng, schemas, specs, src,
			map[string]source.IncrementalCols{
				"product": {Primary: "modify_date"},
			}, lg, nil),
	}
}

func row(id, name string) map[string]any {
	return map[string]any{"item_no": id, "item_name": name}
}

func (h *harness) rawCount(t *testing.T) int {
	t.Helper()
	rows, err := h.db.Query("SELECT COUNT(*) FROM product_raw")
	if err != nil {
		t.Fatalf("查 product_raw: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("product_raw 没有行")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n
}

func (h *harness) watermark(t *testing.T) string {
	t.Helper()
	wm, err := h.loader.readWatermark(context.Background(), "product")
	if err != nil {
		t.Fatalf("读水位: %v", err)
	}
	return wm
}

// ---- 测试 ----

// TestLoader_FullLoadsAndAdvancesWatermark 全量之后水位必须推进。
//
// 不推进的后果不是"慢",而是下一轮增量会从很旧的水位开始拉 ——
// 夜里的全量校准反而把紧随其后的增量变成了全量,完全抵消了优化。
func TestLoader_FullLoadsAndAdvancesWatermark(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A"), row("b", "B")}

	h.loader.Run(context.Background(), ModeFull)

	if got := h.rawCount(t); got != 2 {
		t.Fatalf("全量应写入 2 行,实际 %d", got)
	}
	if wm := h.watermark(t); wm != h.src.now {
		t.Fatalf("全量后水位应等于源库时间 %q,实际 %q", h.src.now, wm)
	}
}

// TestLoader_IncrementalWithoutWatermarkBootstrapsFull 没有水位就必须全量。
//
// 没有基线的"增量"只可能拉到空集或全量,取决于时间从哪算。
// 无论哪种,用户的商品档案都是空的 —— 而 healthz 会全绿。
func TestLoader_IncrementalWithoutWatermarkBootstrapsFull(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A"), row("b", "B")}

	// 注意 mode 是增量,但库里还没有水位。
	h.loader.Run(context.Background(), ModeIncremental)

	if got := h.rawCount(t); got != 2 {
		t.Fatalf("无水位时应先全量 bootstrap,实际写入 %d 行", got)
	}
	if wm := h.watermark(t); wm == "" {
		t.Fatal("bootstrap 之后必须写入水位,否则下一轮又会退化成全量")
	}
}

// TestLoader_IncrementalUpdatesInPlaceKeepsRowCount 同一主键重复拉不能增加行。
//
// 这是 upsert 语义的核心断言。如果实现退化成"只 INSERT 不 DELETE",
// 行数会随每次刷新单调增长,DuckDB 越滚越大,而所有字段看起来都对。
func TestLoader_IncrementalUpdatesInPlaceKeepsRowCount(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A"), row("b", "B")}
	h.loader.Run(context.Background(), ModeFull)
	before := h.rawCount(t)

	// 增量:同一批主键,名称变了。
	h.src.rows = []map[string]any{row("a", "A-改过"), row("b", "B-改过")}
	h.loader.Run(context.Background(), ModeIncremental)

	if after := h.rawCount(t); after != before {
		t.Fatalf("增量覆盖同主键不应改变行数:before=%d after=%d(说明在 INSERT 而不是 upsert)", before, after)
	}
	rows, err := h.db.Query(`SELECT name FROM product_raw WHERE id = 'a'`)
	if err != nil {
		t.Fatalf("查: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("行 a 不见了 —— 增量把已有行删掉却没插回来")
	}
	var name string
	_ = rows.Scan(&name)
	if name != "A-改过" {
		t.Fatalf("增量应更新字段值,实际仍是 %q", name)
	}
}

// TestLoader_IncrementalAddsNewRows 新主键必须被追加。
//
// 与上一条成对:只锁"不增长"的话,"全部丢弃"也能过 ——
// 那正是"增量拉了但一行没插进去"的症状,表现为新条码扫不到且零报错。
func TestLoader_IncrementalAddsNewRows(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A")}
	h.loader.Run(context.Background(), ModeFull)

	h.src.rows = []map[string]any{row("a", "A"), row("c", "C")}
	h.loader.Run(context.Background(), ModeIncremental)

	if got := h.rawCount(t); got != 2 {
		t.Fatalf("增量应追加新主键,实际 %d 行(期望 2)", got)
	}
}

// TestLoader_EmptyIncrementalKeepsRows 增量空结果**必须保持表不变**。
//
// 这是增量与全量刻意相反的语义:
//   - 全量 + 空 = 源库真的空了,必须让表变空(否则把"数据变空"藏起来)
//   - 增量 + 空 = 这段时间没有变更,清表就是纯粹的数据丢失
//
// 把这条与全量的空结果语义搞混,最典型的事故是"凌晨没变更,早上所有商品都查不到"。
func TestLoader_EmptyIncrementalKeepsRows(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A"), row("b", "B")}
	h.loader.Run(context.Background(), ModeFull)

	h.src.rows = nil // 这段时间没有变更
	h.loader.Run(context.Background(), ModeIncremental)

	if got := h.rawCount(t); got != 2 {
		t.Fatalf("增量空结果必须保持表不变,实际变成 %d 行 —— 清表就是数据丢失", got)
	}
	// 水位仍要推进:"确认过没有变更"本身是要记住的信息。
	if wm := h.watermark(t); wm != h.src.now {
		t.Fatalf("增量空结果后水位仍应推进到 %q,实际 %q", h.src.now, wm)
	}
}

// TestLoader_FullEmptyStillClears 锁全量与增量在空结果上的**相反**语义。
//
// 只测其中一条的话,任何一次"把两者统一化"的重构都能悄悄改错另一条。
func TestLoader_FullEmptyStillClears(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A")}
	h.loader.Run(context.Background(), ModeFull)

	h.src.rows = nil
	h.loader.Run(context.Background(), ModeFull)

	if got := h.rawCount(t); got != 0 {
		t.Fatalf("全量空结果应清空表(让「数据变空」这件事看得见),实际仍有 %d 行", got)
	}
}

// TestLoader_NoPrimaryKeyFallsBackToFull 没主键必须回落全量。
//
// 绝不允许"没有主键也照样 upsert":那会产生不可控的行重复,
// 而且是**静默**的 —— 行数增长、DuckDB 膨胀、查询变慢,没有任何报错。
func TestLoader_NoPrimaryKeyFallsBackToFull(t *testing.T) {
	h := newHarness(t, false) // schema 不带 primary_key
	h.src.rows = []map[string]any{row("a", "A")}

	if h.loader.canIncrement("product", h.meta) {
		t.Fatal("没有 primary_key 时必须不能增量")
	}
	if reason := h.loader.noIncrementReason("product", h.meta); reason == "" {
		t.Fatal("必须给出回落全量的原因,否则日志里看不到为什么这个 model 还在全量刷")
	}

	h.loader.Run(context.Background(), ModeFull)
	h.src.rows = []map[string]any{row("a", "A"), row("b", "B")}
	h.loader.Run(context.Background(), ModeIncremental)

	if got := h.rawCount(t); got != 2 {
		t.Fatalf("回落全量后增量刷新应仍是全量替换,实际 %d 行", got)
	}
}

// TestLoader_NoIncrementalColFallsBackToFull 没配增量列同样回落全量。
// category 就是这种 model(源表根本没有时间列)。
func TestLoader_NoIncrementalColFallsBackToFull(t *testing.T) {
	h := newHarness(t, true)
	if h.loader.canIncrement("category", h.meta) {
		t.Fatal("未配置增量列的 model 必须不能增量")
	}
}

// TestLoader_WatermarkNotAdvancedWhenBuildFails 预聚合失败时水位不能推进。
//
// 方向的选择很重要:水位推进但数据没进去 = 那段变更**永久丢失**,而且
// 没有任何报错。宁可下一轮重拉(upsert 幂等,浪费一点源库 IO)。
func TestLoader_WatermarkNotAdvancedWhenBuildFails(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A")}
	h.loader.Run(context.Background(), ModeFull)
	wmAfterFull := h.watermark(t)

	// 让 Build 失败。
	h.specs[0].Build = func(context.Context, *duckdb.Engine, string) error {
		return errors.New("模拟预聚合失败")
	}
	h.src.now = "2026-10-10 04:00:00.000"
	h.src.rows = []map[string]any{row("a", "A"), row("b", "B")}

	loaded := h.loader.Run(context.Background(), ModeIncremental)

	if len(loaded) != 0 {
		t.Fatalf("Build 失败时不应计入成功列表,实际 %v", loaded)
	}
	if wm := h.watermark(t); wm != wmAfterFull {
		t.Fatalf("Build 失败后水位不得推进(否则那段变更永久丢失):before=%q after=%q",
			wmAfterFull, wm)
	}
}

// TestLoader_SourceNowReadBeforeFetch 锁取水位的时机顺序。
//
// 必须**先读源库时间再拉数**:反过来做,拉取耗时期间产生的变更会被算进
// "已处理区间",而它们的 modify_date 可能小于随后读到的时间 → 永久丢失。
// 这类 bug 在数据没变更时完全看不出来,只有真的发生并发写入才会显形。
func TestLoader_SourceNowReadBeforeFetch(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A")}
	h.loader.Run(context.Background(), ModeFull)

	h.src.calls = nil
	h.src.rows = nil
	h.loader.Run(context.Background(), ModeIncremental)

	if len(h.src.calls) < 2 {
		t.Fatalf("增量路径应至少调用 SourceNow + FetchSince,实际 %v", h.src.calls)
	}
	if h.src.calls[0] != "now" || h.src.calls[1] != "fetch:"+h.src.now {
		t.Fatalf("顺序错误:期望 [now, fetch:<水位>],实际 %v", h.src.calls)
	}
}

// TestLoader_SourceNowFailureAbortsIncremental 读不到源库时间就不做增量。
//
// 水位是增量的**唯一**依据,拿不到就不能猜一个 —— 猜错就是静默丢数据。
func TestLoader_SourceNowFailureAbortsIncremental(t *testing.T) {
	h := newHarness(t, true)
	h.src.rows = []map[string]any{row("a", "A")}
	h.loader.Run(context.Background(), ModeFull)
	before := h.rawCount(t)

	h.src.nowErr = errors.New("源库不可达")
	h.src.rows = []map[string]any{row("b", "B")}

	loaded := h.loader.Run(context.Background(), ModeIncremental)

	if len(loaded) != 0 {
		t.Fatalf("读不到源库时间时不应算成功,实际 %v", loaded)
	}
	if got := h.rawCount(t); got != before {
		t.Fatalf("水位拿不到时不应改动数据:before=%d after=%d", before, got)
	}
}

// TestLoader_IncrementalUsesFetchNotFullFetch 增量模式不能调用全量 Fetcher。
//
// 两者的行数上限语义不同(增量也用同一个 row limit,但没有 ORDER BY,
// 超过上限时会随机截断变更集)。更重要的是:如果增量路径悄悄走了全量
// Fetch,增量就退化成了"更慢的全量",而没有任何外部症状。
func TestLoader_IncrementalUsesFetchNotFullFetch(t *testing.T) {
	h := newHarness(t, true)
	var fullCalls int
	h.specs[0].Fetch = func(context.Context) ([]map[string]any, error) {
		fullCalls++
		return nil, nil
	}
	h.src.rows = []map[string]any{row("a", "A")}
	h.loader.Run(context.Background(), ModeFull)
	fullAfterBootstrap := fullCalls

	h.src.rows = nil
	h.loader.Run(context.Background(), ModeIncremental)

	if fullCalls != fullAfterBootstrap {
		t.Fatalf("增量模式不得调用全量 Fetcher(会退化成更慢的全量),实际多调了 %d 次",
			fullCalls-fullAfterBootstrap)
	}
}
