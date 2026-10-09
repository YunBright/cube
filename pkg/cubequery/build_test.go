package cubequery

import (
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
)

func testSchema() *cubeschema.Model {
	return &cubeschema.Model{
		Name:     "product",
		SQLTable: "product",
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "name", SQL: "name", Type: cubeschema.TypeString},
			{Name: "unit", SQL: "unit", Type: cubeschema.TypeString},
			{Name: "created_at", SQL: "created_at", Type: cubeschema.TypeTime},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
			{Name: "avg_price_yuan", SQL: "AVG(price_yuan)", Type: cubeschema.TypeNumber},
		},
	}
}

func mustBuild(t *testing.T, q *Query) *Result {
	t.Helper()
	res, err := Build(q, testSchema())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	return res
}

// 回归锁:hbposv7 的旧实现**完全忽略 Filters**,带过滤的查询会退化成
// "SELECT 前 1000 行",而 supertrade 的 GetProduct 取 data[0]
// → 扫一个条码可能返回**另一个商品**的库存,200 OK 零报错。
// 这是"错答案",比"查不到"危险得多 —— 前者会被当成真实库存写进盘点单。
func TestBuild_EmitsWhereClause(t *testing.T) {
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Filters:    []Filter{{Member: "product.id", Operator: "equals", Values: []any{"6922303199721"}}},
	}
	res := mustBuild(t, q)

	if !strings.Contains(res.SQL, " WHERE ") {
		t.Fatalf("filter 必须出现在 SQL 里,实际: %s", res.SQL)
	}
	if len(res.Args) != 1 || res.Args[0] != "6922303199721" {
		t.Fatalf("filter 值必须以参数形式传给 QueryMap,got %#v", res.Args)
	}
	// 值不得被拼进 SQL 文本。
	if strings.Contains(res.SQL, "6922303199721") {
		t.Fatalf("filter 值不得拼进 SQL(注入面): %s", res.SQL)
	}
}

func TestBuild_MultipleMeasuresAndDimensions(t *testing.T) {
	q := &Query{
		Measures:   []string{"product.count", "product.avg_price_yuan"},
		Dimensions: []string{"product.id", "product.name", "product.unit"},
		Limit:      intPtr(10),
	}
	res := mustBuild(t, q)

	if len(res.MeasureRefs) != 2 {
		t.Fatalf("measures 应全部保留,got %#v", res.MeasureRefs)
	}
	if len(res.DimRefs) != 3 {
		t.Fatalf("dimensions 应全部保留,got %#v", res.DimRefs)
	}
	for _, want := range []string{`"product.id"`, `"product.name"`, `"product.unit"`,
		`"product.count"`, `"product.avg_price_yuan"`} {
		if !strings.Contains(res.SQL, want) {
			t.Fatalf("SELECT 缺少别名 %s,实际: %s", want, res.SQL)
		}
	}
	if !strings.Contains(res.SQL, "LIMIT 10") {
		t.Fatalf("limit 必须生效,实际: %s", res.SQL)
	}
	// GROUP BY 用裸列名(与 SELECT 的 RTRIM 表达式配对合法)。
	if !strings.Contains(res.SQL, "GROUP BY id, name, unit") {
		t.Fatalf("GROUP BY 应用原始列名,实际: %s", res.SQL)
	}
}

// 回归锁:思迅 char 定长列普遍补空格(实测 "6922303199721       "),
// 少一个 RTRIM 就让该门店条码搜索永远匹配不上,且零报错。
func TestBuild_TrimsSimpleColumnsOnBothSides(t *testing.T) {
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Filters:    []Filter{{Member: "product.id", Operator: "equals", Values: []any{"6922303199721"}}},
	}
	res := mustBuild(t, q)

	if !strings.Contains(res.SQL, `RTRIM(id) AS "product.id"`) {
		t.Fatalf("维度列必须 RTRIM,实际: %s", res.SQL)
	}
	if !strings.Contains(res.SQL, "WHERE RTRIM(id) = ?") {
		t.Fatalf("filter 列必须 RTRIM,实际: %s", res.SQL)
	}
}

// 聚合表达式绝不能被 RTRIM 包住(会破坏 AVG/COUNT 语义)。
func TestBuild_DoesNotTrimExpressions(t *testing.T) {
	q := &Query{Measures: []string{"product.avg_price_yuan"}, Dimensions: []string{"product.id"}}
	res := mustBuild(t, q)
	if strings.Contains(res.SQL, "RTRIM(AVG") || strings.Contains(res.SQL, "RTRIM(COUNT") {
		t.Fatalf("measure 聚合表达式不得被 RTRIM 包住,实际: %s", res.SQL)
	}
}

// 未指定 limit 时用默认值;<=0 视为未指定(而不是 LIMIT 0)。
func TestBuild_LimitFallback(t *testing.T) {
	q := &Query{Measures: []string{"product.count"}, Dimensions: []string{"product.id"}}
	if sql := mustBuild(t, q).SQL; !strings.Contains(sql, "LIMIT 1000") {
		t.Fatalf("未指定 limit 应回落默认值,实际: %s", sql)
	}
	q.Limit = intPtr(0)
	if sql := mustBuild(t, q).SQL; !strings.Contains(sql, "LIMIT 1000") {
		t.Fatalf("limit=0 应回落默认值而不是 LIMIT 0,实际: %s", sql)
	}
}

// 不支持的 operator 必须报错,不能被静默忽略(那等于查询条件没生效)。
func TestBuild_RejectsUnknownOperator(t *testing.T) {
	q := &Query{
		Measures: []string{"product.count"},
		Filters:  []Filter{{Member: "product.id", Operator: "regexMatch", Values: []any{"x"}}},
	}
	if _, err := Build(q, testSchema()); err == nil {
		t.Fatal("未知 operator 必须报错 —— 静默忽略会让 WHERE 消失,查询条件完全失效")
	}
}

// contains 必须是**两侧**通配 %v%(子串匹配),不是前缀 —— ysx 另有 startsWith
// 才是前缀。这条是抽共用包时最容易改错的语义。
func TestBuild_ContainsIsSubstringNotPrefix(t *testing.T) {
	q := &Query{
		Measures: []string{"product.count"},
		Filters:  []Filter{{Member: "product.name", Operator: "contains", Values: []any{"抽纸"}}},
	}
	res := mustBuild(t, q)
	arg, _ := res.Args[0].(string)
	if arg != "%抽纸%" {
		t.Fatalf("contains 应生成 %%v%%,got %q", arg)
	}
	if !strings.Contains(res.SQL, "LIKE ?") {
		t.Fatalf("contains 应编译成 LIKE,实际: %s", res.SQL)
	}

	q2 := &Query{
		Measures: []string{"product.count"},
		Filters:  []Filter{{Member: "product.name", Operator: "startsWith", Values: []any{"唯得"}}},
	}
	arg2, _ := mustBuild(t, q2).Args[0].(string)
	if arg2 != "唯得%" {
		t.Fatalf("startsWith 应生成 v%%,got %q", arg2)
	}
}

// LIKE 的 % _ \ 必须转义,否则用户输入的通配符会改变匹配语义。
func TestBuild_ContainsEscapesWildcards(t *testing.T) {
	q := &Query{
		Measures: []string{"product.count"},
		Filters:  []Filter{{Member: "product.name", Operator: "contains", Values: []any{"50%_x"}}},
	}
	res := mustBuild(t, q)
	arg, _ := res.Args[0].(string)
	if !strings.HasPrefix(arg, `%50\%\_x%`) {
		t.Fatalf("LIKE 通配符必须转义,got %q", arg)
	}
}

// 无 measure / 无 model / 未知 member 都必须显式报错。
func TestBuild_RejectsInvalidQueries(t *testing.T) {
	cases := []struct {
		name string
		q    *Query
	}{
		{"无 measure", &Query{Dimensions: []string{"product.id"}}},
		{"无 model 前缀", &Query{Measures: []string{"count"}}},
		{"未知 dimension", &Query{Measures: []string{"product.count"}, Dimensions: []string{"product.nope"}}},
		{"未知 measure", &Query{Measures: []string{"product.nope"}}},
		{"未知 filter member", &Query{Measures: []string{"product.count"},
			Filters: []Filter{{Member: "product.nope", Operator: "equals", Values: []any{"x"}}}}},
	}
	for _, c := range cases {
		if _, err := Build(c.q, testSchema()); err == nil {
			t.Fatalf("%s: 必须报错而不是静默返回空结果", c.name)
		}
	}
}

// 多个 filter 必须 AND 连接,不能只取第一个。
func TestBuild_CombinesMultipleFiltersWithAnd(t *testing.T) {
	q := &Query{
		Measures: []string{"product.count"},
		Filters: []Filter{
			{Member: "product.id", Operator: "equals", Values: []any{"A"}},
			{Member: "product.name", Operator: "contains", Values: []any{"纸"}},
		},
	}
	res := mustBuild(t, q)
	if !strings.Contains(res.SQL, " AND ") {
		t.Fatalf("多个 filter 必须 AND 连接,实际: %s", res.SQL)
	}
	if len(res.Args) != 2 {
		t.Fatalf("参数个数必须与 filter 数一致,got %d: %#v", len(res.Args), res.Args)
	}
}

func intPtr(v int) *int { return &v }
