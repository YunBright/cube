// Package source 定义思迅各版本数据源 connector 的统一接口。
//
// # 2026-10-23:只剩"透传源库"这一条路
//
// 以前这个包负责三件事:拉全量 → mapping → 物化进**本地副本**,
// 外加增量水位(IncrementalCols / FetchSince / SourceNow)与夜间全量校准。
// 这三件在 2026-10-23 **全部删掉**了,理由见 AGENTS.md §4.1:
//
//   - 本地副本 = 第二份真相 = 必然滞后,而它的失效模式最贵:
//     "报告照样成功、结果悄悄是旧的"(2026-10-22 生产实测教训);
//   - 源库是**生产库、只读**,定时全量重拉是压力不是设计;
//   - 24 个 model 全部 storage: live ⇒ 没有副本 ⇒ 水位 / 校准 / 刷新 job
//     这套机制**一个都不需要**,而它们每一个都是能悄悄坏掉的地方。
//
// ⇒ Connector 现在只有一个动作:把一条**只读** SQL 发给源库。
package source

import (
	"context"
	"database/sql"
)

// Connector 是数据源连接器接口。
//
// # 2026-10-23:这里曾经有一整排"拉数"方法,现在全没了
//
// 删掉的有:FetchSupplier / FetchProduct / FetchCategory / FetchSaleDetail
// (以及同类的 FetchCustType / FetchSaleWay / FetchMember / FetchMemberType)、
// FetchSince(增量窗口)、SourceNow(水位)。
//
// 它们**全部**只服务于一件事:把 model 物化进本地副本。
// 副本层删除后,它们一个都没有调用方 ⇒ 整个删掉。
//
// 刻意**不保留**它们:留着就等于把"静默同步百万行"的能力
// 放在一行 `FetchXxx` 之外 —— 而那正是 2026-10-22 之前
// "硬编码 TOP 10000 丢掉 63% / 77% 商品且零报错"那类事故的入口。
type Connector interface {
	// QueryLive 把一条**只读** SQL 直接发给源库并返回行切片。
	//
	// 供 storage: live 的 model 使用:它们不落盘,查询时实时打源库。
	// SQL 由 cubequery.BuildTSQLJoin 编译(参数全部占位符绑定,值不进 SQL 文本)。
	QueryLive(ctx context.Context, query string, args ...any) ([]map[string]any, error)
	// Close 关闭连接。
	Close() error
}

// ScanRows 把 *sql.Rows 转成 []map[string]any。两个 connector 的透传路径共用。
func ScanRows(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 64)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
