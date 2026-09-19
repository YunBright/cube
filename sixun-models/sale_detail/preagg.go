// Package sale_detail 是思迅家族 sale_detail 模型的 DuckDB 预聚合定义。
package sale_detail

import (
	"context"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 预聚合表名。
const PreAggName = "sale_detail"

// Build 构建 sale_detail 预聚合表。
func Build(ctx context.Context, db *duckdb.Engine, rawSourceSQL string) error {
	return db.BuildPreAgg(PreAggName, rawSourceSQL)
}

// Refresh 刷新预聚合(P2)。
func Refresh(ctx context.Context, db *duckdb.Engine) error {
	_ = ctx
	_ = db
	return nil
}