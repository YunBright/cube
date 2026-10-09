// dateop_test.go 锁日期类 filter operator。
//
// 这些 operator(inDateRange / onTheDate / beforeDate / afterDate)
// 与 timeDimensions 的 dateRange 走**同一套列语义**(buildFilter 里的 target
// 计算与 time.go 的 timeColumn 一致)。两者若各写各的,就会出现"按月汇总对了、
// 按日期筛却筛错"这种只在真实数据上才暴露的分叉。
package cubequery

import (
	"strings"
	"testing"
)

func TestDateOp_InDateRangeOnCharColumn(t *testing.T) {
	q := &Query{
		Measures:   []string{"sale_day.sale_qty"},
		Dimensions: []string{"sale_day.item_id"},
		Filters: []Filter{{
			Member:   "sale_day.oper_date",
			Operator: "inDateRange",
			Values:   []any{"2026-07-01", "2026-09-30"},
		}},
	}
	res := mustBuildWith(t, q, saleDaySchema())
	if !strings.Contains(res.SQL, "RTRIM(oper_date) >= ? AND RTRIM(oper_date) <= ?") {
		t.Errorf("char 列应直接字符串比较(可走索引),实际: %s", res.SQL)
	}
	if len(res.Args) != 2 {
		t.Errorf("应有 2 个参数,got %#v", res.Args)
	}
}

func TestDateOp_OnTheDateCharVsDatetime(t *testing.T) {
	// char 列:ISO 字符串天然按天对齐,等值即可。
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		Filters:  []Filter{{Member: "sale_day.oper_date", Operator: "onTheDate", Values: []any{"2026-08-15"}}},
	}
	charSQL := mustBuildWith(t, q, saleDaySchema()).SQL
	if !strings.Contains(charSQL, "RTRIM(oper_date) = ?") {
		t.Errorf("char 列 onTheDate 应是等值,实际: %s", charSQL)
	}

	// 真 datetime 列:必须用 [当天, 次日) 半开区间,否则带上时间部分就漏数据。
	schema := saleDaySchema()
	schema.Dimensions[1].CharDate = false
	dtSQL := mustBuildWith(t, q, schema).SQL
	hasLower := strings.Contains(dtSQL, "CAST(RTRIM(oper_date) AS DATE) >= ?")
	// 上界是"次日 0 点",两个方言平移写法不同。
	hasUpper := strings.Contains(dtSQL, "< ? + INTERVAL 1 DAY") ||
		strings.Contains(dtSQL, "< DATEADD(day, 1, ?)")
	if !hasLower || !hasUpper {
		t.Errorf("datetime 列 onTheDate 应是半开区间 [当天, 次日),实际: %s", dtSQL)
	}
}

func TestDateOp_BeforeAndAfterAreExclusive(t *testing.T) {
	// cube 的 beforeDate/afterDate **不含当天**,这是最容易写错的语义。
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		Filters: []Filter{
			{Member: "sale_day.oper_date", Operator: "beforeDate", Values: []any{"2026-08-01"}},
			{Member: "sale_day.oper_date", Operator: "afterDate", Values: []any{"2026-07-01"}},
		},
	}
	sql := mustBuildWith(t, q, saleDaySchema()).SQL
	if !strings.Contains(sql, "RTRIM(oper_date) < ?") {
		t.Errorf("beforeDate 应是排他的 <,实际: %s", sql)
	}
	if !strings.Contains(sql, "RTRIM(oper_date) > ?") {
		t.Errorf("afterDate 应是排他的 >,实际: %s", sql)
	}
}

func TestDateOp_NotInDateRange(t *testing.T) {
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		Filters: []Filter{{
			Member:   "sale_day.oper_date",
			Operator: "notInDateRange",
			Values:   []any{"2026-07-01", "2026-07-31"},
		}},
	}
	sql := mustBuildWith(t, q, saleDaySchema()).SQL
	if !strings.Contains(sql, "NOT (") {
		t.Errorf("notInDateRange 应包一层 NOT,实际: %s", sql)
	}
}

func TestDateOp_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		op   string
		vals []any
	}{
		{"inDateRange 少一个值", "inDateRange", []any{"2026-07-01"}},
		{"inDateRange 三个值", "inDateRange", []any{"2026-07-01", "2026-08-01", "2026-09-01"}},
		{"inDateRange 非 ISO", "inDateRange", []any{"2026/07/01", "2026/09/01"}},
		{"inDateRange 区间反了", "inDateRange", []any{"2026-09-01", "2026-07-01"}},
		{"onTheDate 非 ISO", "onTheDate", []any{"20260815"}},
		{"onTheDate 是数字", "onTheDate", []any{20260815}},
		{"beforeDate 空值", "beforeDate", []any{}},
		{"afterDate 非字符串", "afterDate", []any{nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &Query{
				Measures: []string{"sale_day.sale_qty"},
				Filters:  []Filter{{Member: "sale_day.oper_date", Operator: c.op, Values: c.vals}},
			}
			// 错误日期若被放行,SQL 仍能跑但恒不匹配,表现为"那段时间没有业务" ——
			// 这类静默空集比报错难查得多。
			if _, err := Build(q, saleDaySchema()); err == nil {
				t.Fatalf("%s %v 应被拒绝", c.op, c.vals)
			}
		})
	}
}

// TestDateOp_DialectsUseOwnShiftSyntax 平移"次日"这一步两个方言写法不同,
// 串了就是运行期语法错。
func TestDateOp_DialectsUseOwnShiftSyntax(t *testing.T) {
	schema := saleDaySchema()
	schema.Dimensions[1].CharDate = false // 触发 DATEADD / INTERVAL 分支
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		Filters:  []Filter{{Member: "sale_day.oper_date", Operator: "onTheDate", Values: []any{"2026-08-15"}}},
	}
	duck := mustBuildWith(t, q, schema).SQL
	if !strings.Contains(duck, "INTERVAL 1 DAY") {
		t.Errorf("DuckDB 侧应出现 INTERVAL 1 DAY,实际: %s", duck)
	}
	if strings.Contains(duck, "DATEADD") {
		t.Errorf("DuckDB 不认 DATEADD: %s", duck)
	}

	live, err := BuildTSQL(q, schema, "t_rm_daysum", mustMapper(t, saleDayMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if !strings.Contains(live.SQL, "DATEADD(day, 1, @p2)") {
		t.Errorf("T-SQL 侧应出现 DATEADD(day, 1, @p2),实际: %s", live.SQL)
	}
	if strings.Contains(live.SQL, "INTERVAL") {
		t.Errorf("T-SQL 不认 INTERVAL: %s", live.SQL)
	}
	// 占位符编号必须连续:两个 ? → @p1/@p2。
	if len(live.Args) != 2 {
		t.Errorf("onTheDate(datetime) 应产生 2 个参数,got %#v", live.Args)
	}
}

// TestDateOp_CoexistsWithTimeDimensions 同一列被 filters 和 timeDimensions
// 同时引用时,参数下标必须仍然与 SQL 里的占位符顺序一致。
func TestDateOp_CoexistsWithTimeDimensions(t *testing.T) {
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		Filters: []Filter{
			{Member: "sale_day.branch_no", Operator: "equals", Values: []any{"0001"}},
			{Member: "sale_day.oper_date", Operator: "inDateRange", Values: []any{"2026-07-01", "2026-09-30"}},
		},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", Granularity: "month"}},
	}
	res := mustBuildWith(t, q, saleDaySchema())
	want := []any{"0001", "2026-07-01", "2026-09-30"}
	if len(res.Args) != len(want) {
		t.Fatalf("参数个数 = %d, want %d", len(res.Args), len(want))
	}
	for i := range want {
		if res.Args[i] != want[i] {
			t.Fatalf("Args[%d] = %v, want %v", i, res.Args[i], want[i])
		}
	}
}
