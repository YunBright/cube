package source

// 增量 SQL 构造的回归测试。
//
// # 这里为什么必须存在
//
// 2026-10-10 真机验证(`cmd/incrementalcheck`,在真库 + 真 libduckdb 上跑)时,
// 增量查询在 **ysx 和 hbposv7 两个生产库上同时**报:
//
//	mssql: '?' 附近有语法错误。 (102)
//
// 而在那之前这个包**一条单测都没有**,全部单测通过 —— 因为原来的 SQL
// 直接内联在 FetchSince 里发给源库,单测永远碰不到它。
//
// 这类 bug 有一个共同特征:**只有真机能发现,而真机验证的成本很高**
// (交叉编译 + scp + 组织目录 + 连生产库)。所以凡是真机抓到一次、
// 原因又完全可以在本地断言的,都必须留下一条回归锁,否则同一个坑会被
// 下一个人再踩一遍。

import (
	"strings"
	"testing"
	"time"
)

// TestIncrementalSelectHasNoBareQuestionMark 是本次事故的直接回归锁。
//
// go-mssqldb 只认 `@p1` 这种具名参数;database/sql 的匿名 `?` 会被
// **原样**发给 SQL Server,当场报 102 语法错误。`pkg/cubequery` 里已经有
// 一条等价防护(`numberPlaceholders`),但它只管自己的产物,管不到这里 ——
// 这正是当初能绕过去的原因。
func TestIncrementalSelectHasNoBareQuestionMark(t *testing.T) {
	q, err := incrementalSelect("t_bd_item_info", IncrementalCols{Primary: "modify_date"}, 50000)
	if err != nil {
		t.Fatalf("incrementalSelect: %v", err)
	}
	if strings.Contains(q, "?") {
		t.Fatalf("SQL 里有裸 `?` 占位符,go-mssqldb 会原样发给 SQL Server 报 102 语法错误,实际: %s", q)
	}
	if !strings.Contains(q, "@p1") {
		t.Errorf("应使用具名占位符 @p1,实际: %s", q)
	}
}

// TestIncrementalSelectShape 锁住整条 SQL 的形状。
//
// 逐字断言而不是"包含某个子串":TOP 的数值、方括号、CAST 目标类型
// 任一处写错都会让结果**静默不对**(比如漏 CAST 时字符串比较仍能跑通,
// 只是比较规则变了),而这类错误不会报错。
func TestIncrementalSelectShape(t *testing.T) {
	q, err := incrementalSelect("t_bd_item_info", IncrementalCols{Primary: "modify_date"}, 12345)
	if err != nil {
		t.Fatalf("incrementalSelect: %v", err)
	}
	want := "SELECT TOP 12345 * FROM [t_bd_item_info] WHERE [modify_date] >= CAST(@p1 AS datetime)"
	if q != want {
		t.Errorf("SQL 不符预期\n got: %s\nwant: %s", q, want)
	}
}

// TestIncrementalSelectUsesGreaterOrEqual 锁住 `>=` 而不是 `>`。
//
// 边界上重复拉一次是幂等的(upsert 覆盖同主键),漏掉边界那一秒的变更
// 是永久丢数据。改回 `>` 会让水位正好落在变更时间戳上的行被永久跳过。
func TestIncrementalSelectUsesGreaterOrEqual(t *testing.T) {
	q, err := incrementalSelect("t_bd_item_info", IncrementalCols{Primary: "modify_date"}, 10)
	if err != nil {
		t.Fatalf("incrementalSelect: %v", err)
	}
	if !strings.Contains(q, ">= CAST(@p1") {
		t.Errorf("水位比较必须是 >=,实际: %s", q)
	}
	if strings.Contains(q, "> CAST(@p1") {
		t.Errorf("出现了裸 `>`,漏边界就是永久丢数据,实际: %s", q)
	}
}

// TestIncrementalSelectRejectsBadIdentifiers 锁住标识符白名单。
//
// 参数占位符只保护**值**,表名/列名是拼进 SQL 文本的 —— 配置写错成
// `t_bd_item_info; DROP TABLE x` 时没有任何注入防护。这些值来自
// config.yaml(不在版本库),但仍然必须校验。
func TestIncrementalSelectRejectsBadIdentifiers(t *testing.T) {
	bad := []struct {
		name       string
		table      string
		col        string
		whyItBites string
	}{
		{"注入语句", "t_bd_item_info; DROP TABLE t_bd_item_info--", "modify_date", "配置注入"},
		{"含空格", "t_bd_item info", "modify_date", "带空格的表名"},
		{"首位数字", "1t_bd_item_info", "modify_date", "非法标识符"},
		{"空表名", "", "modify_date", "空名字"},
		{"含方括号", "[t_bd_item_info]", "modify_date", "已加括号的写法会被双重包裹"},
		{"列名注入", "t_bd_item_info", "modify_date) OR 1=1--", "列名注入"},
		{"列名为空", "t_bd_item_info", "", "空列名"},
		{"列名带空格", "t_bd_item_info", "modify date", "带空格的列名"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			q, err := incrementalSelect(tc.table,
				IncrementalCols{Primary: tc.col}, 100)
			if err == nil {
				t.Errorf("%s 应被拒绝(%s),却构造出了 SQL: %s", tc.name, tc.whyItBites, q)
			}
		})
	}
}

// TestIncrementalSelectRejectsNonPositiveLimit 锁住 limit 校验。
//
// limit<=0 若放行,SQL 变成 `SELECT TOP 0 *`(看起来"成功返回 0 行"),
// 调用方会把它当成"这个时间窗内没有变更"——于是水位照常推进,
// 那一整个窗口的变更被**静默丢弃**,且没有任何报错。
func TestIncrementalSelectRejectsNonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		q, err := incrementalSelect("t_bd_item_info",
			IncrementalCols{Primary: "modify_date"}, limit)
		if err == nil {
			t.Errorf("limit=%d 应被拒绝,却构造出了 SQL: %s", limit, q)
		}
	}
}

// TestSQLServerTimeLayoutMatchesODBCStyle121 锁住格式常量。
//
// 水位全程在**字符串域**里传递就是为了绕开 time.Time 与 SQL Server
// 无时区 datetime 的往返解释(±8h 偏移)。这个常量一旦被改,
// `GETDATE()` 读回来的字符串与本地格式化出来的字符串就再也不能互相比较,
// 而失效方式是"静默拉不到数据",不是报错。
func TestSQLServerTimeLayoutMatchesODBCStyle121(t *testing.T) {
	if SQLServerTimeLayout != "2006-01-02 15:04:05.000" {
		t.Fatalf("SQLServerTimeLayout 必须与 CONVERT(varchar(30), GETDATE(), 121) 的输出逐字对应,实际: %s",
			SQLServerTimeLayout)
	}
	got := FormatSQLServerTime(time.Date(2026, 10, 10, 8, 30, 0, 123000000, time.UTC))
	want := "2026-10-10 08:30:00.123"
	if got != want {
		t.Errorf("格式化结果不符 ODBC style 121\n got: %s\nwant: %s", got, want)
	}
}

// TestParseSQLServerTimeRoundTrip 保证写入与读回用的是同一个格式。
func TestParseSQLServerTimeRoundTrip(t *testing.T) {
	orig := "2026-10-10 08:30:00.123"
	parsed, err := ParseSQLServerTime(orig)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", orig, err)
	}
	if back := FormatSQLServerTime(parsed); back != orig {
		t.Errorf("往返后不一致\n got: %s\nwant: %s", back, orig)
	}
}

// TestParseSQLServerTimeRejectsGarbage 锁住"解析失败必须报错"这条约定。
//
// 返回零值时间听起来无害,但零值传给 SQL 会变成 0001-01-01,
// 后果是**一次性拉回全表**——表现只是"变慢了",增量被静默降级成全量,
// 而 healthz 全绿。这里断言的是"拒绝脏输入",不是"解析得好"。
func TestParseSQLServerTimeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"",
		"abc",
		"2026-10-10",                 // 只有日期,缺时分秒
		"2026/10/10 08:30:00",        // 斜杠分隔不是 style 121
		"2026-10-10T08:30:00.123",    // RFC3339 不是 style 121
		"2026-10-10 08:30:00.123456", // 微秒精度比 layout 多
	} {
		if got, err := ParseSQLServerTime(bad); err == nil {
			t.Errorf("脏输入 %q 应被拒绝(否则零值会变成 0001-01-01 → 静默拉全表),却解析成了 %s", bad, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 增量回退列(COALESCE)
//
// 背景:实测 ysx 近 7 天按 build_date 判定的 45 个新商品里,36 个 modify_date
// 是 NULL —— 单靠 modify_date 增量会漏掉它们,而它们恰恰是**最需要被拉到**的
// 新数据(旧数据早就被上一次全量带进来了)。用 build_date 兜底能救回 139 行
// (30 天窗口,实测 410 → 549)。
//
// ⚠️ 但这**不能替代夜间全量**:hbposv7 有 12607 行商品两个时间列同时为 NULL。
// ---------------------------------------------------------------------------

// TestIncrementalSelectWithFallbackUsesCoalesce 锁住 COALESCE 的形态。
//
// 断言**参数顺序**:COALESCE(a, b) 写反语义就变了(变成"优先用 build_date"),
// 而写反之后 SQL 依然能跑、结果依然合理 —— 只有对照实测数据才看得出不对。
// 所以这里逐字断言。
func TestIncrementalSelectWithFallbackUsesCoalesce(t *testing.T) {
	q, err := incrementalSelect("t_bd_item_info",
		IncrementalCols{Primary: "modify_date", Fallback: "build_date"}, 50000)
	if err != nil {
		t.Fatalf("incrementalSelect: %v", err)
	}
	want := "SELECT TOP 50000 * FROM [t_bd_item_info] WHERE " +
		"COALESCE([modify_date], [build_date]) >= CAST(@p1 AS datetime)"
	if q != want {
		t.Errorf("SQL 不符预期\n got: %s\nwant: %s", q, want)
	}
}

// TestIncrementalSelectWithoutFallbackStaysSingleColumn 锁住向后兼容。
//
// 没配回退列时必须与改造前**逐字相同** —— config.yaml 是本地文件,不是每个
// 部署实例都会立刻补上 incremental_fallback。
func TestIncrementalSelectWithoutFallbackStaysSingleColumn(t *testing.T) {
	for _, cols := range []IncrementalCols{
		{Primary: "modify_date", Fallback: ""},
		{Primary: "modify_date", Fallback: "   "}, // 全空白视为未配
	} {
		q, err := incrementalSelect("t_bd_item_info", cols, 100)
		if err != nil {
			t.Fatalf("incrementalSelect: %v", err)
		}
		if strings.Contains(q, "COALESCE") {
			t.Errorf("未配回退列时不应出现 COALESCE(会白让两列索引失效),实际: %s", q)
		}
		want := "SELECT TOP 100 * FROM [t_bd_item_info] WHERE [modify_date] >= CAST(@p1 AS datetime)"
		if q != want {
			t.Errorf("未配回退列时 SQL 应与改造前逐字相同\n got: %s\nwant: %s", q, want)
		}
	}
}

// TestIncrementalSelectRejectsBadFallback 回退列同样要过标识符白名单。
//
// 加了回退列就等于把注入面从一列扩到两列 —— 只校验主列的话,
// `incremental_fallback.product` 会变成一个没被检查的入口。
func TestIncrementalSelectRejectsBadFallback(t *testing.T) {
	for _, bad := range []string{
		"build_date) OR 1=1--",
		"build_date; DROP TABLE t_bd_item_info--",
		"build date",
		"1bad",
	} {
		q, err := incrementalSelect("t_bd_item_info",
			IncrementalCols{Primary: "modify_date", Fallback: bad}, 100)
		if err == nil {
			t.Errorf("非法回退列 %q 应被拒绝(它是未被校验的注入入口),却构造出了: %s", bad, q)
		}
	}
}

// TestIncrementalColsRequiresPrimary 锁住"主列不能为空"。
//
// 只配了 incremental_fallback 没配 incremental 时,map 里有这个 model
// 但 Primary 是空串 —— 生成的 SQL 会是 `WHERE [] >= ...`,当场语法错误。
func TestIncrementalColsRequiresPrimary(t *testing.T) {
	if _, err := incrementalSelect("t_bd_item_info",
		IncrementalCols{Primary: "", Fallback: "build_date"}, 100); err == nil {
		t.Error("主列为空时应被拒绝(否则生成 `WHERE [] >= ...` 的语法错误 SQL)")
	}
}

// TestIncrColsFromConfig 锁住配置合成逻辑。
//
// main.go 与 cmd/incrementalcheck 都调它 —— 它必须是唯一一份,否则会出现
// "app 配了回退列、验证工具没配",验的就不是线上在跑的 SQL 了。
func TestIncrColsFromConfig(t *testing.T) {
	got := IncrColsFromConfig(
		map[string]string{"product": "modify_date", "supplier": "modify_date"},
		map[string]string{"product": "build_date"},
	)
	if len(got) != 2 {
		t.Fatalf("应合成 2 个 model,实际 %d: %v", len(got), got)
	}
	p := got["product"]
	if p.Primary != "modify_date" || p.Fallback != "build_date" {
		t.Errorf("product 应得到 (modify_date, build_date),实际 (%q, %q)", p.Primary, p.Fallback)
	}
	// supplier 没配回退列 → Fallback 必须为空(而不是 map 里不存在的零值之外的怪东西)。
	if s := got["supplier"]; s.Primary != "modify_date" || s.Fallback != "" {
		t.Errorf("supplier 应得到 (modify_date, \"\"),实际 (%q, %q)", s.Primary, s.Fallback)
	}
	// primary 为空的 model 不该被合成进来 —— 它没法做增量。
	if _, ok := got["category"]; ok {
		t.Error("incremental 里没有的 model 不该出现在结果里")
	}
}
