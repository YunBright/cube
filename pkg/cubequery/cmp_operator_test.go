package cubequery

import (
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"
)

// cmpSchema 是一张 live 结算单表,三种日期列并存 —— 它们是本文件全部断言的基础:
//
//	period_end   真 datetime 源列(type: time,无 char_date)
//	oper_date    char 存 ISO(type: time + char_date: true)
//	sheet_no     非日期列(type: string)
//
// 三者对 gte/lt 的处理必须**各不相同**,而这正是缺陷的藏身处:
// 曾经 gt/gte/lt/lte 一律用 RTRIM 之后的文本表达式比较,于是真 datetime 列
// 被拿去跟 ISO 字面量做文本比 —— MSSQL 隐式转成 "09  1 2026 12:00AM" 再比,
// 结果取决于 SET LANGUAGE。2026-10-19 打生产 ysx 实测:同一账期窗口,
// 文本比较返 0 张,CAST 比较返 113 张。
func cmpSchema() *cubeschema.Model {
	return &cubeschema.Model{
		Name:    "settlement",
		Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "period_end", SQL: "period_end", Type: cubeschema.TypeTime},
			{Name: "oper_date", SQL: "oper_date", Type: cubeschema.TypeTime, CharDate: true},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
	}
}

func cmpMapper(t *testing.T) *fieldmapping.Mapper {
	t.Helper()
	m, err := fieldmapping.Load([]byte(`
version: 1
model: settlement
mappings:
  - source: sheet_no
    target: id
    type: string
  - source: end_date
    target: period_end
    type: datetime
  - source: oper_date
    target: oper_date
    type: string
`))
	if err != nil {
		t.Fatalf("load mapping: %v", err)
	}
	return m
}

// cmpWhere 编译一条单 filter 查询并把 WHERE 段抠出来。
func cmpWhere(t *testing.T, member, op string, vals []any, dialect string) string {
	t.Helper()
	lim := 200
	q := &Query{
		Measures:   []string{"settlement.count"},
		Dimensions: []string{"settlement.id"},
		Filters:    []Filter{{Member: member, Operator: op, Values: vals}},
		Limit:      &lim,
	}
	var sql string
	if dialect == "tsql" {
		res, err := BuildTSQL(q, cmpSchema(), "t_fm_recpay_gx_master", cmpMapper(t))
		if err != nil {
			t.Fatalf("BuildTSQL 失败: %v", err)
		}
		sql = res.SQL
	} else {
		res := mustBuildWith(t, q, cmpSchema())
		sql = res.SQL
	}
	i := strings.Index(sql, " WHERE ")
	if i < 0 {
		t.Fatalf("SQL 里没有 WHERE: %s", sql)
	}
	j := strings.Index(sql[i:], " GROUP BY ")
	if j < 0 {
		return sql[i+7:]
	}
	return sql[i+7 : i+j]
}

// TestCmp_RealDatetimeColumnComparesAsDate 是本文件最重要的一条。
//
// 断言写死字面量 `CAST(RTRIM(end_date) AS DATETIME)`,**不**从被测代码推导
// 期望值 —— 否则把 cmp 退回 col 时断言会跟着变,测试照样全绿。
//
// 注意占位符是 `@p1` 而不是 `?`:BuildTSQL 会把位置占位符改写成 T-SQL 的
// @pN 形式,DuckDB 那条路径才保留 `?`。
func TestCmp_RealDatetimeColumnComparesAsDate(t *testing.T) {
	cases := []struct{ op, sym, want string }{
		{"gt", ">", "CAST(RTRIM(end_date) AS DATETIME) > @p1"},
		{"gte", ">=", "CAST(RTRIM(end_date) AS DATETIME) >= @p1"},
		{"lt", "<", "CAST(RTRIM(end_date) AS DATETIME) < @p1"},
		{"lte", "<=", "CAST(RTRIM(end_date) AS DATETIME) <= @p1"},
	}
	for _, c := range cases {
		t.Run(c.op, func(t *testing.T) {
			where := cmpWhere(t, "settlement.period_end", c.op, []any{"2026-09-01"}, "tsql")
			if !strings.Contains(where, c.want) {
				t.Errorf("真 datetime 列的 %s 必须按日期比较,实际: %s", c.op, where)
			}
			// 反向:不能残留裸的文本比较,那是缺陷本身。
			if strings.Contains(where, "RTRIM(end_date) "+c.sym+" @p1") {
				t.Errorf("%s 仍生成了文本比较: %s", c.op, where)
			}
		})
	}
}

// TestCmp_CharDateColumnKeepsStringCompare 锁住**不回归**的那一半。
//
// char_date 列的源列本来就是存 ISO 的 char,字符串比较是对的(还能走索引)。
// 若这里也改成 CAST,现有三条销售/结算查询会全部退化成不可索引的表达式扫描。
func TestCmp_CharDateColumnKeepsStringCompare(t *testing.T) {
	for _, c := range []struct{ op, want string }{
		{"gt", "RTRIM(oper_date) > @p1"},
		{"gte", "RTRIM(oper_date) >= @p1"},
		{"lt", "RTRIM(oper_date) < @p1"},
		{"lte", "RTRIM(oper_date) <= @p1"},
	} {
		t.Run(c.op, func(t *testing.T) {
			where := cmpWhere(t, "settlement.oper_date", c.op, []any{"2026-09-01"}, "tsql")
			if !strings.Contains(where, c.want) {
				t.Errorf("char_date 列的 %s 应保持字符串比较,实际: %s", c.op, where)
			}
			if strings.Contains(where, "CAST") {
				t.Errorf("char_date 列不该被 CAST: %s", where)
			}
		})
	}
}

// TestCmp_NonDateColumnUntouched 锁住条码/金额这类列没有被日期逻辑波及。
func TestCmp_NonDateColumnUntouched(t *testing.T) {
	for _, c := range []struct{ op, want string }{
		{"gt", "RTRIM(sheet_no) > @p1"},
		{"gte", "RTRIM(sheet_no) >= @p1"},
		{"lt", "RTRIM(sheet_no) < @p1"},
		{"lte", "RTRIM(sheet_no) <= @p1"},
	} {
		t.Run(c.op, func(t *testing.T) {
			where := cmpWhere(t, "settlement.id", c.op, []any{"CP001"}, "tsql")
			if !strings.Contains(where, c.want) {
				t.Errorf("非日期列的 %s 应保持原样,实际: %s", c.op, where)
			}
			if strings.Contains(where, "CAST") {
				t.Errorf("非日期列不该被 CAST: %s", where)
			}
		})
	}
}

// TestCmp_DuckDBCastsToo 锁住 DuckDB 侧同样修好了 ——
// 只修 T-SQL 会让本地 DuckDB 存储与 live 源库对同一查询给出不同答案。
//
// 列名是 canonical 名 `period_end` 而非 `end_date`:Build 不做 mapping 重写
// (那是 BuildTSQL 才做的),所以这一条锁的是"表达式形态",不是最终源列名。
func TestCmp_DuckDBCastsToo(t *testing.T) {
	where := cmpWhere(t, "settlement.period_end", "gte", []any{"2026-09-01"}, "duck")
	if !strings.Contains(where, "CAST(RTRIM(period_end) AS DATE) >= ?") {
		t.Errorf("DuckDB 侧真 datetime 列也应按日期比较,实际: %s", where)
	}
}

// TestCmp_ArgsUnchangedByTheFix 锁住"只改表达式、不改参数"。
//
// 日期值必须仍然走参数绑定。把值内联进 SQL 会同时带来注入面与计划缓存问题。
func TestCmp_ArgsUnchangedByTheFix(t *testing.T) {
	lim := 200
	q := &Query{
		Measures:   []string{"settlement.count"},
		Dimensions: []string{"settlement.id"},
		Filters: []Filter{
			{Member: "settlement.period_end", Operator: "gte", Values: []any{"2026-09-01"}},
			{Member: "settlement.period_end", Operator: "lt", Values: []any{"2026-10-01"}},
		},
		Limit: &lim,
	}
	res, err := BuildTSQL(q, cmpSchema(), "t_fm_recpay_gx_master", cmpMapper(t))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if len(res.Args) != 2 {
		t.Fatalf("应有 2 个参数,got %#v", res.Args)
	}
	if res.Args[0] != "2026-09-01" || res.Args[1] != "2026-10-01" {
		t.Errorf("参数值或顺序不对: %#v", res.Args)
	}
	// 半开区间 [from, to) 的语义必须保持:gte 下界 + lt 上界。
	if !strings.Contains(res.SQL, "CAST(RTRIM(end_date) AS DATETIME) >= @p1") ||
		!strings.Contains(res.SQL, "CAST(RTRIM(end_date) AS DATETIME) < @p2") {
		t.Errorf("半开区间语义被改坏了: %s", res.SQL)
	}
}
