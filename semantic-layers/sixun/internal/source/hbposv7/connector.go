// Package hbposv7 是思迅 7pro 数据源 connector。
//
// # 2026-10-23:只剩"透传源库"这一条路
//
// 以前这里是:思迅 7pro DB → Fetch*(拉全量)→ mapping.yaml 映射
// → 写本地副本(预聚合表)。这条链在 2026-10-23 **整个删掉**了
// (见 AGENTS.md §4.1):24 个 model 都是 storage: live,
// 查询时把 schema 编译成 T-SQL 直打源库,不落任何本地副本。
//
// ⇒ Connector 现在只做三件事:连库、探版本、发一条**只读** SQL。
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
	legacyTSQL bool
	db         *sql.DB
}

// Options 构造参数。
//
// ⚠️ 2026-10-23:以前这里有一长串 Table*(表名)与 RowLimits(行数上限),
// 它们**只**服务于"把 model 拉下来物化进本地副本"。
// 副本层删除后,表名由**编译器**从实例 config.yaml 的 table_* 取
// (见 cmd/sixun-hbposv7/main.go 的 liveTables()),行数由查询里的 limit 决定 ⇒
// 两者都不再是 connector 的责任。
type Options struct {
	DSN string
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
		db:         db,
		legacyTSQL: legacy,
	}, nil
}

// LegacyTSQL 报告目标实例是否为 SQL Server 2008 及更早。
// 供编译器选择日期截断与分页写法(DATEFROMPARTS / OFFSET-FETCH 都是 2012+)。
func (c *Connector) LegacyTSQL() bool { return c.legacyTSQL }

// QueryLive 把只读 SQL 直接发给源库(storage: live 的 model 用)。
//
// live 的 model(全部 24 个):settlement / settlement_line / purchase_sheet /
// purchase_sheet_line / sale_day / stock / sale_detail / payflow / …
// —— cube 不做原始数据同步,它们只在查询时经这条路实时打源库。
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

// Compile-time check:实现 source.Connector
var _ source.Connector = (*Connector)(nil)
