// Package settlement_line 是思迅家族 settlement_line 模型的定义。
//
// 结算明细行 = 结算单里被结算掉的一张源单据,
// voucher_id 是结算单与采购进货/退货的显式外键。
//
// storage: live —— 不落 DuckDB,查询透传源库;Build 是守卫,理由同 settlement。
package settlement_line

import (
	"context"
	"fmt"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 表名(live model 无预聚合)。
const PreAggName = "settlement_line"

// Build 永远失败:settlement_line 是 live model,不允许物化。
func Build(ctx context.Context, db *duckdb.Engine, rawSourceSQL string) error {
	_ = ctx
	_ = db
	_ = rawSourceSQL
	return fmt.Errorf("%s 是 storage: live model,不物化进 DuckDB;查询走 cubequery.BuildTSQL 透传源库", PreAggName)
}

// Refresh 无需刷新:live model 没有预聚合。
func Refresh(ctx context.Context, db *duckdb.Engine) error {
	_ = ctx
	_ = db
	return nil
}
