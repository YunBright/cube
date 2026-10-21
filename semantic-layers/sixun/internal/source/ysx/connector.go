// Package ysx 是思迅云商x 数据源 connector。
package ysx

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
)

// Connector 实现 source.Connector,接思迅云商x DB。
type Connector struct {
	// legacyTSQL	true = 目标实例是 SQL Server 2008 及更早(见 internal/source/legacy.go)
	legacyTSQL      bool
	db              *sql.DB
	tableSupplier   string
	tableProduct    string
	tableCategory   string
	tableCustType   string
	tableSaleWay    string
	tableSale       string
	tableMember     string
	tableMemberType string
	rowLimits       *source.RowLimits
}

// Options 构造参数。
type Options struct {
	DSN           string
	TableSupplier string
	TableProduct  string
	TableCategory string
	// TableCustType 是供应商/客户类别字典表(t_bd_cust_type)。
	// 行数极小(实测 ysx 4 行 / hbposv7 0 行),但字典表被截断
	// = 某些供应商查不到自己的类别。
	TableCustType string
	// TableSaleWay 是「销售方式」字典表(t_bd_base_code)。
	//
	// ⚠️ 这张表是**通用**基础代码字典,含职称/民族/学历/企业类型/品牌/单位/
	// 销售方式… 共 30+ 行;只取 type_no='SW' 的子集(见 FetchSaleWay)。
	TableSaleWay string
	TableSale    string
	// TableMember 是会员卡档案表(t_rm_vip_info)。
	// 实测 ysx/hbposv10 = 4128 行 / hbposv7/hbposepro = 2642 行。
	TableMember string
	// TableMemberType 是会员类别字典表(t_rm_vip_type)。
	// 实测两个库各 3 行;字典表被截断 = 某些会员类别查不到自己的类别。
	TableMemberType string
	// RowLimits 按角色的拉取行数上限;nil 时全部回落到 source.DefaultRowLimit。
	RowLimits *source.RowLimits
}

// New 构造 Connector,做 ping 校验 DSN 通。
func New(opts Options) (*Connector, error) {
	db, err := sql.Open("sqlserver", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("ysx: open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ysx: ping db: %w", err)
	}

	// 探测 SQL Server 版本:hbposv7 实测是 2008(compat 80),不支持
	// DATEFROMPARTS / OFFSET-FETCH;ysx 是 2014,都支持。能力问数据库,
	// 不靠配置 —— 配置迟早会被复制到另一个实例上而忘了改。
	legacy, probeErr := source.DetectLegacyTSQL(context.Background(), db)
	if probeErr != nil {
		legacy = true // 探测失败保守处理:老方言在任何版本都能跑
	}
	return &Connector{
		db:              db,
		legacyTSQL:      legacy,
		tableSupplier:   opts.TableSupplier,
		tableProduct:    opts.TableProduct,
		tableCategory:   opts.TableCategory,
		tableCustType:   opts.TableCustType,
		tableSaleWay:    opts.TableSaleWay,
		tableSale:       opts.TableSale,
		tableMember:     opts.TableMember,
		tableMemberType: opts.TableMemberType,
		rowLimits:       opts.RowLimits,
	}, nil
}

// FetchSupplier 拉供应商原始数据(云商x 原始字段名,可能与 7pro 不同)。
func (c *Connector) FetchSupplier(ctx context.Context) ([]map[string]any, error) {
	if c.tableSupplier == "" {
		return nil, fmt.Errorf("ysx: table_supplier not configured")
	}
	return c.fetchTable(ctx, c.tableSupplier, source.RoleSupplier)
}

// FetchProduct 拉商品原始数据。
func (c *Connector) FetchProduct(ctx context.Context) ([]map[string]any, error) {
	if c.tableProduct == "" {
		return nil, fmt.Errorf("ysx: table_product not configured")
	}
	return c.fetchTable(ctx, c.tableProduct, source.RoleProduct)
}

// FetchCategory 拉商品分类数据。
func (c *Connector) FetchCategory(ctx context.Context) ([]map[string]any, error) {
	if c.tableCategory == "" {
		return nil, fmt.Errorf("ysx: table_category not configured")
	}
	return c.fetchTable(ctx, c.tableCategory, source.RoleCategory)
}

// FetchCustType 拉供应商/客户类别字典(t_bd_cust_type)。
//
// 行数极小(实测 ysx 4 行),但它**独立于 supplier 表**:
// supplier.cust_clsno 指向的是本表的 type_id,两张表都要拉。
func (c *Connector) FetchCustType(ctx context.Context) ([]map[string]any, error) {
	if c.tableCustType == "" {
		return nil, fmt.Errorf("ysx: table_cust_type not configured")
	}
	return c.fetchTable(ctx, c.tableCustType, source.RoleCustType)
}

// FetchSaleWay 拉「销售方式」字典(t_bd_base_code 中 type_no='SW' 的行)。
//
// ⚠️ **必须过滤**:t_bd_base_code 是通用基础代码字典(职称/民族/学历/企业类型/
// 品牌/单位/销售方式…共 30+ 行),而 code_id 在不同 type_no 下**会重复**
// ('1' 在职称/民族/学历里都有)。整张表灌进 sale_way model,primary_key 撞车
// ⇒ 某些行被静默吞掉,查出来的 code_id→code_name 对应关系是错的。
//
// 过滤放在这里而不是 mapping:mapping 只做列名重写,不做行过滤;
// 而 fetchTable 只能 `SELECT TOP n * FROM <表>`,没有 WHERE。
func (c *Connector) FetchSaleWay(ctx context.Context) ([]map[string]any, error) {
	if c.tableSaleWay == "" {
		return nil, fmt.Errorf("ysx: table_sale_way not configured")
	}
	rows, err := c.fetchTable(ctx, c.tableSaleWay, source.RoleSaleWay)
	if err != nil {
		return nil, err
	}
	return source.RequireRows(source.RoleSaleWay,
		source.FilterRowsByColumn(rows, "type_no", "SW"))
}

// FetchMember 拉会员卡档案(t_rm_vip_info)。
//
// ⚠️ 表名只来自 config.yaml(source.table_member):它是 family 私有事实,
// 不能写进共享的 sixun-models/schema.yaml。
func (c *Connector) FetchMember(ctx context.Context) ([]map[string]any, error) {
	if c.tableMember == "" {
		return nil, fmt.Errorf("ysx: table_member not configured")
	}
	return c.fetchTable(ctx, c.tableMember, source.RoleMember)
}

// FetchMemberType 拉会员类别字典(t_rm_vip_type)。
//
// 实测 ysx 3 行('0'=会员卡 / '1'=储值卡 / '2'=VIP储值卡),
// 独立于 member 表:member.card_type 指向本表的 type_id,两张表都要拉。
func (c *Connector) FetchMemberType(ctx context.Context) ([]map[string]any, error) {
	if c.tableMemberType == "" {
		return nil, fmt.Errorf("ysx: table_member_type not configured")
	}
	return c.fetchTable(ctx, c.tableMemberType, source.RoleMemberType)
}

// FetchSaleDetail 拉销售明细(含销售退货)。
func (c *Connector) FetchSaleDetail(ctx context.Context) ([]map[string]any, error) {
	if c.tableSale == "" {
		return nil, fmt.Errorf("ysx: table_sale not configured")
	}
	return c.fetchTable(ctx, c.tableSale, source.RoleSale)
}

// duckTarget 把 model 名映射到 (源库表名, 行数角色)。
//
// 增量刷新按 model 名调用,而不是让调用方传表名 —— 表名是 family 私有事实
// (来自 config.yaml 的 table_*),调用方知道它就等于把配置泄漏进了 dataloader,
// 而 dataloader 是两个 family 共用的。
func (c *Connector) duckTarget(model string) (table, role string, ok bool) {
	switch model {
	case "product":
		return c.tableProduct, source.RoleProduct, true
	case "supplier":
		return c.tableSupplier, source.RoleSupplier, true
	case "category":
		return c.tableCategory, source.RoleCategory, true
	case "cust_type":
		return c.tableCustType, source.RoleCustType, true
	case "sale_way":
		return c.tableSaleWay, source.RoleSaleWay, true
	case "sale_detail":
		return c.tableSale, source.RoleSale, true
	case "member":
		return c.tableMember, source.RoleMember, true
	case "member_type":
		return c.tableMemberType, source.RoleMemberType, true
	}
	return "", "", false
}

// FetchSince 增量拉取(见 source.FetchSince 的时区与边界说明)。
func (c *Connector) FetchSince(ctx context.Context, model string, cols source.IncrementalCols, since string) ([]map[string]any, error) {
	table, role, ok := c.duckTarget(model)
	if !ok {
		return nil, fmt.Errorf("ysx: 未知 model %q,无法增量拉取", model)
	}
	if table == "" {
		return nil, fmt.Errorf("ysx: model %s 的 table_* 未配置", model)
	}
	return source.FetchSince(ctx, c.db, table, cols, since, c.rowLimits.For(role))
}

// SourceNow 返回源库当前时间作为增量水位。
func (c *Connector) SourceNow(ctx context.Context) (string, error) {
	return source.SourceNow(ctx, c.db)
}

// QueryLive 把只读 SQL 直接发给源库(storage: live 的 model 用)。
//
// live 的 model: settlement / settlement_line / purchase_sheet /
// purchase_sheet_line / sale_day / stock —— cube 不做原始数据同步,
// 它们没有 Fetch* 方法,只在查询时经这条路实时打源库。
// LegacyTSQL 报告目标实例是否为 SQL Server 2008 及更早。
// 供编译器选择日期截断与分页写法(DATEFROMPARTS / OFFSET-FETCH 都是 2012+)。
func (c *Connector) LegacyTSQL() bool { return c.legacyTSQL }

func (c *Connector) QueryLive(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	// 源库是生产库:只读校验放在这里,而不仅依赖"编译器不生成写语句"。
	if err := source.AssertReadOnly(query); err != nil {
		return nil, fmt.Errorf("ysx: %w", err)
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ysx: live query: %w", err)
	}
	return source.ScanRows(rows)
}

// Close 关闭连接。
func (c *Connector) Close() error {
	if c.db == nil {
		return nil
	}
	return c.db.Close()
}

func (c *Connector) fetchTable(ctx context.Context, table, role string) ([]map[string]any, error) {
	limit := c.rowLimits.For(role)
	q := fmt.Sprintf("SELECT TOP %d * FROM %s", limit, table)
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ysx: query %s: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("ysx: cols %s: %w", table, err)
	}
	out := make([]map[string]any, 0, 256)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("ysx: scan %s: %w", table, err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Compile-time check
var _ source.Connector = (*Connector)(nil)
