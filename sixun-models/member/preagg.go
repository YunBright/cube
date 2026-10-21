// Package member 是思迅家族 member 模型的 DuckDB 预聚合定义。
//
// 会员卡档案(t_rm_vip_info)是**档案类**数据:行数不大(实测 ysx 4128 行 /
// hbposv7 2642 行)、变化慢、但被高频查询 ⇒ 落 DuckDB(storage: duck),
// 与 supplier / product / category 同属一类。
//
// ⚠️ 本 model **故意没有任何金额类 measure**(见 schema.yaml 的说明):
// balance_yuan 是带脏值的字符串(residual_amt 是 varchar),SUM(它)在 DuckDB 里
// 要么报错、要么把解析不了的行当成 0 —— 那是把数据质量问题藏进聚合结果。
package member

import (
	"context"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 预聚合表名(与 schema.yaml 的 sql_table 对齐)。
const PreAggName = "member"

// Build 构建 member 预聚合表。
//
// rawSourceSQL 由 main.go 传入(形如 "SELECT * FROM member_raw"),
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
