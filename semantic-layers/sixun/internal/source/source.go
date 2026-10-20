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
	"fmt"
	"strings"
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

	// FetchSince 拉取该 model 中增量时间窗口内的行(增量刷新)。
	//
	// model 传的是 **model 名**(如 "product"),不是表名 —— 表名是 family 私有
	// 事实(config.yaml 的 table_*),调用方不该也不需要知道它。
	// cols 给出增量时间列(主列 + 可选回退列),见 IncrementalCols。
	// 未给该 model 配增量列时,Connector 必须返回错误而不是静默返回全量:
	// 调用方会据此回落到全量刷新,而"静默返回全量"会让它以为自己在跑增量。
	FetchSince(ctx context.Context, model string, cols IncrementalCols, since string) ([]map[string]any, error)

	// SourceNow 返回源库当前时间(SQLServerTimeLayout 格式),
	// 作为下一次增量刷新的水位。**不能用本地时钟代替**,理由见 incremental.go。
	SourceNow(ctx context.Context) (string, error)
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
	// RoleCustType 是供应商/客户类别字典(t_bd_cust_type)。
	//
	// 行数极小(实测 ysx 4 行 / hbposv7 0 行),但**仍然要一个独立角色**:
	// 它和 supplier 不是同一张表,行数上限的语义也不同(字典表永远不该被截断,
	// 而截断了就等于"某些供应商查不到自己的类别")。
	RoleCustType = "cust_type"
	// RoleSaleWay 是「销售方式」字典(t_bd_base_code 中 type_no='SW' 的行)。
	//
	// 实测两个库的供应商**全部有 sale_way**(ysx 219 / hbposv7 300),
	// 而客户类别(cust_clsno)有 170/219 是 NULL —— 后者覆盖不了大部分供应商,
	// 当"供应商类别"用会让大部分供应商落进"未分类"。
	//
	// ⚠️ 底层表 t_bd_base_code 是**通用**基础代码字典(职称/民族/学历/企业类型/
	// 品牌/单位/销售方式…),共 30+ 行。整张表灌进 model 会让 id(code_id)跨类型
	// 重复('1' 在职称/民族/学历里都有),primary_key 撞车 ⇒ 行被静默吞掉。
	// 所以过滤必须发生在**入 model 之前**:connector 的 FetchSaleWay 只取
	// type_no='SW' 的子集(见 source.FilterRowsByColumn)。
	RoleSaleWay = "sale_way"
)

// FilterRowsByColumn 按某一列的值过滤原始行(大小写不敏感,两侧去空格)。
//
// 为什么需要它:fetchTable 只能 `SELECT TOP n * FROM <表>` —— **没有 WHERE**,
// 而 mapping 只做列名重写,不做行过滤。所以"只取字典的一个子集"
// (比如 t_bd_base_code 里 type_no='SW' 的那几行)只能在 Go 侧做。
//
// 为什么要放在共享包而不是各 connector 各写一份:两个 family 的
// 思迅表结构一致,过滤规则必须一致;各写一份的话迟早会漂。
//
// 语义是**严格的相等**:列缺失或为 NULL 的行**不保留** ——
// "type_no 为空的行"不是 SW 行。留着的后果是空 id 混进字典表,
// 查出来的 id→name 对应关系里多出一堆没有名字的条目。
func FilterRowsByColumn(rows []map[string]any, col, want string) []map[string]any {
	target := strings.ToLower(strings.TrimSpace(want))
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		v, ok := r[col]
		if !ok || v == nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(fmt.Sprint(v))) == target {
			out = append(out, r)
		}
	}
	return out
}

// RequireRows 断言过滤后**还有行**,否则返回错误。
//
// 为什么需要它:过滤条件写错(列名拼错 / type_no 变了 / 源表结构变了)
// 会让结果**静默变成空集**。而空字典表的症状是
// "App 里所有供应商都落进未分类" —— 看起来像数据问题,
// 实际上是取数逻辑错了,排查方向会被带偏。
//
// ⇒ 宁可导入失败(healthz 的 fresh 状态变红、日志里有明确原因),
// 也不要一个**看起来正常**的空字典。
func RequireRows(role string, rows []map[string]any) ([]map[string]any, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("source: %s 过滤后没有任何数据"+
			"(过滤条件写错了?源表结构变了?)", role)
	}
	return rows, nil
}

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
