// Package supplier 是思迅家族 supplier 模型的 DuckDB 预聚合定义。
package supplier

import (
	"context"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 预聚合表名(与 schema.yaml sql_table 对齐)。
const PreAggName = "supplier"

// Build 构建 supplier 预聚合表。
// rawSourceSQL 必须是纯 SELECT 语句(由 main.go 传入),
// pkg/duckdb.BuildPreAgg 会自动包装成 CREATE OR REPLACE TABLE <name> AS <sql>。
func Build(ctx context.Context, db *duckdb.Engine, rawSourceSQL string) error {
	return db.BuildPreAgg(PreAggName, rawSourceSQL)
}

// Refresh 刷新预聚合(P2)。
func Refresh(ctx context.Context, db *duckdb.Engine) error {
	_ = ctx
	_ = db
	return nil
}