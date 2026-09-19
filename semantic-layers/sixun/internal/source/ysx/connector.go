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
	db             *sql.DB
	tableSupplier  string
	tableProduct   string
	tableCategory  string
	tableSale      string
	tableStock     string
}

// Options 构造参数。
type Options struct {
	DSN            string
	TableSupplier  string
	TableProduct   string
	TableCategory  string
	TableSale      string
	TableStock     string
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
	return &Connector{
		db:             db,
		tableSupplier:  opts.TableSupplier,
		tableProduct:   opts.TableProduct,
		tableCategory:  opts.TableCategory,
		tableSale:      opts.TableSale,
		tableStock:     opts.TableStock,
	}, nil
}

// FetchSupplier 拉供应商原始数据(云商x 原始字段名,可能与 7pro 不同)。
func (c *Connector) FetchSupplier(ctx context.Context) ([]map[string]any, error) {
	if c.tableSupplier == "" {
		return nil, fmt.Errorf("ysx: table_supplier not configured")
	}
	return c.fetchTable(ctx, c.tableSupplier)
}

// FetchProduct 拉商品原始数据。
func (c *Connector) FetchProduct(ctx context.Context) ([]map[string]any, error) {
	if c.tableProduct == "" {
		return nil, fmt.Errorf("ysx: table_product not configured")
	}
	return c.fetchTable(ctx, c.tableProduct)
}

// FetchCategory 拉商品分类数据。
func (c *Connector) FetchCategory(ctx context.Context) ([]map[string]any, error) {
	if c.tableCategory == "" {
		return nil, fmt.Errorf("ysx: table_category not configured")
	}
	return c.fetchTable(ctx, c.tableCategory)
}

// FetchSaleDetail 拉销售明细(含销售退货)。
func (c *Connector) FetchSaleDetail(ctx context.Context) ([]map[string]any, error) {
	if c.tableSale == "" {
		return nil, fmt.Errorf("ysx: table_sale not configured")
	}
	return c.fetchTable(ctx, c.tableSale)
}

// FetchStock 拉库存数据。
func (c *Connector) FetchStock(ctx context.Context) ([]map[string]any, error) {
	if c.tableStock == "" {
		return nil, fmt.Errorf("ysx: table_stock not configured")
	}
	return c.fetchTable(ctx, c.tableStock)
}

// Close 关闭连接。
func (c *Connector) Close() error {
	if c.db == nil {
		return nil
	}
	return c.db.Close()
}

func (c *Connector) fetchTable(ctx context.Context, table string) ([]map[string]any, error) {
	q := fmt.Sprintf("SELECT TOP 10000 * FROM %s", table)
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