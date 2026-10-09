// Package source 定义思迅各版本数据源 connector 的统一接口。
//
// 设计要点:
//   - 每个版本(hbposv7 / ysx)实现 Connector,返回思迅原始数据
//   - 数据经 mapping.yaml 映射后写 DuckDB 预聚合表
//   - 各实例 cmd 启动时通过 build tag / config 选具体 connector
package source

import "context"

// Connector 是数据源连接器接口。
//
// 实现:hbposv7.Connector / ysx.Connector
type Connector interface {
	// FetchSupplier 拉供应商数据。
	FetchSupplier(ctx context.Context) ([]map[string]any, error)
	// FetchProduct 拉商品数据。
	FetchProduct(ctx context.Context) ([]map[string]any, error)
	// FetchCategory 拉商品分类数据。
	FetchCategory(ctx context.Context) ([]map[string]any, error)
	// FetchSaleDetail 拉销售明细(含销售退货)。
	FetchSaleDetail(ctx context.Context) ([]map[string]any, error)
	// FetchStock 拉库存数据。
	FetchStock(ctx context.Context) ([]map[string]any, error)
	// Close 关闭连接。
	Close() error
}

// Config 是 connector 共享配置。
type Config struct {
	// DSN 由 dapr secrets 提供,不要写死
	DSN string `yaml:"dsn"`
	// Version 用于日志
	Version string `yaml:"version"`
}

// Fetch 角色名。与 Connector 各 Fetch* 方法一一对应,用于按角色配行数上限。
const (
	RoleSupplier = "supplier"
	RoleProduct  = "product"
	RoleCategory = "category"
	RoleSale     = "sale"
	RoleStock    = "stock"
)

// DefaultRowLimit 是未显式配置时的行数上限兜底值。
//
// 历史包袱:两个 connector 曾把 "SELECT TOP 10000 *" 硬编码在 fetchTable 里,
// 意图是"别一次性拉太多"。实测行数(2026-10-09):
//
//	t_bd_item_info    (商品) ysx/hbposv10 = 27299,hbposv7/hbposepro = 44313
//	t_im_branch_stock (库存) ysx/hbposv10 = 23576,hbposv7/hbposepro = 10607
//	t_bd_supcust_info (供应商) 218 / 300
//	t_bd_item_cls     (分类)   186 / 596
//
// 10000 的上限让 ysx 丢 63% 商品、hbposv7 丢 77%,**库存表还丢 57%**
// —— 后者会让盘点单的账面数量直接是错的。表现统一是"某些条码扫不到",
// 服务 / sidecar / 健康检查全绿,没有任何报错。
//
// 因此默认值必须**高于所有维表的真实行数**:漏配配置时多拉一点数据
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