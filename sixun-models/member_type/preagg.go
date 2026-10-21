// Package member_type 是思迅家族 member_type 模型的 DuckDB 预聚合定义。
//
// 会员类别字典(t_rm_vip_type)是**枚举表**:实测两个库各 3 行
// (ysx: 会员卡 / 储值卡 / VIP储值卡;hbposv7: 会员卡 / 储值卡 / 微信会员)。
//
// ⚠️ 两个库的类别字典**取值集合不同** ⇒ 下游做 member.card_type → member_type.id
// 关联时必须按同一个库的字典关联,跨库拿 ID 会张冠李戴。
// 这里不做任何归一化(没有 enum_map,也没有把 'wx' 归一成 '2' 之类)——
// 归一化留在 schema meta + BI 层(AGENTS.md §4)。
package member_type

import (
	"context"

	"github.com/YunBright/cube/pkg/duckdb"
)

// PreAggName 预聚合表名(与 schema.yaml 的 sql_table 对齐)。
const PreAggName = "member_type"

// Build 构建 member_type 预聚合表。
func Build(ctx context.Context, db *duckdb.Engine, rawSourceSQL string) error {
	return db.BuildPreAgg(PreAggName, rawSourceSQL)
}

// Refresh 刷新预聚合(P2)。
func Refresh(ctx context.Context, db *duckdb.Engine) error {
	_ = ctx
	_ = db
	return nil
}
