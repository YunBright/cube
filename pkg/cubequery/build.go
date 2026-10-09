// build.go 把 Query 编译成 DuckDB SQL。
//
// **为什么在共用包里**:这段逻辑曾各写一份 —— sixun-ysx 的 main.go 是完整实现
// (多 measure / 多 dimension / filters / RTRIM),sixun-hbposv7 的 main.go 却是一套
// 残缺的 MVP:只取 Measures[0] 和 Dimensions[0]、**完全忽略 Filters**、LIMIT 硬编码、
// 没有 RTRIM。
//
// 后果不是"功能少",而是**返回错误的数据**:带 filter 的查询在 hbposv7 上被静默丢弃,
// SELECT 于是变成"取前 1000 行",而 supertrade 的 GetProduct 直接取 data[0]
// → 扫一个条码可能拿到**另一个商品**的库存,而且 200 OK、零报错。
// hbposv7 的 t_bd_item_info.id 本身就是 char 定长补空格("6922303199721       "),
// 少个 RTRIM 就让该门店的条码搜索永远匹配不上。
//
// 两个 family 共用 sixun-models 下的同一份 schema,查询语义**必须**一致,
// 否则"同一套 API 在不同门店表现不同"这类问题会反复出现。
package cubequery

import (
	"fmt"
	"strings"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// DefaultLimit 是未指定 limit 时的默认返回行数。
const DefaultLimit = 1000

// BuildError 是可被上层翻译成 4xx 子码的构建期错误。
//
// Kind 的语义分两类,上层必须区别对待:
//   - "filter" / "order" / "query" —— **调用方的查询写错了** → 400 QUERY_INVALID
//   - "dimension" / "measure" / "model" —— 请求的成员/模型不存在 → 404 MODEL_NOT_FOUND
//
// 把后者报成前者(或反过来)会让调用方查错方向。
type BuildError struct {
	Kind string // 见上
	Ref  string // 出错的 member 名(已剥 model 前缀)
	Msg  string // 面向人的说明
}

func (e *BuildError) Error() string { return e.Msg }

// Result 是一次成功编译的结果。
type Result struct {
	SQL         string
	Args        []any
	DimRefs     []string // bare ref(已剥 "<model>." 前缀)
	MeasureRefs []string
}

// Build 编译 Query → DuckDB SQL。
//
// 两个刻意的设计:
//  1. 简单列名统一包 RTRIM —— 思迅的 char 定长列普遍补空格, ysx / hbposv7 都中招。
//     只在**维度表达式**和 **filter 两侧**包, GROUP BY 用原列(SQL 允许
//     GROUP BY id 而 SELECT RTRIM(id));不包 measure 的聚合表达式。
//  2. filter 值一律走占位符参数绑定,值永远不被拼进 SQL 文本。
func Build(q *Query, schema *cubeschema.Model) (*Result, error) {
	modelName := q.Model()
	if modelName == "" {
		return nil, &BuildError{Kind: "model", Msg: "cannot infer model"}
	}
	if len(q.Measures) == 0 {
		return nil, &BuildError{Kind: "measure", Msg: "at least one measure required"}
	}

	res := &Result{DimRefs: []string{}, MeasureRefs: []string{}}

	var selectList []string
	var groupBy []string
	for _, raw := range q.Dimensions {
		ref := strings.TrimPrefix(raw, modelName+".")
		d, ok := schema.FindDimension(ref)
		if !ok {
			return nil, &BuildError{Kind: "dimension", Ref: ref,
				Msg: "dimension not found: " + ref}
		}
		selectList = append(selectList, fmt.Sprintf("%s AS %q", TrimExpr(d.SQL), modelName+"."+ref))
		// GROUP BY 用原列:SQL 允许 GROUP BY id 配 SELECT RTRIM(id)。
		groupBy = append(groupBy, d.SQL)
		res.DimRefs = append(res.DimRefs, ref)
	}

	for _, raw := range q.Measures {
		ref := strings.TrimPrefix(raw, modelName+".")
		ms, ok := schema.FindMeasure(ref)
		if !ok {
			return nil, &BuildError{Kind: "measure", Ref: ref,
				Msg: "measure not found: " + ref}
		}
		selectList = append(selectList, fmt.Sprintf("%s AS %q", ms.SQL, modelName+"."+ref))
		res.MeasureRefs = append(res.MeasureRefs, ref)
	}

	var whereParts []string
	for _, f := range q.Filters {
		ref := strings.TrimPrefix(f.Member, modelName+".")
		d, ok := schema.FindDimension(ref)
		if !ok {
			return nil, &BuildError{Kind: "filter", Ref: ref,
				Msg: "filter dimension not found: " + ref}
		}
		expr, args, err := buildFilter(d.SQL, d.CharDate, f.Operator, f.Values, duckTime)
		if err != nil {
			return nil, &BuildError{Kind: "filter", Ref: ref,
				Msg: "filter: " + err.Error()}
		}
		whereParts = append(whereParts, expr)
		res.Args = append(res.Args, args...)
	}

	// segments 与 filters 一起进 WHERE。segment 不带参数,所以顺序不影响参数绑定。
	segParts, err := expandSegments(q, schema, func(s string) string { return s })
	if err != nil {
		return nil, err
	}
	whereParts = append(whereParts, segParts...)

	// timeDimensions 在 filters 之后处理,whereParts 与 res.Args 的追加顺序
	// 必须严格一致 —— 参数是按下标绑定的,两者错位会让过滤值串到别的列上。
	plan, err := planTimeDimensions(q, schema, modelName,
		func(d *cubeschema.Dimension) string { return d.SQL },
		quoteDouble, duckTime)
	if err != nil {
		return nil, err
	}
	selectList = append(selectList, plan.selectList...)
	groupBy = append(groupBy, plan.groupBy...)
	whereParts = append(whereParts, plan.whereParts...)
	res.Args = append(res.Args, plan.args...)
	res.DimRefs = append(res.DimRefs, plan.dimRefs...)

	// total 用 window function 而不是第二次查询:对 live model 意味着少打一次
	// 源库往返,而源库是思迅的生产库。COUNT(*) OVER() 在 GROUP BY 之后、
	// LIMIT 之前求值,语义正是"去掉 limit/offset 后的行数"。
	if q.Total {
		selectList = append(selectList, "COUNT(*) OVER() AS "+quoteDouble(TotalField))
	}

	sql := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectList, ", "), schema.SQLTable)
	if len(whereParts) > 0 {
		sql += " WHERE " + strings.Join(whereParts, " AND ")
	}
	if len(groupBy) > 0 {
		sql += " GROUP BY " + strings.Join(groupBy, ", ")
	}
	// ORDER BY 必须在 LIMIT **之前** —— 放错位置的 SQL 是合法的,但语义变成
	// "先截断再排序",返回的是任意 1000 行里最大的 10 个,而不是全部里最大的 10 个。
	orderBy, err := buildOrder(q, schema, modelName, quoteDouble, plan.dimRefs)
	if err != nil {
		return nil, err
	}
	sql += orderBy
	limit := DefaultLimit
	if q.Limit != nil && *q.Limit > 0 {
		limit = *q.Limit
	}
	sql += fmt.Sprintf(" LIMIT %d", limit)
	if off := q.Offset.First(); off > 0 {
		sql += fmt.Sprintf(" OFFSET %d", off)
	}
	res.SQL = sql
	return res, nil
}

// quoteDouble 是 DuckDB 的标识符引用,与上面 SELECT 的 %q 保持逐字一致。
func quoteDouble(s string) string { return fmt.Sprintf("%q", s) }

// quoteBracket 是 T-SQL 的标识符引用,与 BuildTSQL 的 [..] 保持逐字一致。
func quoteBracket(s string) string { return "[" + s + "]" }

// buildOrder 编译 q.Order → " ORDER BY ..." 片段。两个方言**共用这一份实现**,
// 只靠 quote 参数区分别名引号(DuckDB "x" / T-SQL [x])。
//
// timeDimRefs 是 timeDimensions 按 granularity 生成的**合成 ref**
// (如 oper_date.month)。它们不在 schema 里,但确实在 SELECT 里 ——
// "按月汇总、按月倒序"是最自然的用法,不能因为它没写进 schema.yaml 就排不了。
//
// 为什么共用:build.go 顶部记着 hbposv7 那套残缺实现的教训 —— 同一份契约
// 各写一份必然分叉,而分叉只在真实数据上才暴露。这里多写一遍的代价是
// "duck 有 order、live 没有",结算单列表在两个 family 上排序结果不一致。
//
// 排序按 **SELECT 别名**而不是原始表达式,两个理由:
//  1. 别名已经过 schema 校验,用户输入不可能混进 SQL 文本;
//  2. 维度是 RTRIM(col)、度量是聚合,重推一遍极易与 SELECT 处不一致
//     (而 GROUP BY 里排的是原列,SELECT 里排的是 RTRIM 列,正是 build.go
//     顶部描述的 char 补空格坑)。
func buildOrder(q *Query, schema *cubeschema.Model, modelName string, quote func(string) string, timeDimRefs []string) (string, error) {
	if len(q.Order) == 0 {
		// 分页必须在**稳定排序**上。DuckDB 允许无 ORDER BY 的 OFFSET,
		// T-SQL 则直接语法报错(SQL Server 要求 OFFSET/FETCH 配 ORDER BY) ——
		// 与其让两个方言一个能跑一个报错、或都跑但翻页结果不稳定,
		// 不如在编译期把这条约束显式化:这是"能不能翻页"的前置条件,不是可选优化。
		if off := q.Offset.First(); off > 0 {
			return "", &BuildError{Kind: "query",
				Msg: fmt.Sprintf("offset=%d 需要显式 order:分页必须建立在稳定排序上,"+
					"否则每页可能重复或漏行", off)}
		}
		return "", nil
	}
	selected := make(map[string]bool, len(q.Dimensions)+len(q.Measures)+len(timeDimRefs))
	for _, raw := range q.Dimensions {
		selected[strings.TrimPrefix(raw, modelName+".")] = true
	}
	for _, raw := range q.Measures {
		selected[strings.TrimPrefix(raw, modelName+".")] = true
	}
	// 合成 ref 由 planTimeDimensions 产出,天然已在 SELECT 与 GROUP BY 里。
	synthetic := make(map[string]bool, len(timeDimRefs))
	for _, ref := range timeDimRefs {
		selected[ref] = true
		synthetic[ref] = true
	}

	parts := make([]string, 0, len(q.Order))
	for _, o := range q.Order {
		ref := strings.TrimPrefix(o.ID, modelName+".")
		_, isDim := schema.FindDimension(ref)
		_, isMeasure := schema.FindMeasure(ref)
		if !isDim && !isMeasure && !synthetic[ref] {
			return "", &BuildError{Kind: "order", Ref: ref,
				Msg: "order member not found: " + ref}
		}
		// 必须同时出现在本次 SELECT 里:ORDER BY 只能引用输出列,否则
		// GROUP BY 查询会在 DB 层报 "not a grouped column" —— 那是个
		// 调用方看不懂的 500,而这里能给出"把它加进 dimensions/measures"。
		if !selected[ref] {
			return "", &BuildError{Kind: "order", Ref: ref,
				Msg: "order member must also be selected: " + ref +
					" (add it to dimensions or measures)"}
		}
		// 别名会原样进 SQL 文本(quote 不做转义),不校验就是注入面。
		alias := modelName + "." + ref
		if !isSimpleColumn(alias) {
			return "", &BuildError{Kind: "order", Ref: ref,
				Msg: "illegal order member name: " + alias}
		}
		dir, ok := normalizeOrderDir(o.Order)
		if !ok {
			return "", &BuildError{Kind: "order", Ref: ref,
				Msg: fmt.Sprintf("unsupported order direction %q for %s (want asc or desc)", o.Order, ref)}
		}
		parts = append(parts, quote(alias)+" "+dir)
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

// normalizeOrderDir 归一排序方向。cube 契约是 asc / desc;空串按 asc
// (BI 工具常只给 id 不给 order)。其它值一律拒绝 —— 直接透传会把
// "unsupported direction" 变成一段用户可控的 SQL 文本。
func normalizeOrderDir(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "asc":
		return "ASC", true
	case "desc":
		return "DESC", true
	default:
		return "", false
	}
}

// expandSegments 把 q.Segments 编译成 WHERE 片段。
//
// segment 的 SQL 来自 schema 而非请求,所以是可信的静态 SQL;但它的列名同样
// 是 canonical 名 —— BuildTSQL 必须先 rewriteColumns,否则会把 `goods_amount`
// 这种 schema 名发给源库,直接 column not found(这正是 mapping parity 测试
// 当初抓到的 sale_detail.order_status 那个真缺陷的同款)。
func expandSegments(q *Query, schema *cubeschema.Model, expr func(string) string) ([]string, error) {
	var parts []string
	for _, name := range q.Segments {
		sg, ok := schema.FindSegment(name)
		if !ok {
			return nil, &BuildError{Kind: "query",
				Msg: "segment not found: " + name + " (available: " +
					strings.Join(schema.SegmentNames(), ", ") + ")"}
		}
		parts = append(parts, "("+expr(sg.SQL)+")")
	}
	return parts, nil
}

// buildFilter 把 filter 编译成 SQL 片段。列名两侧都 RTRIM,否则 char 补空格
// 会让"看起来一样的条码"匹配不上。
//
// charDate / td 供日期类 operator(inDateRange 等)使用 —— 它们必须和
// timeDimensions 的 dateRange 走**同一套列语义**(见 time.go 的 timeColumn):
// 同一列在两条路径上比较方式不一致,就会出现"按日汇总对了、按日期筛却筛错"。
//
// ⚠️ operator 语义必须与**上线前 ysx 的实现逐字一致** —— 这段代码原本只存在于
// sixun-ysx/main.go,抽到共用包时最容易出的错就是顺手"优化"语义:
//   - contains = **两侧**通配 `%v%`(不是前缀!ysx 另有 startsWith 才是前缀)
//   - 少写一个 operator 会让 supertrade 的查询静默变成 400,而不是给出错答案
func buildFilter(columnSQL string, charDate bool, op string, values []any, td timeDialect) (string, []any, error) {
	col := TrimExpr(columnSQL)
	// 时间比较用的目标表达式,与 time.go 的 timeColumn 同义。
	target := col
	if !charDate {
		target = td.toDate(col)
	}
	one := func(name string) error {
		if len(values) != 1 {
			return fmt.Errorf("%s needs 1 value, got %d", name, len(values))
		}
		return nil
	}
	switch op {
	case "equals":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " = ?", values, nil
	case "notEquals":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " != ?", values, nil
	case "contains":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " LIKE ?", []any{"%" + likeEscape(fmt.Sprint(values[0])) + "%"}, nil
	case "notContains":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " NOT LIKE ?", []any{"%" + likeEscape(fmt.Sprint(values[0])) + "%"}, nil
	case "startsWith":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " LIKE ?", []any{likeEscape(fmt.Sprint(values[0])) + "%"}, nil
	case "in":
		if len(values) == 0 {
			return "", nil, fmt.Errorf("in needs >=1 value")
		}
		return col + " IN (" + placeholders(len(values)) + ")", values, nil
	case "notIn":
		if len(values) == 0 {
			return "", nil, fmt.Errorf("notIn needs >=1 value")
		}
		return col + " NOT IN (" + placeholders(len(values)) + ")", values, nil
	case "gt":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " > ?", values, nil
	case "gte":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " >= ?", values, nil
	case "lt":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " < ?", values, nil
	case "lte":
		if err := one(op); err != nil {
			return "", nil, err
		}
		return col + " <= ?", values, nil
	case "inDateRange", "notInDateRange":
		if len(values) != 2 {
			return "", nil, fmt.Errorf("%s needs 2 values (start, end), got %d", op, len(values))
		}
		start, ok1 := values[0].(string)
		end, ok2 := values[1].(string)
		if !ok1 || !ok2 || !isISODate(start) || !isISODate(end) {
			return "", nil, fmt.Errorf("%s values must be 2 ISO dates (YYYY-MM-DD), got %#v", op, values)
		}
		if start > end {
			return "", nil, fmt.Errorf("%s start %s is after end %s", op, start, end)
		}
		lo := target + " >= ? AND " + target + " <= ?"
		if op == "notInDateRange" {
			lo = "NOT (" + lo + ")"
		}
		return lo, values, nil
	case "onTheDate":
		if err := one(op); err != nil {
			return "", nil, err
		}
		day, err := isoArg(op, values[0])
		if err != nil {
			return "", nil, err
		}
		// char 列直接等值即可(ISO 字符串天然按天对齐);真 datetime 列要
		// 用 [当天, 次日) 半开区间,否则会带上当天的时间部分。
		if charDate {
			return target + " = ?", []any{day}, nil
		}
		return target + " >= ? AND " + target + " < " + shiftPlaceholder(td, "day", 1),
			[]any{day, day}, nil
	case "beforeDate":
		if err := one(op); err != nil {
			return "", nil, err
		}
		day, err := isoArg(op, values[0])
		if err != nil {
			return "", nil, err
		}
		// cube 的 beforeDate 是**排他**的:不含当天。
		return target + " < ?", []any{day}, nil
	case "afterDate":
		if err := one(op); err != nil {
			return "", nil, err
		}
		day, err := isoArg(op, values[0])
		if err != nil {
			return "", nil, err
		}
		return target + " > ?", []any{day}, nil
	default:
		return "", nil, fmt.Errorf("unsupported operator: %s", op)
	}
}

// shiftPlaceholder 返回"占位符 +/平移 n unit"的 SQL 片段。
//
// 注意它作用在**参数占位符**上,不是列表达式 —— 之前写成对列做 shift,
// 会拼出 `DATEADD(day, 1, (col ?))` 这种把占位符当字符串接起来的废 SQL。
// 用两个占位符而不是内联字面量,值才始终走参数绑定。
func shiftPlaceholder(td timeDialect, unit string, n int) string {
	return td.shift("?", unit, n)
}

// isoArg 校验单个日期值。日期错成 "2026/07/01" 时 SQL 仍能跑但恒不匹配,
// 表现为"那段时间没有业务" —— 所以在这里挡住并说清楚。
func isoArg(op string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || !isISODate(s) {
		return "", fmt.Errorf("%s value must be an ISO date (YYYY-MM-DD), got %#v", op, v)
	}
	return s, nil
}

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

// likeEscape 转义 LIKE 通配符。
//
// 上线前 ysx 未做转义(直接 "%"+v+"%");这里补上是**有意的安全加固** ——
// 否则用户输入里的 % 会匹配任意长度的任意内容,_ 匹配任意单字符。
// 正常条码 / 品名不含这些字符,所以对既有查询结果无影响。
func likeEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%', '_', '[', ']', '\\':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// TrimExpr 是简单列名时包一层 RTRIM,否则原样返回。
//
// 只认字母/数字/下划线/点 —— 出现括号、运算符、引号等一律视为表达式,
// 不加 RTRIM(避免破坏 AVG(...) 这类聚合)。
func TrimExpr(expr string) string {
	if isSimpleColumn(expr) {
		return "RTRIM(" + expr + ")"
	}
	return expr
}

func isSimpleColumn(expr string) bool {
	if expr == "" {
		return false
	}
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}
