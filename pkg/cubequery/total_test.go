// total_test.go 锁 `total: true` 的行为。
//
// 它的存在理由:没有总行数,前端只能靠"取满一页再翻"猜有没有下一页。
// 结算单列表(ysx 4348 张 / hbposv7 799 张)必须能告诉用户"共 4348 条,当前第 1 页"。
package cubequery

import (
	"strings"
	"testing"
)

// TestTotal_AddsWindowFunctionToBothDialects 总数用 window function 一次算完,
// 不额外打第二次查询 —— live model 直连思迅生产库,少一次往返就是少一次压力。
func TestTotal_AddsWindowFunctionToBothDialects(t *testing.T) {
	q := &Query{
		Measures:   []string{"sale_day.sale_qty"},
		Dimensions: []string{"sale_day.item_id"},
		Total:      true,
	}
	duck := mustBuildWith(t, q, saleDaySchema()).SQL
	if !strings.Contains(duck, `COUNT(*) OVER() AS "`+TotalField+`"`) {
		t.Errorf("DuckDB 侧应注入 COUNT(*) OVER(),实际: %s", duck)
	}
	live, err := BuildTSQL(q, saleDaySchema(), "t_rm_daysum", mustMapper(t, saleDayMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if !strings.Contains(live.SQL, "COUNT(*) OVER() AS ["+TotalField+"]") {
		t.Errorf("T-SQL 侧应注入 COUNT(*) OVER(),实际: %s", live.SQL)
	}
}

// TestTotal_NotRequestedMeansNoColumn 没请求 total 就不该多查一列 ——
// 它是每行都重复的值,白白撑大响应体。
func TestTotal_NotRequestedMeansNoColumn(t *testing.T) {
	q := &Query{Measures: []string{"sale_day.sale_qty"}, Dimensions: []string{"sale_day.item_id"}}
	for _, sql := range []string{mustBuildWith(t, q, saleDaySchema()).SQL} {
		if strings.Contains(sql, TotalField) {
			t.Errorf("未请求 total 时不应出现 %s 列: %s", TotalField, sql)
		}
	}
}

// TestExtractTotal_LiftsAndRemovesColumn 关键行为:总数被**提出来**,
// 而且必须从**每一行**里删掉 —— 只删第一行的话,后面的行还带着这列。
func TestExtractTotal_LiftsAndRemovesColumn(t *testing.T) {
	rows := []map[string]any{
		{"sale_day.item_id": "A", TotalField: int64(4348)},
		{"sale_day.item_id": "B", TotalField: int64(4348)},
	}
	total, out := ExtractTotal(rows, true)
	if total != 4348 {
		t.Fatalf("total = %d, want 4348", total)
	}
	if len(out) != 2 {
		t.Fatalf("行数不该变,got %d", len(out))
	}
	for i, r := range out {
		if _, ok := r[TotalField]; ok {
			t.Errorf("第 %d 行仍带着 %s 列", i, TotalField)
		}
	}
	if out[0]["sale_day.item_id"] != "A" {
		t.Errorf("业务列不该被动过,got %#v", out[0])
	}
}

// TestExtractTotal_NormalizesDriverTypes go-mssqldb 与 duckdb 驱动返回的
// 数值类型不一致(这在 source.ScanRows 上已经踩过一次:数值被当 []byte 打印)。
// total 必须跨类型都归一,否则会返回 "[52 49 51 50]" 这种东西。
func TestExtractTotal_NormalizesDriverTypes(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want int
	}{
		{"int64", int64(1208), 1208},
		{"int", 1208, 1208},
		{"float64", float64(1208), 1208},
		{"[]byte", []byte("1208"), 1208},
		{"string", "1208", 1208},
		{"无法解析", []byte("abc"), 0},
		{"未知类型", struct{}{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := []map[string]any{{TotalField: c.v}}
			total, _ := ExtractTotal(rows, true)
			if total != c.want {
				t.Errorf("%v → total=%d, want %d", c.v, total, c.want)
			}
		})
	}
}

// TestExtractTotal_EmptyResultIsZero 空结果集的总行数是 **0**,不是"未知"。
// 这是"查了,确实没有",与"查不了"必须分得开。
func TestExtractTotal_EmptyResultIsZero(t *testing.T) {
	total, rows := ExtractTotal(nil, true)
	if total != 0 {
		t.Errorf("空结果集的 total 应为 0,got %d", total)
	}
	if rows != nil {
		t.Errorf("空结果集应原样返回 nil,got %#v", rows)
	}
}

// TestExtractTotal_NotRequestedIsNoOp 没请求 total 时不做任何遍历,
// 也不假装给出总数(0 在这里是"没问",不是"一共 0 条")。
func TestExtractTotal_NotRequestedIsNoOp(t *testing.T) {
	rows := []map[string]any{{"sale_day.item_id": "A"}}
	total, out := ExtractTotal(rows, false)
	if total != 0 {
		t.Errorf("未请求时 total 应为 0(表示没问),got %d", total)
	}
	if _, ok := out[0]["sale_day.item_id"]; !ok {
		t.Errorf("未请求时不该动 rows")
	}
}
