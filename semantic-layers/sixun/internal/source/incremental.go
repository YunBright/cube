package source

// 增量刷新(Incremental refresh)的共用 SQL 构造。
//
// # 背景
//
// product 实测 ysx 27304 / hbposv7 44316 行,但每天实际只改约 10 行
// (7 天 70 行 / 30 天 410 行)。每 30 分钟全量重拉一次是在为 10 行的变化
// 付出 2.7~4.4 万行的扫描成本,而源库是 2008 R2 的生产思迅库。
//
// 增量为主的策略:常规刷新只拉 `modify_date >= 水位` 的行,
// 夜间 3 点做一次全量校准兜底(增量天然会漏:某些客户端写入时不更新
// modify_date,而新建商品的 modify_date 常为 NULL —— 实测 ysx 近 7 天
// 新建商品 80% 是 NULL)。
//
// # 为什么水位全程用字符串传递
//
// SQL Server 的 datetime **不带时区**,`GETDATE()` 返回的是服务器本地时间。
// 若用 Go 的 time.Time 在两侧往返(mssql 驱动把无时区时间按 UTC 解释),
// 扫描时与绑定时的处理只要有一端不一致,水位就会整体偏移一个时区(±8h):
// 偏前 → 漏数据;偏后 → 重复拉。
//
// 所以这里用 ODBC canonical 格式(style 121)在字符串域里完成比较:
//
//	SELECT CONVERT(varchar(30), GETDATE(), 121)  →  "2026-10-10 08:30:00.123"
//	WHERE modify_date >= CAST(@p1 AS datetime)     →  显式 CAST,不猜类型
//
// 整条链路上没有 time.Time 参与,时区问题就不存在 —— 而不是"看起来对"。
//
// # 占位符必须是 @p1,不能是 ?
//
// go-mssqldb 只认 `@p1` 这种具名参数。database/sql 的匿名 `?` 会被它
// **原样**发给 SQL Server,报 `'?': 附近有语法错误 (102)` —— 2026-10-10
// 真机验证时就是这个错误,两个生产库同时炸,而当时全部单测通过。
// 既有先例:pkg/cubequery.numberPlaceholders、cmd/inspect 的 `WHERE TABLE_NAME = @p1`。

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SQLServerTimeLayout 是 ODBC canonical datetime 格式(SQL Server style 121)。
//
// 与 `CONVERT(varchar(30), <datetime>, 121)` 的输出逐字对应,
// 所以同一份代码既能读源库时间,也能格式化本地时间。
const SQLServerTimeLayout = "2006-01-02 15:04:05.000"

// sqlIdentRE 限定可以直接拼进 SQL 的标识符。
//
// 表名 / 增量列名都来自 config.yaml(不在版本库),但它们仍然会被拼进
// SQL 文本。参数占位符只能保护**值**,保护不了标识符,所以这里显式校验:
// 只允许字母数字下划线,其余一律拒绝。
//
// 这不是洁癖:一个写错成 `t_bd_item_info; DROP TABLE ...` 的配置值,
// 后果是静默的数据损坏,而没有任何报错。
var sqlIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validIdent 校验标识符,并返回可直接拼进 SQL 的形式(加方括号)。
func validIdent(name, what string) (string, error) {
	name = strings.TrimSpace(name)
	if !sqlIdentRE.MatchString(name) {
		return "", fmt.Errorf("source: %s %q 非法(只允许字母/数字/下划线,且不以数字开头)", what, name)
	}
	return "[" + name + "]", nil
}

// IncrementalCols 描述一个 model 的增量时间列(主列 + 可选回退列)。
//
// # 为什么需要回退列
//
// `modify_date` 为 NULL **不等于**"这条记录没动过" —— 实测 ysx(hbposv10)
// 商品表 27304 行里有 6787 行(25%)modify_date 为 NULL,其中**近 7 天新建的
// 45 个商品里有 36 个是 NULL**(按 build_date >= DATEADD(day,-7,GETDATE()) 判定)。
//
// 语义上 NULL 的含义是"**建档之后从未被修改过**",那么这条记录最后一次被
// 写入的时间就是它的 `build_date`。用它兜底不会引入时间倒流 —— 实测两个库
// `modify_date < build_date` 的行数都是 **0**,即 modify_date >= build_date 恒成立。
//
// 实测收益(2026-10-10,回看 30 天):
//
//	                  只用 modify_date    COALESCE(modify_date, build_date)
//	ysx(27304 行)              410                        549  (+139)
//	hbposv7(44316 行)          209                        209  (+0)
//
// hbposv7 零收益的原因很具体:它有 **12607 行商品 modify_date 与 build_date
// 同时为 NULL**(`modify_date IS NULL AND build_date IS NOT NULL` 查出来是 0 行),
// 所以 COALESCE 对它完全没有改变。**这类行只能靠夜间全量校准兜底**,本改进
// 不能替代它 —— 12607 行占 hbposv7 全部商品的 28%。
//
// 性能代价(2026-10-10 实测,同连接重复 10 次的墙钟):
//
//	             全表扫描   modify_date 单列   COALESCE 两列
//	ysx              70ms          145ms           173ms
//	hbposv7         730ms          832ms           850ms
//
// 即每次多 2~3 ms —— COALESCE 让两列的索引都用不上,但对四万行的表来说这点
// 代价远小于它省掉的那 139 次"新建商品当天查不到"。
type IncrementalCols struct {
	// Primary 是增量时间主列(如 modify_date),为增量唯一依据。
	Primary string
	// Fallback 是主列为 NULL 时的回退列(如 build_date)。留空则退化成单列比较。
	Fallback string
}

// Expression 返回可直接拼进 SQL 的比较表达式(已加方括号 / COALESCE 包裹)。
//
// 单独抽出来是为了让"到底生成了什么 SQL 文本"可以被单测断言 —— 这段 SQL
// 只会在真机上执行,本地测不到(见下方 incrementalSelect 的说明)。
func (c IncrementalCols) Expression() (string, error) {
	primary, err := validIdent(c.Primary, "增量列名")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(c.Fallback) == "" {
		return primary, nil
	}
	fallback, err := validIdent(c.Fallback, "增量回退列名")
	if err != nil {
		return "", err
	}
	return "COALESCE(" + primary + ", " + fallback + ")", nil
}

// IncrColsFromConfig 把 config.yaml 的两段配置合成一份增量列表。
//
// 合成逻辑只有这一份 —— main.go 与 cmd/incrementalcheck 都要用它,
// 各自拼一遍的话迟早出现"app 配了回退列、验证工具没配"的对不上。
func IncrColsFromConfig(primary, fallback map[string]string) map[string]IncrementalCols {
	out := make(map[string]IncrementalCols, len(primary))
	for model, col := range primary {
		out[model] = IncrementalCols{Primary: col, Fallback: fallback[model]}
	}
	return out
}

// incrementalSelect 构造增量 SELECT 的 SQL 文本。
//
// 刻意抽成纯函数而不是内联在 FetchSince 里:**这段 SQL 之前没有任何单测
// 能覆盖**,因为它直接发给源库。2026-10-10 真机验证时它带着 `?` 占位符
// 在 ysx / hbposv7 两个生产库上同时炸掉,而当时全部单测通过 ——
// 单测抓不到"驱动方言不对"这类 bug,只有把 SQL 文本本身变成可断言的
// 纯函数才拦得住。
func incrementalSelect(table string, cols IncrementalCols, limit int) (string, error) {
	tbl, err := validIdent(table, "表名")
	if err != nil {
		return "", err
	}
	expr, err := cols.Expression()
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		return "", fmt.Errorf("source: 增量拉取的 limit 必须 > 0,实际 %d", limit)
	}
	return fmt.Sprintf("SELECT TOP %d * FROM %s WHERE %s >= CAST(@p1 AS datetime)",
		limit, tbl, expr), nil
}

// FetchSince 拉取增量窗口内的行,最多 limit 行。
//
// since 必须是用 SQLServerTimeLayout 格式化过的字符串(见 SourceNow)。
// 用 `>=` 而不是 `>`:同一时间戳边界上重复拉一次是幂等的,而漏掉边界
// 那一秒的变更就是真丢数据。重复的代价是几行,漏的代价是永久缺数据。
//
// 返回空切片是**正常**的 —— 表示"这个时间窗内没有变更"。
// 调用方必须区别对待空结果与失败,详见 dataloader 的 loadIncremental。
func FetchSince(ctx context.Context, db *sql.DB, table string, cols IncrementalCols, since string, limit int) ([]map[string]any, error) {
	if since == "" {
		return nil, fmt.Errorf("source: 水位为空,增量刷新无从做起(应由调用方先做全量 bootstrap)")
	}
	q, err := incrementalSelect(table, cols, limit)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("source: 增量查询 %s: %w", table, err)
	}
	return ScanRows(rows)
}

// SourceNow 返回源库当前时间,格式化成 SQLServerTimeLayout。
//
// 增量水位**必须**取自源库而不是本地时钟:两者有漂移时水位会落错区间,
// 而"水位落错区间"的后果是静默丢数据 —— 不报错、不降级、healthz 全绿。
func SourceNow(ctx context.Context, db *sql.DB) (string, error) {
	var now string
	if err := db.QueryRowContext(ctx, "SELECT CONVERT(varchar(30), GETDATE(), 121)").Scan(&now); err != nil {
		return "", fmt.Errorf("source: 读源库当前时间: %w", err)
	}
	return now, nil
}

// FormatSQLServerTime 把 Go 时间格式化成能与 SQL Server datetime 比较的形式。
//
// 只用于把**已知语义**的时间(比如"回拨水位做一次重拉")写成字面量;
// 正常推进水位走 SourceNow,不经过 Go 的时钟。
func FormatSQLServerTime(t time.Time) string { return t.Format(SQLServerTimeLayout) }

// ParseSQLServerTime 解析 style 121 格式,供"读回水位再解析"的场景使用。
//
// 解析失败返回错误而不是零值:零值时间传给 SQL 会变成 0001-01-01,
// 后果是"一次性拉回全表"——看起来只是慢,实际是把增量静默降级成全量。
func ParseSQLServerTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	t, err := time.ParseInLocation(SQLServerTimeLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("source: 解析水位 %q(期望格式 %s): %w",
			s, SQLServerTimeLayout, err)
	}
	return t, nil
}
