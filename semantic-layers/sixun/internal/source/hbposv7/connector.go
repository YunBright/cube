// Package hbposv7 是思迅 7pro 数据源 connector。
//
// 数据流:
//
//	思迅 7pro DB → hbposv7.Connector.Fetch* → 原始 rows
//	→ 经 cmd/sixun-hbposv7/mapping.yaml 映射
//	→ 写 DuckDB 预聚合表(supplier / product / order_fact)
package hbposv7

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
)

// Connector 实现 source.Connector,接思迅 7pro DB。
type Connector struct {
	// legacyTSQL	true = 目标实例是 SQL Server 2008 及更早(见 internal/source/legacy.go)
	legacyTSQL    bool
	db            *sql.DB
	tableSupplier string
	tableProduct  string
	tableCategory string
	tableSale     string
	rowLimits     *source.RowLimits
}

// Options 构造参数。
type Options struct {
	DSN           string
	TableSupplier string
	TableProduct  string
	TableCategory string
	TableSale     string
	// RowLimits 按角色的拉取行数上限;nil 时全部回落到 source.DefaultRowLimit。
	RowLimits *source.RowLimits
}

// New 构造 Connector,做 ping 校验 DSN 通。
func New(opts Options) (*Connector, error) {
	db, err := sql.Open("sqlserver", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("hbposv7: open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hbposv7: ping db: %w", err)
	}

	// 探测 SQL Server 版本:hbposv7 实测是 2008(compat 80),不支持
	// DATEFROMPARTS / OFFSET-FETCH;ysx 是 2014,都支持。能力问数据库,
	// 不靠配置 —— 配置迟早会被复制到另一个实例上而忘了改。
	legacy, probeErr := source.DetectLegacyTSQL(context.Background(), db)
	if probeErr != nil {
		legacy = true // 探测失败保守处理:老方言在任何版本都能跑
	}
	return &Connector{
		db:            db,
		legacyTSQL:    legacy,
		tableSupplier: opts.TableSupplier,
		tableProduct:  opts.TableProduct,
		tableCategory: opts.TableCategory,
		tableSale:     opts.TableSale,
		rowLimits:     opts.RowLimits,
	}, nil
}

// FetchSupplier 拉供应商原始数据(思迅7pro 原始字段名)。
func (c *Connector) FetchSupplier(ctx context.Context) ([]map[string]any, error) {
	if c.tableSupplier == "" {
		return nil, fmt.Errorf("hbposv7: table_supplier not configured")
	}
	return c.fetchTable(ctx, c.tableSupplier, source.RoleSupplier)
}

// FetchProduct 拉商品原始数据。
func (c *Connector) FetchProduct(ctx context.Context) ([]map[string]any, error) {
	if c.tableProduct == "" {
		return nil, fmt.Errorf("hbposv7: table_product not configured")
	}
	return c.fetchTable(ctx, c.tableProduct, source.RoleProduct)
}

// FetchCategory 拉商品分类数据。
func (c *Connector) FetchCategory(ctx context.Context) ([]map[string]any, error) {
	if c.tableCategory == "" {
		return nil, fmt.Errorf("hbposv7: table_category not configured")
	}
	return c.fetchTable(ctx, c.tableCategory, source.RoleCategory)
}

// FetchSaleDetail 拉销售明细(含销售退货)。
func (c *Connector) FetchSaleDetail(ctx context.Context) ([]map[string]any, error) {
	if c.tableSale == "" {
		return nil, fmt.Errorf("hbposv7: table_sale not configured")
	}
	return c.fetchTable(ctx, c.tableSale, source.RoleSale)
}

// duckTarget 把 model 名映射到 (源库表名, 行数角色)。
//
// 与 ysx 的同名方法逐字一致 —— 两个 family 的 Connector 结构相同,
// 增量 SQL 构造共用 source.FetchSince,这里只做 model→表 的映射。
// 复制粘贴时若某个 family 漏了这个方法,它会变成"编译不过"而不是
// "静默只支持全量"。
func (c *Connector) duckTarget(model string) (table, role string, ok bool) {
	switch model {
	case "product":
		return c.tableProduct, source.RoleProduct, true
	case "supplier":
		return c.tableSupplier, source.RoleSupplier, true
	case "category":
		return c.tableCategory, source.RoleCategory, true
	case "sale_detail":
		return c.tableSale, source.RoleSale, true
	}
	return "", "", false
}

// FetchSince 增量拉取(见 source.FetchSince 的时区与边界说明)。
func (c *Connector) FetchSince(ctx context.Context, model string, cols source.IncrementalCols, since string) ([]map[string]any, error) {
	table, role, ok := c.duckTarget(model)
	if !ok {
		return nil, fmt.Errorf("hbposv7: 未知 model %q,无法增量拉取", model)
	}
	if table == "" {
		return nil, fmt.Errorf("hbposv7: model %s 的 table_* 未配置", model)
	}
	return source.FetchSince(ctx, c.db, table, cols, since, c.rowLimits.For(role))
}

// SourceNow 返回源库当前时间作为增量水位。
func (c *Connector) SourceNow(ctx context.Context) (string, error) {
	return source.SourceNow(ctx, c.db)
}

// FetchStock 拉库存数据。
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
		return nil, fmt.Errorf("hbposv7: %w", err)
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("hbposv7: live query: %w", err)
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

// fetchTable 通用 SELECT TOP <role 上限> * 拉数据。
//
// 行数上限来自配置(source.row_limit / source.row_limits),不再硬编码:
// 硬编码 TOP 10000 会静默截断大表(hbposv10 的 t_bd_item_info 实测 27299 行),
// 表现为"部分商品查不到"且无任何报错。生产上若单表超出上限,应先确认是否需要
// 改为 cursor / 分页,而不是靠调小 limit 掩盖。
func (c *Connector) fetchTable(ctx context.Context, table, role string) ([]map[string]any, error) {
	limit := c.rowLimits.For(role)
	q := fmt.Sprintf("SELECT TOP %d * FROM %s", limit, table)
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("hbposv7: query %s: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("hbposv7: cols %s: %w", table, err)
	}
	out := make([]map[string]any, 0, 256)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("hbposv7: scan %s: %w", table, err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Compile-time check:实现 source.Connector
var _ source.Connector = (*Connector)(nil)
