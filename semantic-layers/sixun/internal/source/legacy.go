// legacy.go 探测源库的 SQL Server 版本能力。
//
// 为什么必须探测而不是配置:同一个思迅产品线,hbposv7(hbposepro)实测是
// **compat level 80 / SQL Server 2008**,ysx(hbposv10)是 2014。
// 前者用不了 DATEFROMPARTS(2012+)、用不了 OFFSET/FETCH(2012+) ——
// 这两条都是 2026-10-09 拿编译出的 SQL 真打源库才暴露出来的,
// 单测无论如何都发现不了。
//
// 如果靠配置,迟早有人把同一个 config 复制到另一个实例上而忘了改,
// 症状是"分页 / 按月汇总在某个门店报语法错"。**能力属于数据库,就该问数据库。**
package source

import (
	"context"
	"database/sql"
	"fmt"
)

// DetectLegacyTSQL 探测实例是否为 SQL Server 2008 及更早。
//
// 判据用 ProductMajorVersion(2008=10 / 2012=11 / 2014=12)。
// 探测失败时**保守返回 true** —— 老方言在任何版本上都能跑,
// 而新方言在老版本上必然炸。宁可慢一点,也不能发必然失败的 SQL 给生产库。
//
// 第二个返回值是探测本身的错误,只用于日志:即便探测失败,legacy=true
// 这个决定仍然成立,调用方应当继续启动。
func DetectLegacyTSQL(ctx context.Context, db *sql.DB) (bool, error) {
	var major sql.NullInt64
	if err := db.QueryRowContext(ctx,
		"SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS INT)").Scan(&major); err != nil {
		return true, fmt.Errorf("detect sqlserver version: %w", err)
	}
	if !major.Valid {
		return true, nil
	}
	return major.Int64 < 11, nil
}
