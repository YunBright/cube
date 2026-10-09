// Package sale_day 是思迅家族 sale_day 模型的定义。
//
// 源表 t_rm_daysum 是唯一两库都有数据、列结构一致的销售表。
// 注意:现有 config 的 table_sale 指向的两张销售表在两个库里都是 0 行空表。
//
// storage: live —— 不落 DuckDB,查询透传源库;Build 是守卫,理由同 settlement。
// 这条尤其重要:t_rm_daysum 在 ysx 有 120 万行,静默同步进 DuckDB 是最大的坑。
package sale_day

import (
	"context"
	"fmt"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 表名(live model 无预聚合)。
const PreAggName = "sale_day"

// Build 永远失败:sale_day 是 live model,不允许物化。
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
