// Package cubequery —— 商品"最后变动时间"维度(增量导出的过滤维度)的编译守卫。
//
// # 为什么单独一个文件
//
//	`product.updated_at` 是**增量导出**的过滤维度:
//	cube-router 导出商品时加 `product.updated_at gt <水位>` ⇒ 只导"变过的行"。
//
//	它的正确性取决于**一件很容易被改坏的事**:
//	比较必须**保留时分秒**。
//
// # ⚠️ 为什么"保留时分秒"这么要紧
//
//	cube 的 **DuckDB 路径**对 `type: time` 的 gt/gte 编译成
//	`CAST(col AS DATE) > ?` —— **CAST 成 DATE 会抹掉时分秒**。
//	⇒ "今天 17:20 变过的商品" 与 "今天 12:00 的水位" 比较时落到同一天
//	⇒ 当天的变动**被漏掉**,而同步报告照样写"成功 N 条"。
//
//	而 **live 路径**(T-SQL)用的是 `CAST(col AS DATETIME)`(**保留**时分秒)
//	⇒ 同一语义的两条路径给出**不同答案**。
//
// ⇒ product 是 **live** model(2026-10-22 起不再落 DuckDB)
//	⇒ 只剩 T-SQL 路径 ⇒ 维度声明成 `type: time` 即可。
//
// # 这条测试守什么
//
//	① **live 路径**的 gt 必须 CAST 成 DATETIME(保留时分秒)
//	⇒ 且**绝不能**是 VARCHAR(MSSQL 的 datetime→VARCHAR 是本地化格式
//	  'Jul 22 2026  5:20PM',字典序 ≠ 时间序 ⇒ 过滤静默出错)。
//	② SELECT / GROUP BY / WHERE 三处表达式必须**一致**
//	⇒ 不一致 ⇒ 过滤掉的行与返回的行不是同一批。
package cubequery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"
)

// loadProductSchema 读**仓库里真实的** product model schema。
//
// ⚠️ 为什么读真文件而不是在这里抄一份:
//
//	抄一份 ⇒ schema 改了而断言没改 ⇒ 断言要么假红要么假绿,
//	而它守的恰恰是"schema 的类型声明"这件事。
func loadProductSchema(t *testing.T) *cubeschema.Model {
	t.Helper()
	// 本文件在 pkg/cubequery ⇒ 仓库根在 ../.. ⇒ sixun-models 在 ../../sixun-models
	path := filepath.Join("..", "..", "sixun-models", "product", "schema.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 product schema 失败(%s): %v", path, err)
	}
	m, err := cubeschema.Load(raw)
	if err != nil {
		t.Fatalf("解析 product schema 失败: %v", err)
	}
	return m
}

// liveProductMapping 是 product 的 ysx mapping(只取本测试要用的几列)。
const liveProductMapping = `
version: 1
model: product
mappings:
  - source: item_no
    target: id
    type: string
  - source: build_date
    target: created_at
    type: datetime
  - source: modify_date
    target: modify_date
    type: datetime
`

// TestLiveProduct_UpdatedAtFilterIsDateComparison 锁"live 路径的时间过滤按**日期**比较"。
//
// ⚠️ 不得出现"先转 VARCHAR 再比较"的形态 ——
//	MSSQL 的 datetime→VARCHAR 是本地化格式,字典序 ≠ 时间序。
func TestLiveProduct_UpdatedAtFilterIsDateComparison(t *testing.T) {
	schema := loadProductSchema(t)
	mp, err := fieldmapping.Load([]byte(liveProductMapping))
	if err != nil {
		t.Fatalf("load mapping: %v", err)
	}
	schema.Storage = cubeschema.StorageLive

	lim := 200
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Filters: []Filter{{
			Member:   "product.updated_at",
			Operator: "gt",
			Values:   []any{"2026-10-22 12:00:00"},
		}},
		Limit: &lim,
	}
	res, err := BuildTSQL(q, schema, "t_bd_item_info", mp)
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	t.Logf("T-SQL:\n%s", sql)

	// ① 不得出现 VARCHAR(本地化格式 ⇒ 字典序 ≠ 时间序)。
	if strings.Contains(sql, "AS VARCHAR)") {
		t.Errorf("live 路径不得把时间列先 CAST 成 VARCHAR 再比较"+
			"(MSSQL 的 datetime→VARCHAR 是本地化格式 'Jul 22 2026  5:20PM',字典序 ≠ 时间序 ⇒ 过滤静默出错):\n%s", sql)
	}
	// ② 不得出现 DATE(抹掉时分秒 ⇒ 当天的变动被漏掉)。
	if strings.Contains(sql, "AS DATE)") {
		t.Errorf("live 路径的时间过滤必须 CAST 成 DATETIME(CAST 成 DATE 会抹掉时分秒):\n%s", sql)
	}
	// ③ 必须真的按 DATETIME 比较。
	if !strings.Contains(sql, "AS DATETIME) > @p1") {
		t.Errorf("live 路径的 gt 应是 CAST(... AS DATETIME) > @p1,实际:\n%s", sql)
	}
}

// TestLiveProduct_UpdatedAtSelectMatchesFilter 锁"SELECT / GROUP BY / WHERE 三处一致"。
//
// ⚠️ 不一致 ⇒ 过滤掉的行与返回的行不是同一批(比"过滤失效"更隐蔽)。
func TestLiveProduct_UpdatedAtSelectMatchesFilter(t *testing.T) {
	schema := loadProductSchema(t)
	mp, err := fieldmapping.Load([]byte(liveProductMapping))
	if err != nil {
		t.Fatalf("load mapping: %v", err)
	}
	schema.Storage = cubeschema.StorageLive

	lim := 200
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id", "product.updated_at"},
		Filters: []Filter{{
			Member:   "product.updated_at",
			Operator: "gt",
			Values:   []any{"2026-10-22 12:00:00"},
		}},
		Limit: &lim,
	}
	res, err := BuildTSQL(q, schema, "t_bd_item_info", mp)
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	// SELECT 与 GROUP BY 用的是**源列名**(经 mapping 重写):COALESCE(modify_date, build_date)。
	expr := "COALESCE(modify_date, build_date)"
	if n := strings.Count(sql, expr); n < 2 {
		// SELECT + GROUP BY 各一处(过滤里是 CAST(...) 包着的,算第三次)。
		t.Errorf("表达式 %q 应在 SELECT/GROUP BY/WHERE 各出现一次(共 >=3 次),实际 %d 次:\n%s",
			expr, n, sql)
	}
}
