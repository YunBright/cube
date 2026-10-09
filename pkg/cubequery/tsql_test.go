package cubequery

import (
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"
)

// liveSchema 是 storage: live 的结算单 schema(列名是 family 无关的 canonical 名)。
func liveSchema() *cubeschema.Model {
	return &cubeschema.Model{
		Name:     "settlement",
		SQLTable: "settlement", // live 下无意义,真正的 FROM 来自 config table_*
		Storage:  cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "supplier_id", SQL: "supplier_id", Type: cubeschema.TypeString},
			{Name: "document_type", SQL: "document_type", Type: cubeschema.TypeString},
			{Name: "settled_at", SQL: "settled_at", Type: cubeschema.TypeTime},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
			{Name: "total_goods_yuan", SQL: "SUM(goods_amount)", Type: cubeschema.TypeNumber},
		},
	}
}

// hbposv7Resolver 复刻 hbposv7 mapping 的反向映射:
// canonical goods_amount ← 源列 Goods_amt;canonical id ← 源列 sheet_no。
func hbposv7Resolver() ColumnResolver {
	return &fieldmapping.Mapper{}
}

func mustMapper(t *testing.T, y string) *fieldmapping.Mapper {
	t.Helper()
	m, err := fieldmapping.Load([]byte(y))
	if err != nil {
		t.Fatalf("load mapping: %v", err)
	}
	return m
}

const hbSettlementMapping = `
version: 1
model: settlement
mappings:
  - source: sheet_no
    target: id
    type: string
  - source: sup_no
    target: supplier_id
    type: string
  - source: trans_no
    target: document_type
    type: string
  - source: settle_date
    target: settled_at
    type: datetime
  - source: Goods_amt
    target: goods_amount
    type: decimal
    unit: yuan
`

// TestBuildTSQL_ResolvesCanonicalColumnsToSourceColumns 是透传最核心的锁:
// schema 写 canonical 名,发往源库的必须是该 family 的真实列名。
func TestBuildTSQL_ResolvesCanonicalColumnsToSourceColumns(t *testing.T) {
	lim := 50
	q := &Query{
		Measures:   []string{"settlement.total_goods_yuan", "settlement.count"},
		Dimensions: []string{"settlement.id", "settlement.supplier_id"},
		Limit:      &lim,
	}
	res, err := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	for _, want := range []string{"RTRIM(sheet_no)", "RTRIM(sup_no)", "SUM(Goods_amt)", "COUNT(*)"} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL 缺少 %q\n实际: %s", want, sql)
		}
	}
	// canonical 名绝不能漏进发给源库的 SQL。
	// 别名 [model.ref] 里保留 canonical 名是**对的** —— 那是响应体的 key,
	// 所以先剥掉 `AS [...]` 再断言。
	exprPart := stripAliases(sql)
	for _, bad := range []string{"goods_amount", "RTRIM(id)", "supplier_id", "settled_at"} {
		if strings.Contains(exprPart, bad) {
			t.Errorf("SQL 表达式里仍含未解析的 canonical 名 %q\n实际: %s", bad, sql)
		}
	}
	if !strings.Contains(sql, "FROM t_fm_recpay_gx_master") {
		t.Errorf("FROM 应是 config 里的源表名,实际: %s", sql)
	}
}

// TestBuildTSQL_UsesTopNotLimit 锁 T-SQL 方言:LIMIT 放最后是 DuckDB 语法,
// 发给 SQL Server 会直接语法错。
func TestBuildTSQL_UsesTopNotLimit(t *testing.T) {
	lim := 25
	q := &Query{Measures: []string{"settlement.count"}, Dimensions: []string{"settlement.id"}, Limit: &lim}
	res, err := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if !strings.Contains(res.SQL, "SELECT TOP 25 ") {
		t.Errorf("应使用 SELECT TOP 25,实际: %s", res.SQL)
	}
	if strings.Contains(strings.ToUpper(res.SQL), "LIMIT") {
		t.Errorf("T-SQL 不支持 LIMIT,实际: %s", res.SQL)
	}
}

// TestBuildTSQL_NumbersPlaceholdersForMSSQL 锁占位符:go-mssqldb 用 @pN,
// 沿用 DuckDB 的 `?` 会直接查询失败。
func TestBuildTSQL_NumbersPlaceholdersForMSSQL(t *testing.T) {
	q := &Query{
		Measures:   []string{"settlement.count"},
		Dimensions: []string{"settlement.id"},
		Filters: []Filter{
			{Member: "settlement.document_type", Operator: "equals", Values: []any{"DP"}},
			{Member: "settlement.supplier_id", Operator: "in", Values: []any{"1045", "1044"}},
		},
	}
	res, err := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if strings.Contains(res.SQL, "?") {
		t.Errorf("SQL 里仍有裸 `?` 占位符,实际: %s", res.SQL)
	}
	if !strings.Contains(res.SQL, "@p1") || !strings.Contains(res.SQL, "@p2") || !strings.Contains(res.SQL, "@p3") {
		t.Errorf("应有 @p1/@p2/@p3 三个占位符,实际: %s", res.SQL)
	}
	if len(res.Args) != 3 {
		t.Errorf("参数个数应为 3,实际 %d", len(res.Args))
	}
	if !strings.Contains(res.SQL, "RTRIM(trans_no) = @p1") {
		t.Errorf("filter 两侧都应 RTRIM 且用命名占位符,实际: %s", res.SQL)
	}
}

// TestBuildTSQL_KeepsStringLiteralsIntact 锁字面量保护:
// `CASE WHEN LEFT(RTRIM(voucher_id),2) IN ('PI','RO')` 里的 'PI'/'RO'
// 若被当标识符处理,SQL 就毁了。
func TestBuildTSQL_KeepsStringLiteralsIntact(t *testing.T) {
	m := mustMapper(t, `
version: 1
model: settlement_line
mappings:
  - source: voucher_no
    target: voucher_id
    type: string
`)
	schema := &cubeschema.Model{
		Name:     "settlement_line",
		SQLTable: "settlement_line",
		Storage:  cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "voucher_id", SQL: "voucher_id", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "total_goods_yuan", SQL: "SUM(CASE WHEN LEFT(RTRIM(voucher_id), 2) IN ('PI','RO') THEN amount ELSE 0 END)", Type: cubeschema.TypeNumber},
		},
	}
	q := &Query{Measures: []string{"settlement_line.total_goods_yuan"}, Dimensions: []string{"settlement_line.voucher_id"}}
	res, err := BuildTSQL(q, schema, "t_fm_recpay_detail", m)
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if !strings.Contains(res.SQL, "IN ('PI','RO')") {
		t.Errorf("字符串字面量被破坏,实际: %s", res.SQL)
	}
	if !strings.Contains(res.SQL, "RTRIM(voucher_no)") {
		t.Errorf("字面量内的 voucher_id 不应被替换,但函数外的应该,实际: %s", res.SQL)
	}
}

// stripAliases 去掉 `AS [xxx]` 别名部分,只留下真正发给源库的表达式。
func stripAliases(sql string) string {
	var b strings.Builder
	for {
		i := strings.Index(sql, " AS [")
		if i < 0 {
			b.WriteString(sql)
			return b.String()
		}
		j := strings.Index(sql[i:], "]")
		if j < 0 {
			b.WriteString(sql)
			return b.String()
		}
		b.WriteString(sql[:i])
		sql = sql[i+j+1:]
	}
}

// TestBuildTSQL_RejectsDuckModel 防止误用:duck model 走 Build,不该被透传。
func TestBuildTSQL_RejectsDuckModel(t *testing.T) {
	duck := testSchema() // testSchema() 没设 Storage → 默认 duck
	if _, err := BuildTSQL(&Query{Measures: []string{"product.count"}}, duck, "t_bd_item_info", nil); err == nil {
		t.Fatal("duck model 不应被 BuildTSQL 接受")
	}
}

// TestBuildTSQL_RequiresSourceTable 源表名来自 config,漏配必须报错而不是
// 拼出一条 FROM 空表的 SQL。
func TestBuildTSQL_RequiresSourceTable(t *testing.T) {
	q := &Query{Measures: []string{"settlement.count"}}
	if _, err := BuildTSQL(q, liveSchema(), "  ", nil); err == nil {
		t.Fatal("源表名为空时应报错")
	}
}
