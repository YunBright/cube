// Package stock 是思迅家族 stock 模型的定义。
//
// storage: live —— 库存不落 DuckDB,查询透传源库。理由见 settlement/preagg.go:
// 留着能用的 Build 就等于留着"静默同步库存表"的能力。
package stock

import (
	"context"
	"fmt"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 表名(live model 无预聚合)。
const PreAggName = "stock"

// Build 永远失败:stock 是 live model,不允许物化。
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
