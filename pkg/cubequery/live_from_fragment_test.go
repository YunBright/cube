// 本文件锁「live model 的源是**SQL 片段**(派生表)时」的行为。
//
// # 为什么需要片段
//
// 有些列**在源库里不存在**,但能在源库侧算出来:
// 会员卡内余额 = Σ充值 − Σ储值支付(要 JOIN 三张流水表)。
// live 路没有"先把表物化下来再算"这一步 ⇒ 这类列必须由**片段**在源库侧算。
//
// # 这条测试守什么
//
//  1. 片段必须被包成**带别名的派生表** —— T-SQL 的派生表强制要求别名,
//     写成 `FROM (SELECT …)` 直接报 "An object or column name is missing or empty"。
//  2. canonical 列名照常被重写成**源列名** —— 片段的列名就是源列名。
//  3. **普通表名不受影响** —— 单表查询的 SQL 必须逐字不变。
package cubequery

import (
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// memberLikeSchema 是"派生列在源库侧算"的 model 的最小形态。
func memberLikeSchema() *cubeschema.Model {
	return &cubeschema.Model{
		Name:     "member",
		SQLTable: "member",
		Storage:  cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "balance_yuan", SQL: "balance_yuan", Type: cubeschema.TypeNumber},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
	}
}

const memberLikeMapping = `
version: 1
model: member
mappings:
  - source: card_id
    target: id
    type: string
  - source: memb_balance
    target: balance_yuan
    type: decimal
`

// TestLive_FromFragmentBecomesAliasedDerivedTable 锁"片段 ⇒ 带别名的派生表"。
func TestLive_FromFragmentBecomesAliasedDerivedTable(t *testing.T) {
	lim := 50
	fragment := "(SELECT v.*, ISNULL(p.total, 0) AS memb_balance FROM t_rm_vip_info v)"
	q := &Query{
		Measures:   []string{"member.count"},
		Dimensions: []string{"member.id", "member.balance_yuan"},
		Limit:      &lim,
	}
	res, err := BuildTSQL(q, memberLikeSchema(), fragment, mustMapper(t, memberLikeMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL

	// ① 片段必须带别名。
	if !strings.Contains(sql, "FROM (SELECT v.*, ISNULL(p.total, 0) AS memb_balance FROM t_rm_vip_info v) AS t0") {
		t.Errorf("片段必须被包成带别名的派生表(否则 T-SQL 报 'An object or column name is missing or empty'):\n%s", sql)
	}
	// ② canonical 名照常重写成源列名。
	if !strings.Contains(sql, "RTRIM(card_id) AS [member.id]") {
		t.Errorf("canonical 列名必须重写成源列名 card_id:\n%s", sql)
	}
	// ③ 派生列(memb_balance)也在片段里,编译器把它重写成源列名。
	if !strings.Contains(sql, "memb_balance") {
		t.Errorf("派生列必须落到片段提供的列名上:\n%s", sql)
	}
	// ④ 度量的聚合函数名**不能**被打上表别名(t0.COUNT(*) 是非法 SQL)。
	if strings.Contains(sql, "t0.COUNT(") {
		t.Errorf("聚合函数名不能带表别名:\n%s", sql)
	}
}

// TestLive_PlainTableNameIsUnchanged 锁"普通表名不受片段逻辑影响"。
//
// ⚠️ 这是"加片段支持"最容易踩的坑:顺手给所有 FROM 都加了别名,
// 于是**单表查询的 SQL 逐字变了** —— 而单表那条路有几十个既有测试在比对。
func TestLive_PlainTableNameIsUnchanged(t *testing.T) {
	lim := 50
	q := &Query{
		Measures:   []string{"member.count"},
		Dimensions: []string{"member.id"},
		Limit:      &lim,
	}
	res, err := BuildTSQL(q, memberLikeSchema(), "t_rm_vip_info", mustMapper(t, memberLikeMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	if strings.Contains(sql, "AS t0") {
		t.Errorf("普通表名不该被加别名(单表 SQL 必须逐字不变):\n%s", sql)
	}
	if !strings.Contains(sql, "FROM t_rm_vip_info") {
		t.Errorf("FROM 必须是原表名:\n%s", sql)
	}
}
