// time.go 时间维度(timeDimensions)与日期过滤的编译。
//
// **为什么单独成文件**:dateRange / granularity 都要用到"按方言生成日期表达式",
// 而 DuckDB 与 T-SQL 的写法完全不同(DATE_TRUNC vs DATEFROMPARTS、
// now() vs GETDATE())。这些差异必须收敛在这里,由 build.go / tsql.go 各自
// 传一个 timeDialect 进来 —— 与 buildOrder 同一套路:共用语义、方言不同。
//
// ⚠️ 思迅的日期列是 **char(10) 'YYYY-MM-DD' 字符串**,不是 datetime
// (t_rm_daysum.oper_date 实测如此)。这带来一个性能上的关键分叉:
//
//	dimension 标了 char_date: true → 过滤直接对**原列**做字符串比较
//	                            → sargable,SQL Server 能用索引
//	没标(真 datetime 列)          → 必须 CAST 后比较,全表扫描
//
// ISO 日期的字典序等于时间序,所以字符串比较在 char_date 上是**正确**的,
// 不是近似。我们实测过两库都是 'YYYY-MM-DD' 零填充,才敢这么用。
package cubequery

import (
	"errors"
	"fmt"
	"strings"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// timeDialect 是日期相关的方言差异集合。
type timeDialect struct {
	name string
	// nowExpr 返回"今天"的表达式(不含引号)。
	nowExpr string
	// toDate 把列转成 date 类型,用于 granularity 截断。
	toDate func(col string) string
	// truncate 按粒度截断(传入已 toDate 的表达式)。
	truncate func(dateExpr, granularity string) string
	// shift 把日期表达式前后平移。**必须方言化**:T-SQL 的 DATEADD 在
	// DuckDB 里不存在,反过来 INTERVAL 也不是 T-SQL 语法 ——
	// 写成其中一套就等于给另一条路径生成非法 SQL。
	shift func(dateExpr, unit string, n int) string
	// legacy 为真时只能用 SQL Server 2008 及更早支持的写法
	// (DATEFROMPARTS 是 2012+ 才有的)。
	legacy bool
}

var duckTime = timeDialect{
	name:    "duckdb",
	nowExpr: "CURRENT_DATE",
	toDate:  func(col string) string { return "CAST(" + col + " AS DATE)" },
	truncate: func(d, g string) string {
		return "DATE_TRUNC('" + g + "', " + d + ")"
	},
	shift: func(expr, unit string, n int) string {
		sign := "+"
		if n < 0 {
			sign, n = "-", -n
		}
		// 单位统一大写:DuckDB 的 interval 单位大小写不敏感,但生成的 SQL
		// 要跟人写的样子一致,否则日志里 `INTERVAL 1 day` 看着像 bug。
		return expr + " " + sign + " INTERVAL " + fmt.Sprint(n) + " " + strings.ToUpper(unit)
	},
}

var tsqlTime = timeDialect{
	name:     "tsql",
	nowExpr:  "CAST(GETDATE() AS DATE)",
	toDate:   func(col string) string { return "CAST(" + col + " AS DATETIME)" },
	truncate: tsqlTruncate(false),
	shift: func(expr, unit string, n int) string {
		return fmt.Sprintf("DATEADD(%s, %d, %s)", unit, n, expr)
	},
}

// tsqlLegacyTime 是 SQL Server 2008(compat level 80)方言。
//
// **实测得来的**,不是推测:2026-10-09 拿编译出的 SQL 打 hbposv7(hbposepro,
// compat level 80),`DATEFROMPARTS` 报"不是可以识别的内置函数名称" ——
// DATEFROMPARTS 是 SQL Server 2012 才引入的。同一产品线的 ysx 是 2014,支持。
//
// DATEFROMPARTS 这类差异**必须做成方言闭包而不是包级开关**:同一进程里
// 可能同时编译连不同实例的查询,用全局变量切方言会互相污染,
// 而且并发编译时是数据竞争。
var tsqlLegacyTime = timeDialect{
	name:     "tsql-2008",
	nowExpr:  "CAST(GETDATE() AS DATE)",
	toDate:   func(col string) string { return "CAST(" + col + " AS DATETIME)" },
	truncate: tsqlTruncate(true),
	legacy:   true,
	shift: func(expr, unit string, n int) string {
		return fmt.Sprintf("DATEADD(%s, %d, %s)", unit, n, expr)
	},
}

// tsqlTruncate 返回一个 T-SQL 粒度截断函数。legacy=true 时只用 2005 就有的
// DATEADD/DATEDIFF 写法。
func tsqlTruncate(legacy bool) func(d, g string) string {
	return func(d, g string) string {
		switch g {
		case "day":
			return "CAST(" + d + " AS DATE)"
		case "week":
			// DATEDIFF(week, 0, d) 给"周数",DATEADD 回该周的周一(1900-01-01 是周一)。
			return "DATEADD(week, DATEDIFF(week, 0, " + d + "), 0)"
		case "month":
			if legacy {
				return "DATEADD(month, DATEDIFF(month, 0, " + d + "), 0)"
			}
			return "DATEFROMPARTS(YEAR(" + d + "), MONTH(" + d + "), 1)"
		case "quarter":
			if legacy {
				first := "DATEADD(month, DATEDIFF(month, 0, " + d + "), 0)"
				return "DATEADD(month, 3 * ((MONTH(" + d + ") - 1) / 3), " + first + ")"
			}
			return "DATEFROMPARTS(YEAR(" + d + "), 3 * ((MONTH(" + d + ") - 1) / 3) + 1, 1)"
		case "year":
			if legacy {
				return "DATEADD(year, DATEDIFF(year, 0, " + d + "), 0)"
			}
			return "DATEFROMPARTS(YEAR(" + d + "), 1, 1)"
		}
		return d
	}
}

// validGranularities 是支持的粒度。刻意**不支持 hour/minute/second**:
// 思迅是零售日结业务,按秒聚合没有意义,而每个多支持的粒度都是一处能写错的地方。
var validGranularities = map[string]bool{
	"day": true, "week": true, "month": true, "quarter": true, "year": true,
}

// dateRange 是解析后的绝对日期区间。
type dateRange struct {
	start string // YYYY-MM-DD,闭区间
	end   string
}

// parseDateRange 解析 dateRange 字段。
//
// 支持三类,其它一律报错 —— **绝不静默忽略一个看不懂的 dateRange**:
//  1. 绝对区间:["2026-07-01", "2026-09-30"] 或单值 "2026-07-01"(当天)
//  2. 相对区间:["Last 30 days"]、"last_30_days" 等关键字
//  3. 绝对单值数组:["2026-07-01"]
//
// 相对区间**不产生参数占位符**,直接展开成日期表达式 —— 关键字是白名单,
// 不含用户文本,不构成注入面。
func parseDateRange(v any, d timeDialect) (*dateRange, []string, error) {
	switch val := v.(type) {
	case string:
		if r, ok := relativeRange(val, d); ok {
			return nil, r, nil
		}
		if !isISODate(val) {
			return nil, nil, fmt.Errorf("dateRange %q 不是 ISO 日期(YYYY-MM-DD),也不是受支持的相对关键字", val)
		}
		return &dateRange{start: val, end: val}, nil, nil
	case []any:
		if len(val) == 0 {
			return nil, nil, errors.New("dateRange 是空数组")
		}
		if len(val) == 1 {
			return parseDateRange(val[0], d)
		}
		if len(val) != 2 {
			return nil, nil, fmt.Errorf("dateRange 数组长度应为 1 或 2,got %d", len(val))
		}
		start, ok1 := val[0].(string)
		end, ok2 := val[1].(string)
		if !ok1 || !ok2 {
			return nil, nil, errors.New("dateRange 的两个元素都必须是 YYYY-MM-DD 字符串")
		}
		if !isISODate(start) || !isISODate(end) {
			return nil, nil, fmt.Errorf("dateRange 必须是 YYYY-MM-DD,got [%s, %s]", start, end)
		}
		if start > end {
			// 区间反了是调用方的错,但静默返回空集会被当成"该时段没有业务"。
			return nil, nil, fmt.Errorf("dateRange 起点晚于终点:[%s, %s]", start, end)
		}
		return &dateRange{start: start, end: end}, nil, nil
	case nil:
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("dateRange 类型不支持:%T", v)
	}
}

// relativeRange 把受支持的相对关键字展开成 SQL 日期表达式。
// 返回 (exprs, true) 表示命中白名单。**只认白名单**,否则调用方拼个
// "last_3_business_days" 就会被静默忽略 —— 那是"查了全历史却以为是近期"。
func relativeRange(s string, d timeDialect) ([]string, bool) {
	norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", "_"))
	now := d.nowExpr
	switch norm {
	case "today":
		return []string{now, now}, true
	case "yesterday":
		y := d.shift(now, "day", -1)
		return []string{y, y}, true
	case "last_7_days", "last_7_days_to_date":
		return []string{d.shift(now, "day", -6), now}, true
	case "last_30_days", "last_30_days_to_date":
		return []string{d.shift(now, "day", -29), now}, true
	case "last_90_days", "last_90_days_to_date":
		return []string{d.shift(now, "day", -89), now}, true
	case "this_month":
		return []string{d.truncate(now, "month"), now}, true
	case "last_month":
		prev := d.shift(now, "month", -1)
		return []string{d.truncate(prev, "month"), d.truncate(now, "month")}, true
	case "this_year":
		return []string{d.truncate(now, "year"), now}, true
	}
	return nil, false
}

func isISODate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// buildTimeWhere 生成一条时间过滤 SQL 片段。
//
// charDate=true 时对**原列**做字符串比较(sargable,能走索引);
// 否则 CAST 成日期再比 —— 正确性优先。
func buildTimeWhere(colSQL string, charDate bool, dr *dateRange, relExprs []string, d timeDialect) (string, []any, error) {
	col := TrimExpr(colSQL)
	target := col
	if !charDate {
		target = d.toDate(col)
	}
	if relExprs != nil {
		// 相对区间:关键字展开成的表达式,无用户文本。
		if len(relExprs) != 2 {
			return "", nil, errors.New("内部错误:相对区间应有 2 个表达式")
		}
		return target + " >= " + relExprs[0] + " AND " + target + " <= " + relExprs[1], nil, nil
	}
	if dr == nil {
		return "", nil, nil
	}
	// 绝对区间:值走参数绑定,永不进 SQL 文本。
	return target + " >= ? AND " + target + " <= ?", []any{dr.start, dr.end}, nil
}

// timeGroupExpr 按 granularity 生成分组表达式(带 RTRIM 保护 char 列)。
func timeGroupExpr(colSQL, granularity string, d timeDialect) (string, error) {
	if !validGranularities[granularity] {
		return "", fmt.Errorf("unsupported granularity %q (want day|week|month|quarter|year)", granularity)
	}
	// 先 RTRIM:char 定长列补空格会让 CAST 结果带时间部分,截断不准。
	return d.truncate(d.toDate(TrimExpr(colSQL)), granularity), nil
}

// timePlan 是 timeDimensions 的编译产物,由 Build / BuildTSQL 追加到各自的
// selectList / groupBy / whereParts 上。
type timePlan struct {
	selectList []string
	groupBy    []string
	whereParts []string
	args       []any
	dimRefs    []string
}

// planTimeDimensions 编译 q.TimeDimensions。
//
// exprOf 由调用方提供**已方言化**的列表达式(Build 直接取 d.SQL;
// BuildTSQL 要先经 rewriteColumns 把 canonical 名换成源库列名)——
// 这是两条路径唯一真正的差别,其余语义共用。
//
// 语义要点:
//   - 只有 dateRange → 纯过滤,不进 SELECT/GROUP BY
//   - 只有 granularity → 进 SELECT + GROUP BY,输出 key 是 `<model>.<dim>.<gran>`
//   - 两者都有 → 过滤 + 分组
func planTimeDimensions(
	q *Query,
	schema *cubeschema.Model,
	modelName string,
	exprOf func(*cubeschema.Dimension) string,
	aliasOf func(string) string,
	d timeDialect,
) (*timePlan, error) {
	plan := &timePlan{}
	for _, td := range q.TimeDimensions {
		if strings.TrimSpace(td.Dimension) == "" {
			return nil, &BuildError{Kind: "dimension", Msg: "timeDimensions[].dimension is empty"}
		}
		ref := strings.TrimPrefix(td.Dimension, modelName+".")
		dim, ok := schema.FindDimension(ref)
		if !ok {
			return nil, &BuildError{Kind: "dimension", Ref: ref,
				Msg: "time dimension not found: " + ref}
		}
		if dim.Type != cubeschema.TypeTime {
			return nil, &BuildError{Kind: "dimension", Ref: ref,
				Msg: "timeDimensions 只能引用 type: time 的维度,got " + string(dim.Type) +
					" (" + ref + ");把它改成 type: time,或改用普通 dimensions + filter"}
		}
		expr := exprOf(dim)

		if td.DateRange != nil {
			dr, relExprs, err := parseDateRange(td.DateRange, d)
			if err != nil {
				return nil, &BuildError{Kind: "filter", Ref: ref,
					Msg: "dateRange: " + err.Error()}
			}
			where, args, err := buildTimeWhere(expr, dim.CharDate, dr, relExprs, d)
			if err != nil {
				return nil, &BuildError{Kind: "filter", Ref: ref,
					Msg: "dateRange: " + err.Error()}
			}
			if where != "" {
				plan.whereParts = append(plan.whereParts, where)
				plan.args = append(plan.args, args...)
			}
		}

		if strings.TrimSpace(td.Granularity) != "" {
			gexpr, err := timeGroupExpr(expr, td.Granularity, d)
			if err != nil {
				return nil, &BuildError{Kind: "dimension", Ref: ref, Msg: err.Error()}
			}
			outRef := ref + "." + td.Granularity
			plan.selectList = append(plan.selectList, gexpr+" AS "+aliasOf(modelName+"."+outRef))
			plan.groupBy = append(plan.groupBy, gexpr)
			plan.dimRefs = append(plan.dimRefs, outRef)
		}

		// 两者都没给 → 什么都不产出。这不是"静默忽略":调用方拿到了**正确但更宽**
		// 的结果集,没有任何过滤被假装执行。要让它有意义就得给 dateRange 或
		// granularity;真要拦可以在 schema 校验里要求两者至少给一个。
	}
	return plan, nil
}
