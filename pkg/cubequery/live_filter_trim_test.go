package cubequery

// live(T-SQL)路径的**过滤侧** RTRIM 策略守卫。
//
// # 为什么这条重要
//
// MSSQL 的 RTRIM 会把数字**隐式转成文本** ⇒ 数值列的过滤变成**文本比较**。
// 实测(2026-10-23,ysx 源库 t_rm_saleflow,条件 qty >= 2 AND qty < 3):
//
//	源库**数值**比较 = 82,950 行
//	源库**文本**比较 = 84,613 行
//	cube 过滤(恒包 RTRIM)= 73,026 行   ⇒ **漏 12%**
//
// ⇒ 任何"按数值区间筛"的查询都给出与源库不同的答案,而且**零报错**。
//
// ⇒ 这条测试锁住:number 维度的过滤**不包** RTRIM(数值比较),
// 其它类型(string / time)**照旧包**。

import (
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"
)

// numFilterSchema 有一个 number 维度(金额)与一个 string 维度。
func numFilterSchema() *cubeschema.Model {
	return &cubeschema.Model{
		Name:     "settlement",
		SQLTable: "settlement",
		Storage:  cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "amount_yuan", SQL: "amount_yuan", Type: cubeschema.TypeNumber},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
	}
}

func numFilterMapper(t *testing.T) *fieldmapping.Mapper {
	t.Helper()
	m, err := fieldmapping.Load([]byte(`
version: 1
model: settlement
mappings:
  - source: sheet_no
    target: id
    type: string
  - source: sheet_amt
    target: amount_yuan
    type: decimal
`))
	if err != nil {
		t.Fatalf("load mapping: %v", err)
	}
	return m
}

// numFilterWhere 编译一条过滤并抠出 WHERE 段。
func numFilterWhere(t *testing.T, member, op string, vals []any) string {
	t.Helper()
	lim := 200
	q := &Query{
		Measures:   []string{"settlement.count"},
		Dimensions: []string{"settlement.id"},
		Filters:    []Filter{{Member: member, Operator: op, Values: vals}},
		Limit:      &lim,
	}
	res, err := BuildTSQL(q, numFilterSchema(), "t_fm_recpay_gx_master", numFilterMapper(t))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	i := strings.Index(res.SQL, " WHERE ")
	if i < 0 {
		t.Fatalf("SQL 里没有 WHERE: %s", res.SQL)
	}
	j := strings.Index(res.SQL[i:], " GROUP BY ")
	if j < 0 {
		return res.SQL[i+7:]
	}
	return res.SQL[i+7 : i+j]
}

// TestLiveFilter_NumberDimensionComparesNumerically 锁"数值维度的过滤是数值比较"。
func TestLiveFilter_NumberDimensionComparesNumerically(t *testing.T) {
	where := numFilterWhere(t, "settlement.amount_yuan", "gte", []any{"5000"})

	if strings.Contains(where, "RTRIM") {
		t.Errorf("数值维度的过滤不能包 RTRIM(会把数字转文本 ⇒ 变成字典序比较):\n%s", where)
	}
	if !strings.Contains(where, "sheet_amt >= @p1") {
		t.Errorf("过滤必须直接用源列做数值比较:\n%s", where)
	}
}

// TestLiveFilter_StringDimensionStillTrims 锁"字符串维度的过滤仍然 RTRIM"。
//
// ⚠️ 思迅的 char 定长列带尾随空格 ⇒ 不 RTRIM 就匹配不上
// (" 801598" ≠ "801598")⇒ 这一条不能为了修数值比较而被顺手改掉。
func TestLiveFilter_StringDimensionStillTrims(t *testing.T) {
	where := numFilterWhere(t, "settlement.id", "equals", []any{"801598"})

	if !strings.Contains(where, "RTRIM(sheet_no)") {
		t.Errorf("字符串维度的过滤必须仍包 RTRIM(否则带尾随空格的 char 列匹配不上):\n%s", where)
	}
}

// TestLiveFilter_SelectSideStillTrims 锁"SELECT 侧仍然包 RTRIM"。
//
// ⚠️ 这是**故意**与过滤侧不同:返回值保持"明文文本"("1.0000"),
// 下游一个都不用改。若 SELECT 侧也不包,number 维度会变成 base64
// (驱动给 []byte)⇒ `cuberead.Str(r, "sale_detail.id")` 会拿到 base64 串
// ⇒ **列表去重失效**(不同行变成同一个 key ⇒ 只剩 1 行)。
func TestLiveFilter_SelectSideStillTrims(t *testing.T) {
	lim := 200
	q := &Query{
		Measures:   []string{"settlement.count"},
		Dimensions: []string{"settlement.amount_yuan"},
		Limit:      &lim,
	}
	res, err := BuildTSQL(q, numFilterSchema(), "t_fm_recpay_gx_master", numFilterMapper(t))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	if !strings.Contains(res.SQL, "RTRIM(sheet_amt) AS [settlement.amount_yuan]") {
		t.Errorf("SELECT 侧仍须包 RTRIM(返回值保持明文文本,下游零改动):\n%s", res.SQL)
	}
}
