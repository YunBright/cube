// Package purchase_sheet 是思迅家族 purchase_sheet 模型的定义。
//
// 源表 t_pm_sheet_master 装全部采购类单据(PI 收货 / RO 退货 / PO 订单 ...),
// 按 trans_no 区分,不预置过滤。
//
// storage: live —— 不落 DuckDB,查询透传源库;Build 是守卫,理由同 settlement。
package purchase_sheet

import (
	"context"
	"fmt"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 表名(live model 无预聚合)。
const PreAggName = "purchase_sheet"

// Build 永远失败:purchase_sheet 是 live model,不允许物化。
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
