// time_test.go 锁 timeDimensions(dateRange + granularity)语义。
//
// 这个特性的存在理由是业务闭环:"采购量 vs **近期**销售差异大"。
// 在它之前,cube 完全没有时间过滤能力 —— 调用方传了日期窗,cube 静默忽略,
// 返回**全历史**数据,200 OK 零报错。那是错答案,不是缺功能。
//
// 本文件的核心断言:不支持的写法**必须报错**。
// 任何"看不懂就当没这回事"的分支,都会退化成上一行说的那个错答案。
package cubequery

import (
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// saleDaySchema 是 sale_day 的近似形态:oper_date 是 **char(10) 'YYYY-MM-DD'**
// (实测 t_rm_daysum 就是这样,不是 datetime),且维表里很多列 char 定长补空格。
func saleDaySchema() *cubeschema.Model {
	return &cubeschema.Model{
		Name:     "sale_day",
		SQLTable: "sale_day",
		Storage:  cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "item_id", SQL: "item_id", Type: cubeschema.TypeString},
			{Name: "oper_date", SQL: "oper_date", Type: cubeschema.TypeTime, CharDate: true},
			{Name: "branch_no", SQL: "branch_no", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
			{Name: "sale_qty", SQL: "SUM(sale_qty)", Type: cubeschema.TypeNumber},
		},
	}
}

const saleDayMapping = `
version: 1
model: sale_day
mappings:
  - source: item_no
    target: item_id
    type: string
  - source: oper_date
    target: oper_date
    type: string
  - source: branch_no
    target: branch_no
    type: string
  - source: sale_qty
    target: sale_qty
    type: number
`

// TestTime_CharDateRangeIsStringComparison 锁最关键的性能与正确性取舍:
// char_date 列直接对**原列**做字符串比较 —— sargable,SQL Server 能走索引。
// 若哪天改成 CAST(oper_date AS DATETIME) >= ?,这一条会红(性能悄悄退化)。
func TestTime_CharDateRangeIsStringComparison(t *testing.T) {
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", DateRange: []any{"2026-07-01", "2026-09-30"}}},
	}
	res := mustBuildWith(t, q, saleDaySchema())

	if !strings.Contains(res.SQL, "RTRIM(oper_date) >= ?") {
		t.Errorf("char_date 应直接对 RTRIM 原列比较(可用索引),实际: %s", res.SQL)
	}
	if strings.Contains(res.SQL, "CAST(oper_date AS DATE) >=") {
		t.Errorf("char_date 列被 CAST 了,时间过滤会退化成全表扫描: %s", res.SQL)
	}
	if len(res.Args) != 2 || res.Args[0] != "2026-07-01" || res.Args[1] != "2026-09-30" {
		t.Fatalf("日期必须以参数绑定,got %#v", res.Args)
	}
	if strings.Contains(res.SQL, "2026-07-01") {
		t.Errorf("日期值不得拼进 SQL 文本: %s", res.SQL)
	}
}

// TestTime_RealDateColumnGetsCast 反向锁:没标 char_date 的**真 datetime 列**
// 必须 CAST,否则字符串比较 datetime 在两个方言里都是错的。
func TestTime_RealDateColumnGetsCast(t *testing.T) {
	schema := saleDaySchema()
	schema.Dimensions[1].CharDate = false // 换成真 datetime 列
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", DateRange: []any{"2026-07-01", "2026-09-30"}}},
	}
	res := mustBuildWith(t, q, schema)
	if !strings.Contains(res.SQL, "CAST(RTRIM(oper_date) AS DATE) >= ?") {
		t.Errorf("非 char_date 列必须 CAST 后比较,实际: %s", res.SQL)
	}
}

// TestTime_GranularityBecomesGroupedDimension granularity 不是过滤,
// 是"按月/按周分组",必须同时出现在 SELECT 和 GROUP BY,别名带粒度后缀。
func TestTime_GranularityBecomesGroupedDimension(t *testing.T) {
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", Granularity: "month"}},
	}
	res := mustBuildWith(t, q, saleDaySchema())

	if !strings.Contains(res.SQL, `DATE_TRUNC('month', CAST(RTRIM(oper_date) AS DATE)) AS "sale_day.oper_date.month"`) {
		t.Errorf("应按月分组并用 <model>.<dim>.<gran> 作别名,实际: %s", res.SQL)
	}
	if !strings.Contains(res.SQL, "GROUP BY DATE_TRUNC('month'") {
		t.Errorf("分组表达式必须同时进 GROUP BY,实际: %s", res.SQL)
	}
	if len(res.DimRefs) != 1 || res.DimRefs[0] != "oper_date.month" {
		t.Errorf("输出 ref 应是 oper_date.month,got %#v", res.DimRefs)
	}
}

// TestTime_DateRangeAndGranularityTogether 两者同时给的组合场景:
// "近 90 天按月汇总" —— 这是业务侧真正会发的形状。
func TestTime_DateRangeAndGranularityTogether(t *testing.T) {
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{
			Dimension:   "sale_day.oper_date",
			DateRange:   []any{"2026-07-01", "2026-09-30"},
			Granularity: "month",
		}},
	}
	res := mustBuildWith(t, q, saleDaySchema())

	hasWhere := strings.Contains(res.SQL, " WHERE ")
	hasGroup := strings.Contains(res.SQL, " GROUP BY ")
	hasLimit := strings.Contains(res.SQL, " LIMIT ")
	if !hasWhere || !hasGroup || !hasLimit {
		t.Errorf("应同时有 WHERE/GROUP BY/LIMIT,实际: %s", res.SQL)
	}
	if len(res.Args) != 2 {
		t.Errorf("应有 2 个日期参数,got %#v", res.Args)
	}
}

// TestTime_ArgOrderMatchesWhereOrder 是最容易静默出错的一处:
// 参数按位置绑定,若 res.Args 的顺序与 SQL 里 ? 出现的顺序不一致,
// 过滤值会串到别的列上 —— 而 SQL 依然能跑,结果是错的。
func TestTime_ArgOrderMatchesWhereOrder(t *testing.T) {
	q := &Query{
		Measures:   []string{"sale_day.sale_qty"},
		Dimensions: []string{"sale_day.item_id"},
		Filters: []Filter{
			{Member: "sale_day.branch_no", Operator: "equals", Values: []any{"0001"}},
		},
		TimeDimensions: []TimeDim{{
			Dimension: "sale_day.oper_date",
			DateRange: []any{"2026-07-01", "2026-09-30"},
		}},
	}
	res := mustBuildWith(t, q, saleDaySchema())

	// filters 的参数在前,timeDimensions 的在后 —— 与 build 顺序一致。
	want := []any{"0001", "2026-07-01", "2026-09-30"}
	if len(res.Args) != len(want) {
		t.Fatalf("参数个数 = %d, want %d (args=%#v)", len(res.Args), len(want), res.Args)
	}
	for i := range want {
		if res.Args[i] != want[i] {
			t.Fatalf("Args[%d] = %v, want %v(顺序错了会让过滤值串列)", i, res.Args[i], want[i])
		}
	}
}

// TestTime_RejectsUnknownDateRange 是**本文件最重要的一条**。
// 解析不了的 dateRange 必须报错:静默忽略等于"查了全历史却以为是近期"。
func TestTime_RejectsUnknownDateRange(t *testing.T) {
	bad := []any{
		"last_3_business_days", // 不在白名单的相对关键字
		"2026/07/01",           // 非 ISO 格式
		"not-a-date",
		[]any{"2026-07-01", "2026-09-30", "2026-10-01"}, // 长度 3
		[]any{"2026-09-30", "2026-07-01"},               // 起点晚于终点
		[]any{1751328000, 1756684800},                   // Unix 时间戳
		[]any{},                                         // 空数组
		12345,                                           // 纯数字
	}
	for _, v := range bad {
		q := &Query{
			Measures:       []string{"sale_day.sale_qty"},
			TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", DateRange: v}},
		}
		_, err := Build(q, saleDaySchema())
		var be *BuildError
		if !errors.As(err, &be) || be.Kind != "filter" {
			t.Errorf("dateRange=%#v 必须被拒且 Kind=filter,got err=%v", v, err)
		}
	}
}

// TestTime_RejectsNonTimeDimension 把普通维度塞进 timeDimensions 是调用方的错,
// 要给出可执行的提示(改 type: time,或用普通 dimensions + filter)。
func TestTime_RejectsNonTimeDimension(t *testing.T) {
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.branch_no", DateRange: []any{"2026-07-01", "2026-07-02"}}},
	}
	_, err := Build(q, saleDaySchema())
	var be *BuildError
	if !errors.As(err, &be) || be.Kind != "dimension" {
		t.Fatalf("非 time 维度进 timeDimensions 应报 Kind=dimension,got %v", err)
	}
	if !strings.Contains(be.Msg, "type: time") {
		t.Errorf("错误信息要告诉调用方怎么改,got: %s", be.Msg)
	}
}

// TestTime_RejectsUnknownGranularity 粒度是白名单 —— 不认识的一律拒。
func TestTime_RejectsUnknownGranularity(t *testing.T) {
	for _, g := range []string{"hour", "minute", "fortnight", ""} {
		if g == "" {
			continue
		}
		q := &Query{
			Measures:       []string{"sale_day.sale_qty"},
			TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", Granularity: g}},
		}
		if _, err := Build(q, saleDaySchema()); err == nil {
			t.Errorf("granularity=%q 应被拒绝", g)
		}
	}
}

// TestTime_RelativeRangeHasNoPlaceholders 相对区间展开成 SQL 表达式,
// 不产生占位符 —— 关键字是白名单,不含用户文本,没有注入面。
func TestTime_RelativeRangeHasNoPlaceholders(t *testing.T) {
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", DateRange: "last_30_days"}},
	}
	res := mustBuildWith(t, q, saleDaySchema())
	if strings.Contains(res.SQL, "?") {
		t.Errorf("相对区间不应产生占位符,实际: %s", res.SQL)
	}
	if len(res.Args) != 0 {
		t.Errorf("相对区间不应带参数,got %#v", res.Args)
	}
	if !strings.Contains(res.SQL, "CURRENT_DATE") {
		t.Errorf("DuckDB 侧应出现 CURRENT_DATE,实际: %s", res.SQL)
	}
	if strings.Contains(res.SQL, "DATEADD") {
		t.Errorf("DuckDB 没有 DATEADD —— 方言串了,这会在运行时报语法错: %s", res.SQL)
	}
}

// TestTime_TSQLUsesTsqlDateFunctions 反向锁:DuckDB 的 INTERVAL 也不能
// 出现在 T-SQL 侧。两边互为对方的"看起来对、运行即错"陷阱。
func TestTime_TSQLUsesTsqlDateFunctions(t *testing.T) {
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", DateRange: "last_30_days"}},
	}
	res, err := BuildTSQL(q, saleDaySchema(), "t_rm_daysum", mustMapper(t, saleDayMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	if !strings.Contains(sql, "GETDATE()") {
		t.Errorf("T-SQL 侧应出现 GETDATE(),实际: %s", sql)
	}
	if !strings.Contains(sql, "DATEADD(day, -29,") {
		t.Errorf("T-SQL 侧应出现 DATEADD(day, -29, ...),实际: %s", sql)
	}
	if strings.Contains(sql, "INTERVAL") {
		t.Errorf("INTERVAL 是 DuckDB 语法,T-SQL 不认: %s", sql)
	}
	if strings.Contains(sql, "DATE_TRUNC") {
		t.Errorf("DATE_TRUNC 是 DuckDB 语法,T-SQL 不认: %s", sql)
	}
}

// TestTime_TSQLGranularityUsesTsqlFunctions 粒度截断同样两套写法。
func TestTime_TSQLGranularityUsesTsqlFunctions(t *testing.T) {
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", Granularity: "month"}},
	}
	res, err := BuildTSQL(q, saleDaySchema(), "t_rm_daysum", mustMapper(t, saleDayMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	if !strings.Contains(sql, "DATEFROMPARTS(YEAR(") || !strings.Contains(sql, "AS [sale_day.oper_date.month]") {
		t.Errorf("T-SQL 应按月截断且别名用方括号,实际: %s", sql)
	}
	if strings.Contains(sql, "DATE_TRUNC") {
		t.Errorf("DATE_TRUNC 是 DuckDB 语法,T-SQL 不认: %s", sql)
	}
}

// TestTime_TSQLResolvesDateColumnToSourceColumn 透传路径的老规矩:
// schema 写 canonical 名,发往源库的必须是该 family 的真实列名。
func TestTime_TSQLResolvesDateColumnToSourceColumn(t *testing.T) {
	m := mustMapper(t, `
version: 1
model: sale_day
mappings:
  - source: oper_date
    target: oper_day
    type: string
`)
	schema := saleDaySchema()
	schema.Dimensions[1].SQL = "oper_day" // canonical 名与源列名故意不同
	q := &Query{
		Measures:       []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", DateRange: []any{"2026-07-01", "2026-07-31"}}},
	}
	res, err := BuildTSQL(q, schema, "t_rm_daysum", m)
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if !strings.Contains(res.SQL, "RTRIM(oper_date) >=") {
		t.Errorf("应解析成源列名 oper_date,实际: %s", res.SQL)
	}
	if strings.Contains(res.SQL, "RTRIM(oper_day)") {
		t.Errorf("canonical 名漏进了发给源库的 SQL: %s", res.SQL)
	}
}

// TestTime_DialectsAgree 时间维度也必须两路一致(同 buildOrder 的理由)。
func TestTime_DialectsAgree(t *testing.T) {
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		TimeDimensions: []TimeDim{{
			Dimension:   "sale_day.oper_date",
			DateRange:   []any{"2026-07-01", "2026-09-30"},
			Granularity: "month",
		}},
		Order: []Order{{ID: "sale_day.oper_date.month", Order: "desc"}},
	}
	duck := mustBuildWith(t, q, saleDaySchema()).SQL
	live, err := BuildTSQL(q, saleDaySchema(), "t_rm_daysum", mustMapper(t, saleDayMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	// 结构上的三个要点在两边必须都在:过滤 / 分组 / 排序。
	for _, want := range []string{" WHERE ", " GROUP BY ", " ORDER BY "} {
		if !strings.Contains(duck, want) || !strings.Contains(live.SQL, want) {
			t.Errorf("两条路径都该有 %q\n duck: %s\n live: %s", want, duck, live.SQL)
		}
	}
	if len(live.Args) != 2 {
		t.Errorf("两边参数个数应一致,live got %#v", live.Args)
	}
}
