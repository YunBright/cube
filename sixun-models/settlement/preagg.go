// Package settlement 是思迅家族 settlement 模型的定义。
//
// 源表 t_fm_recpay_gx_master 是多态表,同一物理表按 trans_no 装 DP/CP/LY/RP,
// 结款单据类型由 schema 的 document_type 维度区分,这里不做任何过滤。
//
// storage: live —— 本 model 不落 DuckDB,查询由 queryHandler 透传源库
// (cubequery.BuildTSQL)。因此 Build 是**故意不可用**的守卫,不是可用实现:
// 接进 fetchers 就等于把整张结算单静默同步进 DuckDB,正是 AGENTS.md
// 「静默截断/静默同步比报错危险」要防的事。
package settlement

import (
	"context"
	"fmt"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 表名(live model 无预聚合,保留常量只为报错信息可读)。
const PreAggName = "settlement"

// Build 永远失败:settlement 是 live model,不允许物化。
func Build(ctx context.Context, db *duckdb.Engine, rawSourceSQL string) error {
	_ = ctx
	_ = db
	_ = rawSourceSQL
	return fmt.Errorf("%s 是 storage: live model,不物化进 DuckDB;查询走 cubequery.BuildTSQL 透传源库", PreAggName)
}

// Refresh 无需刷新:live model 没有预聚合。返回 nil 是**正确语义**(确实无事可做),
// 不是"假装成功"——想刷新的是 duck model,走各自的 Build。
func Refresh(ctx context.Context, db *duckdb.Engine) error {
	_ = ctx
	_ = db
	return nil
}
