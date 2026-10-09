// Package source 定义思迅各版本数据源 connector 的统一接口。
//
// 设计要点:
//   - 每个版本(hbposv7 / ysx)实现 Connector,返回思迅原始数据
//   - 数据经 mapping.yaml 映射后写 DuckDB 预聚合表
//   - 各实例 cmd 启动时通过 build tag / config 选具体 connector
package source

import (
	"context"
	"database/sql"
)

// Connector 是数据源连接器接口。
//
// ⚠️ 这里**只列 storage: duck 的 model**(枚举/档案类)。
// live 的 model(settlement / settlement_line / purchase_sheet /
// purchase_sheet_line / sale_day / stock)不在接口里 —— cube 不做任何原始数据同步,
// 它们由 queryHandler 用 QueryLive 实时透传,没有"拉全量"这一步。
// 刻意不给它们留 Fetch* 方法:留着就等于把"静默同步百万行"的能力放在一行之外。
type Connector interface {
	// FetchSupplier 拉供应商数据。
	FetchSupplier(ctx context.Context) ([]map[string]any, error)
	// FetchProduct 拉商品数据。
	FetchProduct(ctx context.Context) ([]map[string]any, error)
	// FetchCategory 拉商品分类数据。
	FetchCategory(ctx context.Context) ([]map[string]any, error)
	// FetchSaleDetail 拉销售明细(含销售退货)。
	FetchSaleDetail(ctx context.Context) ([]map[string]any, error)
	// QueryLive 把一条**只读** SQL 直接发给源库并返回行切片。
	//
	// 供 storage: live 的 model 使用:它们不落 DuckDB,查询时实时打源库。
	// SQL 由 cubequery.BuildTSQL 编译(参数全部占位符绑定,值不进 SQL 文本)。
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

// Config 是 connector 共享配置。
type Config struct {
	// DSN 由 dapr secrets 提供,不要写死
	DSN string `yaml:"dsn"`
	// Version 用于日志
	Version string `yaml:"version"`
}

// Fetch 角色名。与 Connector 各 Fetch* 方法一一对应,用于按角色配行数上限。
//
// ⚠️ 这里**只有 duck 的 model**。live 的 model 没有"拉全量"这一步,
// 它们的体量(实测 ysx sale_day 120 万行)恰恰是不能落盘的原因。
const (
	RoleSupplier = "supplier"
	RoleProduct  = "product"
	RoleCategory = "category"
	RoleSale     = "sale"
)

// DefaultRowLimit 是未显式配置时的行数上限兜底值。
//
// 只对 **storage: duck** 的 model 生效(枚举/档案类)。live 的 model
// 不走 Fetch,行数由调用方的 limit 决定,不受此值约束。
//
// 历史包袱:两个 connector 曾把 "SELECT TOP 10000 *" 硬编码在 fetchTable 里,
// 意图是"别一次性拉太多"。实测行数(2026-10-09):
//
//	t_bd_item_info    (商品) ysx/hbposv10 = 27299,hbposv7/hbposepro = 44313
//	t_im_branch_stock (库存) ysx/hbposv10 = 23576,hbposv7/hbposepro = 10607  ← 现已改 live
//	t_bd_supcust_info (供应商) 218 / 300
//	t_bd_item_cls     (分类)   186 / 596
//
// 10000 的上限让 ysx 丢 63% 商品、hbposv7 丢 77%,**库存表还丢 57%**
// —— 后者会让盘点单的账面数量直接是错的。表现统一是"某些条码扫不到",
// 服务 / sidecar / 健康检查全绿,没有任何报错。
//
// 因此默认值必须**高于所有 duck model 的真实行数**:漏配配置时多拉一点数据
// (实测 27299 行 × 65 列全量 = 3.6MB / 1.4s)远比静默丢数据安全。
// **截断必须是显式决定,不是默认行为。** 真要限流时按角色单独配
// source.row_limits(sale 流水表才是该限的那个)。
const DefaultRowLimit = 50000

// RowLimits 按角色解析行数上限。零值 / 未覆盖的角色回落到 DefaultRowLimit。
type RowLimits struct {
	defaultLimit int
	byRole       map[string]int
}

// NewRowLimits 构造 RowLimits。defaultLimit <= 0 时用 DefaultRowLimit。
func NewRowLimits(defaultLimit int, byRole map[string]int) *RowLimits {
	if defaultLimit <= 0 {
		defaultLimit = DefaultRowLimit
	}
	return &RowLimits{defaultLimit: defaultLimit, byRole: byRole}
}

// For 返回该角色的行数上限。always > 0。
func (r *RowLimits) For(role string) int {
	if r != nil {
		if v, ok := r.byRole[role]; ok && v > 0 {
			return v
		}
		return r.defaultLimit
	}
	return DefaultRowLimit
}
