// join_test.go 锁多表 join 的语义。
//
// 本文件锁三件事:
//  1. **列名按 model 分派 + 别名限定** —— 单表的盲替换在 JOIN 下是错的
//  2. **扇出防护** —— one_to_many 会把"一"那一侧的度量乘 N 倍,必须编译期拦下
//  3. **只走显式声明的 joins** —— 不做 JIT 自动关联(那是拖垮源库的查询)
package cubequery

import (
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"
)

// approvalChain 复刻审批链路的三个 model:
// settlement(1) ──1:N── settlement_line(N) ──N:1── purchase_sheet(1)
//
// 真实表:两 family 同名同表,列名却不同(hbposv7 的 Goods_amt / ysx 的 sheet_amt),
// 这正是必须按 model 分派解析的原因。
func approvalSchemas() (*cubeschema.Model, *cubeschema.Model, *cubeschema.Model) {
	settlement := &cubeschema.Model{
		Name: "settlement", SQLTable: "settlement", Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "document_type", SQL: "document_type", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
			{Name: "goods_amount_yuan", SQL: "SUM(goods_amount)", Type: cubeschema.TypeNumber},
		},
		Joins: []cubeschema.Join{{
			Name: "settlement_line", Relationship: "one_to_many",
			On: "{CUBE}.id = settlement_line.settlement_id",
		}},
	}
	line := &cubeschema.Model{
		Name: "settlement_line", SQLTable: "settlement_line", Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "settlement_id", SQL: "settlement_id", Type: cubeschema.TypeString},
			{Name: "voucher_id", SQL: "voucher_id", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
			{Name: "amount_yuan", SQL: "SUM(amount)", Type: cubeschema.TypeNumber},
		},
		Joins: []cubeschema.Join{
			{Name: "settlement", Relationship: "many_to_one", On: "{CUBE}.settlement_id = settlement.id"},
			{Name: "purchase_sheet", Relationship: "many_to_one", On: "{CUBE}.voucher_id = purchase_sheet.id"},
		},
	}
	sheet := &cubeschema.Model{
		Name: "purchase_sheet", SQLTable: "purchase_sheet", Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "document_type", SQL: "document_type", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
	}
	return settlement, line, sheet
}

// approvalRegistry 用**真实的列名差异**构造:settlement 的 id 是 sheet_no,
// 而 settlement_line 的 settlement_id 也是 sheet_no —— 两张表同名列不同表,
// 正是盲替换会翻车的地方。
func approvalRegistry(t *testing.T) (ModelRegistry, *cubeschema.Model) {
	t.Helper()
	settlement, line, sheet := approvalSchemas()
	sm := mustMapper(t, `
version: 1
model: settlement
mappings:
  - source: sheet_no
    target: id
    type: string
  - source: trans_no
    target: document_type
    type: string
  - source: Goods_amt
    target: goods_amount
    type: decimal
`)
	lm := mustMapper(t, `
version: 1
model: settlement_line
mappings:
  - source: sheet_no
    target: settlement_id
    type: string
  - source: voucher_no
    target: voucher_id
    type: string
  - source: amount
    target: amount
    type: decimal
`)
	pm := mustMapper(t, `
version: 1
model: purchase_sheet
mappings:
  - source: sheet_no
    target: id
    type: string
  - source: trans_no
    target: document_type
    type: string
`)
	reg := NewRegistry(map[string]ModelEntry{
		"settlement":      {Schema: settlement, SourceTable: "t_fm_recpay_gx_master", Mapper: sm},
		"settlement_line": {Schema: line, SourceTable: "t_fm_recpay_detail", Mapper: lm},
		"purchase_sheet":  {Schema: sheet, SourceTable: "t_pm_sheet_master", Mapper: pm},
	})
	return reg, line
}

// TestJoin_LookupJoinQualifiesEachTableColumns 查档案型(many_to_one)的 join。
//
// 关键断言:**两张表的同名列被解析成各自的别名**,没有混在一起。
func TestJoin_LookupJoinQualifiesEachTableColumns(t *testing.T) {
	reg, line := approvalRegistry(t)
	lm := reg.(interface {
		Get(string) (ModelEntry, bool)
	})
	entry, _ := lm.Get("settlement_line")

	q := &Query{
		Measures:   []string{"settlement_line.amount_yuan"},
		Dimensions: []string{"settlement_line.voucher_id", "settlement.document_type"},
	}
	res, err := BuildTSQLJoin(q, line, "t_fm_recpay_detail", entry.Mapper, reg)
	if err != nil {
		t.Fatalf("BuildTSQLJoin 失败: %v", err)
	}
	sql := res.SQL

	if !strings.Contains(sql, "FROM t_fm_recpay_detail AS t0") {
		t.Errorf("base 表应带别名,实际: %s", sql)
	}
	if !strings.Contains(sql, "LEFT JOIN t_fm_recpay_gx_master AS t") {
		t.Errorf("many_to_one 查档案应用 LEFT JOIN(用 INNER 会让无档案的行凭空消失),实际: %s", sql)
	}
	// settlement.document_type → t?.trans_no,而 settlement_line.voucher_id → t0.voucher_no。
	if !strings.Contains(sql, "RTRIM(t0.voucher_no)") {
		t.Errorf("base 表列应带 t0 前缀,实际: %s", sql)
	}
	if strings.Contains(sql, "RTRIM(trans_no)") {
		t.Errorf("joined 表的列没打上别名前缀(会与 base 表的同名列混淆): %s", sql)
	}
	// settlement_line.amount_yuan → SUM(t0.amount)
	if !strings.Contains(sql, "SUM(t0.amount)") {
		t.Errorf("base 表的度量列应带别名,实际: %s", sql)
	}
}

// TestJoin_OneToManyMultipliesMeasureIsRejected 是本文件**最重要**的一条。
//
// settlement(1) ⋈ settlement_line(N) 之后,一个结算单变成 N 行,
// 此时对 settlement.goods_amount 求 SUM 得到的是 N 倍金额 ——
// SQL 合法、查询 200 OK、金额凭空翻倍。必须在编译期就拦下。
func TestJoin_OneToManyMultipliesMeasureIsRejected(t *testing.T) {
	reg, _ := approvalRegistry(t)
	settlement, _, _ := approvalSchemas()
	smEntry, _ := reg.Get("settlement")

	q := &Query{
		Measures:   []string{"settlement.goods_amount_yuan"},
		Dimensions: []string{"settlement.id", "settlement_line.voucher_id"},
	}
	_, err := BuildTSQLJoin(q, settlement, "t_fm_recpay_gx_master", smEntry.Mapper, reg)
	var be *BuildError
	if !errors.As(err, &be) || be.Kind != "query" {
		t.Fatalf("one_to_many 会放大度量,应报 Kind=query,got %v", err)
	}
	if !strings.Contains(be.Msg, "multiplied") {
		t.Errorf("错误信息要说清是被复制放大,got: %s", be.Msg)
	}
	// 错误里要给出可执行的方向,而不只是"不行"。
	if !strings.Contains(be.Msg, "many side") {
		t.Errorf("错误信息应指出改用多的一侧的度量,got: %s", be.Msg)
	}
}

// TestJoin_OneToManyWithManySideMeasureIsAllowed 同一张表,但度量取自"多"的一侧
// 就是正确的 —— 不该被误伤。
func TestJoin_OneToManyWithManySideMeasureIsAllowed(t *testing.T) {
	reg, _ := approvalRegistry(t)
	settlement, _, _ := approvalSchemas()
	smEntry, _ := reg.Get("settlement")

	q := &Query{
		Measures:   []string{"settlement_line.amount_yuan"},
		Dimensions: []string{"settlement.id", "settlement_line.voucher_id"},
	}
	res, err := BuildTSQLJoin(q, settlement, "t_fm_recpay_gx_master", smEntry.Mapper, reg)
	if err != nil {
		t.Fatalf("度量取自多的一侧是安全的,不该报错: %v", err)
	}
	if !strings.Contains(res.SQL, "INNER JOIN t_fm_recpay_detail") {
		t.Errorf("one_to_many 应用 INNER JOIN,实际: %s", res.SQL)
	}
	// base 表的度量必须仍然只对它自己那张表求和
	if !strings.Contains(res.SQL, "SUM(t1.amount)") {
		t.Errorf("度量应打在多的一侧别名上,实际: %s", res.SQL)
	}
}

// TestJoin_UndeclaredJoinIsRejected 只走 schema 显式声明的关联。
// 没声明就是没声明 —— 不做"猜同名表"的 JIT 自动关联。
func TestJoin_UndeclaredJoinIsRejected(t *testing.T) {
	reg, _ := approvalRegistry(t)
	lineEntry, _ := reg.Get("settlement_line")
	line := lineEntry.Schema

	q := &Query{
		Measures:   []string{"settlement_line.amount_yuan"},
		Dimensions: []string{"sale_day.business_date"},
	}
	_, err := BuildTSQLJoin(q, line, "t_fm_recpay_detail", lineEntry.Mapper, reg)
	var be *BuildError
	if !errors.As(err, &be) {
		t.Fatalf("未声明的 join 必须报错,got %v", err)
	}
	if !strings.Contains(be.Msg, "no join chain") {
		t.Errorf("错误信息要说清是没有声明这条 join,got: %s", be.Msg)
	}
}

// TestJoin_CrossStorageJoinIsRejected 跨 storage 的 join 会要求在一条语句里
// 同时查两个引擎,必须明确拒绝而不是生成半对的 SQL。
func TestJoin_CrossStorageJoinIsRejected(t *testing.T) {
	duck := &cubeschema.Model{
		Name: "product", SQLTable: "product", // storage 默认 duck
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber}},
		Joins: []cubeschema.Join{{
			Name: "supplier", Relationship: "many_to_one",
			On: "{CUBE}.supplier_id = supplier.id",
		}},
	}
	_, line, _ := approvalSchemas()
	reg := NewRegistry(map[string]ModelEntry{
		"product":         {Schema: duck, SourceTable: "product"},
		"settlement_line": {Schema: line, SourceTable: "t_fm_recpay_detail"},
		"supplier":        {Schema: &cubeschema.Model{Name: "supplier", SQLTable: "supplier", Dimensions: duck.Dimensions, Measures: duck.Measures}, SourceTable: "supplier"},
	})
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id", "supplier.id"},
	}
	_, err := BuildTSQLJoin(q, duck, "product", nil, reg)
	var be *BuildError
	if !errors.As(err, &be) {
		t.Fatalf("duck model 的 join 应被拒绝,got %v", err)
	}
	if !strings.Contains(be.Msg, "live") {
		t.Errorf("错误信息要说清限制来自 storage,got: %s", be.Msg)
	}
}

// TestJoin_SingleTableSQLIsByteIdentical 保证 join 支持**没有**改变单表查询的
// 输出。这条很重要:一旦单表 SQL 也变了,所有既有的行为锁就失效了。
//
// 比的是两条 **T-SQL** 路径(旧的单表入口 vs 新的 registry 入口)——
// DuckDB 路径的 SQL 本来就不同(引号/别名/无重写),拿它们比没有意义。
func TestJoin_SingleTableSQLIsByteIdentical(t *testing.T) {
	q := &Query{
		Measures:   []string{"sale_day.sale_qty"},
		Dimensions: []string{"sale_day.item_id"},
	}
	schema := saleDaySchema()
	m := mustMapper(t, saleDayMapping)
	plain, err := BuildTSQL(q, schema, "t_rm_daysum", m)
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	reg := NewRegistry(map[string]ModelEntry{
		"sale_day": {Schema: schema, SourceTable: "t_rm_daysum", Mapper: m},
	})
	withReg, err := BuildTSQLJoin(q, schema, "t_rm_daysum", m, reg)
	if err != nil {
		t.Fatalf("BuildTSQLJoin 失败: %v", err)
	}
	if withReg.SQL != plain.SQL {
		t.Errorf("单表查询经 registry 路径后 SQL 变了\n旧: %s\n新: %s", plain.SQL, withReg.SQL)
	}
	if strings.Contains(plain.SQL, " JOIN ") {
		t.Errorf("单表查询不该出现 JOIN: %s", plain.SQL)
	}
	// 单表也不能凭空多出别名限定 —— 那是 JOIN 才需要的。
	if strings.Contains(plain.SQL, "t0.") {
		t.Errorf("单表查询不该打别名: %s", plain.SQL)
	}
}

// TestJoin_ChainedJoinsProduceAllEdges 三级链路:
// settlement → settlement_line → purchase_sheet,两张 JOIN 都要出现。
func TestJoin_ChainedJoinsProduceAllEdges(t *testing.T) {
	reg, line := approvalRegistry(t)
	lmEntry, _ := reg.Get("settlement_line")

	q := &Query{
		Measures: []string{"settlement_line.amount_yuan"},
		Dimensions: []string{
			"settlement.document_type",
			"purchase_sheet.document_type",
		},
	}
	res, err := BuildTSQLJoin(q, line, "t_fm_recpay_detail", lmEntry.Mapper, reg)
	if err != nil {
		t.Fatalf("BuildTSQLJoin 失败: %v", err)
	}
	sql := res.SQL
	for _, want := range []string{
		"LEFT JOIN t_fm_recpay_gx_master AS ",
		"LEFT JOIN t_pm_sheet_master AS ",
		"FROM t_fm_recpay_detail AS t0",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("应包含 %q,实际: %s", want, sql)
		}
	}
	// 两张 joined 表的 ON 都要各自成立
	if !strings.Contains(sql, "ON t0.sheet_no = ") && !strings.Contains(sql, "ON t0.voucher_no = ") {
		t.Errorf("ON 条件里 base 表列应带别名,实际: %s", sql)
	}
}

// TestJoin_SameSQLForSameQuery 编译必须确定:同样的查询两次生成同样的 SQL。
// (map 遍历顺序会泄漏到输出里,是那种"重启一次结果就变了"的怪 bug。)
func TestJoin_SameSQLForSameQuery(t *testing.T) {
	build := func() string {
		reg, line := approvalRegistry(t)
		e, _ := reg.Get("settlement_line")
		q := &Query{
			Measures: []string{"settlement_line.amount_yuan"},
			Dimensions: []string{
				"settlement.document_type",
				"settlement_line.voucher_id",
				"purchase_sheet.document_type",
			},
		}
		res, err := BuildTSQLJoin(q, line, "t_fm_recpay_detail", e.Mapper, reg)
		if err != nil {
			t.Fatalf("BuildTSQLJoin 失败: %v", err)
		}
		return res.SQL
	}
	first := build()
	for i := 0; i < 20; i++ {
		if got := build(); got != first {
			t.Fatalf("第 %d 次编译出的 SQL 与第一次不同(确定性被破坏)\n首次: %s\n本次: %s", i, first, got)
		}
	}
}

var _ = fieldmapping.Mapper{}
