// segment_test.go 锁 segments(预定义过滤片段)。
//
// 它解决的是"同一套判定条件被多处复用却各写一遍":企业微信审批里那套
// 异常判据会被机器人、BI 报表、回归脚本反复用。
package cubequery

import (
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
)

func segmentSchema() *cubeschema.Model {
	m := saleDaySchema()
	m.Segments = []cubeschema.Segment{
		{Name: "recent", SQL: "oper_date >= '2026-07-01'"},
		{Name: "empty_qty", SQL: "sale_qty <= 0"},
	}
	return m
}

func TestSegment_ExpandsIntoWhere(t *testing.T) {
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		Segments: []string{"recent"},
	}
	sql := mustBuildWith(t, q, segmentSchema()).SQL
	if !strings.Contains(sql, " WHERE (oper_date >= '2026-07-01')") {
		t.Errorf("segment 应展开成 WHERE 片段,实际: %s", sql)
	}
}

func TestSegment_CombinesWithFiltersAndTimeDimensions(t *testing.T) {
	q := &Query{
		Measures:   []string{"sale_day.sale_qty"},
		Dimensions: []string{"sale_day.item_id"},
		Filters: []Filter{
			{Member: "sale_day.branch_no", Operator: "equals", Values: []any{"0001"}},
		},
		Segments:       []string{"recent"},
		TimeDimensions: []TimeDim{{Dimension: "sale_day.oper_date", Granularity: "month"}},
	}
	res := mustBuildWith(t, q, segmentSchema()).SQL
	for _, want := range []string{" WHERE ", "(oper_date >= '2026-07-01')", "GROUP BY", "RTRIM(branch_no) = ?"} {
		if !strings.Contains(res, want) {
			t.Errorf("应同时包含 %q,实际: %s", want, res)
		}
	}
}

func TestSegment_UnknownNameIsRejectedWithAvailableList(t *testing.T) {
	q := &Query{
		Measures: []string{"sale_day.sale_qty"},
		Segments: []string{"nonexistent"},
	}
	_, err := Build(q, segmentSchema())
	var be *BuildError
	if !errors.As(err, &be) || be.Kind != "query" {
		t.Fatalf("未知 segment 应报 Kind=query,got %v", err)
	}
	// 错误信息里要列出可用的名字 —— 调用方得能自己改对。
	if !strings.Contains(be.Msg, "recent") || !strings.Contains(be.Msg, "empty_qty") {
		t.Errorf("错误信息应列出可用 segment,got: %s", be.Msg)
	}
}

// TestSegment_TSQLRewritesCanonicalColumns 是透传路径的老坑:
// segment 的 SQL 来自 schema(写的是 canonical 名),发给源库前必须 rewrite,
// 否则就是 column not found。mapping parity 测试当初抓到的 sale_detail
// order_status 就是这一类。
func TestSegment_TSQLRewritesCanonicalColumns(t *testing.T) {
	m := mustMapper(t, `
version: 1
model: sale_day
mappings:
  - source: oper_date
    target: oper_day
    type: string
`)
	schema := segmentSchema()
	schema.Dimensions[1].SQL = "oper_day" // canonical 名
	schema.Segments[0].SQL = "oper_day >= '2026-07-01'"
	q := &Query{Measures: []string{"sale_day.sale_qty"}, Segments: []string{"recent"}}
	res, err := BuildTSQL(q, schema, "t_rm_daysum", m)
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if !strings.Contains(res.SQL, "oper_date >= '2026-07-01'") {
		t.Errorf("segment 的列名应被解析成源列名,实际: %s", res.SQL)
	}
	if strings.Contains(res.SQL, "oper_day") {
		t.Errorf("canonical 名漏进了发给源库的 SQL: %s", res.SQL)
	}
}

func TestSegment_SchemaRejectsSubquery(t *testing.T) {
	// rewriteColumns 是**标识符级盲替换**,子查询里属于另一张表的同名列会被
	// 一起改掉 —— 碰巧对上就是对的,对不上就是错的,而且不报错。
	// 所以 schema 加载时就必须拒:segment 只能是"本表上的一个谓词"。
	yamlDoc := `
name: sale_day
sql_table: sale_day
storage: live
dimensions:
  - name: item_id
    sql: item_id
    type: string
measures:
  - name: sale_qty
    sql: SUM(sale_qty)
    type: number
segments:
  - name: cross_table
    sql: "item_id IN (SELECT item_id FROM t_bd_item_info)"
`
	if _, err := cubeschema.Load([]byte(yamlDoc)); err == nil {
		t.Fatal("segment 含子查询时应在 schema 加载阶段被拒绝(列名盲替换会误伤)")
	} else if !strings.Contains(err.Error(), "子查询") {
		t.Errorf("错误信息要说清是子查询的问题,got: %v", err)
	}
}

func TestSegment_SchemaRejectsPlaceholder(t *testing.T) {
	// segment 不携带参数,里面写 `?` 会变成无人绑定的占位符。
	yamlDoc := `
name: sale_day
sql_table: sale_day
dimensions:
  - name: item_id
    sql: item_id
    type: string
measures:
  - name: sale_qty
    sql: SUM(sale_qty)
    type: number
segments:
  - name: has_param
    sql: "item_id = ?"
`
	if _, err := cubeschema.Load([]byte(yamlDoc)); err == nil {
		t.Fatal("segment 含 `?` 时应在 schema 加载阶段被拒绝")
	}
}
