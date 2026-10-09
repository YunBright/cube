// tsql.go 把 cube Query 编译成 **T-SQL**,用于 storage: live 的 model
// 直接查思迅源库 —— 不落 DuckDB。
//
// 为什么不复用 Build 的输出再字符串替换:
// 那会踩两个坑。一是要把 DuckDB 的 `LIMIT n` 挪到 `SELECT TOP n`,
// 二是 schema 里的列名是 family 无关的 canonical 名(goods_amount),
// 而源库里是 family 私有列名(hbposv7 = Goods_amt / ysx = sheet_amt)。
// 所以这里从一开始就按"列名要解析、方言是 T-SQL"来编译。
//
// 与 Build 共享的部分(刻意的,避免两套语义分叉):
//   - dimension / measure / filter 的查找与 4xx 子码语义
//   - buildFilter 的 operator 语义与占位符参数绑定
//   - 简单列名统一包 RTRIM(思迅 char 定长列普遍补空格)
//   - buildOrder 的 ORDER BY 编译(只差别名引号)
//
// 剩下的唯一限制(与 Build 相同,不是这里引入的):不支持 joins ——
// Build 也不支持,measure 只能引用本表列。
package cubequery

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// ColumnResolver 把 schema 的 canonical 列名解析为源库列名。
//
// 实现方是 *fieldmapping.Mapper(mapping.yaml 的反向索引)。
// 解析不到的标识符(SQL 关键字、字面量)原样保留。
type ColumnResolver interface {
	Resolve(target string) (string, bool)
}

var (
	stringLitRE = regexp.MustCompile(`'[^']*'`)
	identRE     = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
)

// BuildTSQL 编译 Query → T-SQL,打到 sourceTable 指定的源库表。
//
// resolver 负责 canonical 列名 → 源库列名的翻译;为 nil 时列名原样使用
// (仅适用于 schema 直接写源列名的场景)。
//
// 这是**单表**入口(等价于"registry 里只有它自己"),保留是为了让既有调用方
// 与测试一字不变。需要跨 model 的查询请用 BuildTSQLJoin。
func BuildTSQL(q *Query, schema *cubeschema.Model, sourceTable string, resolver ColumnResolver) (*Result, error) {
	return BuildTSQLJoin(q, schema, sourceTable, resolver, baseEntry(schema, sourceTable, resolver))
}

// BuildTSQLJoin 编译可能跨 model 的 Query → T-SQL。
//
// reg 必须包含 base model 自身。查询里出现别的 model 的成员时,
// 按 base schema 的 joins 声明把那些表连进来(见 join.go)。
func BuildTSQLJoin(q *Query, schema *cubeschema.Model, sourceTable string, resolver ColumnResolver, reg ModelRegistry) (*Result, error) {
	if schema == nil {
		return nil, &BuildError{Kind: "model", Msg: "schema is nil"}
	}
	if schema.EffectiveStorage() != cubeschema.StorageLive {
		return nil, &BuildError{Kind: "model", Msg: fmt.Sprintf("model %q is not a live model (storage=%s)", schema.Name, schema.EffectiveStorage())}
	}
	if strings.TrimSpace(sourceTable) == "" {
		return nil, &BuildError{Kind: "model", Msg: "live model requires a source table name (config table_*)"}
	}
	// base model 取**被编译的 schema**,不是从查询里再推一次。
	//
	// 早先用 q.Model()(measures 第一个的前缀)当 base:当查询里第一个 measure
	// 来自被 join 的 model 时,它算出来的 base 与实际传入的 schema 不是同一个,
	// 于是 FROM 的别名和 ON 里引用的别名对不上 ——
	// 实测生成过 `FROM t_fm_recpay_gx_master AS t1 INNER JOIN settlement_line AS t1`,
	// 一张表两个别名。调用方是按 schema 路由过来的,schema 才是权威。
	modelName := schema.Name
	if modelName == "" {
		return nil, &BuildError{Kind: "model", Msg: "cannot infer model"}
	}
	if len(q.Measures) == 0 {
		return nil, &BuildError{Kind: "measure", Msg: "at least one measure required"}
	}
	if reg == nil {
		reg = baseEntry(schema, sourceTable, resolver)
	}
	baseModel, _ := reg.Get(modelName)

	res := &Result{DimRefs: []string{}, MeasureRefs: []string{}}

	// ---- 第一步:收集查询引用了哪些 model,算出 join 集合 ----
	referenced := map[string]bool{modelName: true}
	collect := func(name string) {
		if i := strings.Index(name, "."); i > 0 {
			referenced[name[:i]] = true
		}
	}
	for _, d := range q.Dimensions {
		collect(d)
	}
	for _, m := range q.Measures {
		collect(m)
	}
	for _, f := range q.Filters {
		collect(f.Member)
	}
	for _, td := range q.TimeDimensions {
		collect(td.Dimension)
	}

	jp, err := planJoins(schema, referenced, reg)
	if err != nil {
		return nil, err
	}
	if err := checkStorage(schema, jp, reg); err != nil {
		return nil, err
	}

	// 成员的归属解析:<model>.<ref> → (model, ref, 该 model 的 entry, 别名)
	owner := func(qualified string) (string, string, ModelEntry, string, error) {
		mn, ref := modelName, qualified
		if i := strings.Index(qualified, "."); i > 0 {
			mn, ref = qualified[:i], qualified[i+1:]
		}
		e, ok := reg.Get(mn)
		if !ok || e.Schema == nil {
			return "", "", ModelEntry{}, "", &BuildError{Kind: "model", Ref: mn,
				Msg: "model not available: " + mn}
		}
		alias, ok := jp.Aliases[mn]
		if !ok {
			return "", "", ModelEntry{}, "", &BuildError{Kind: "model", Ref: mn,
				Msg: "model " + mn + " is not in the join chain"}
		}
		return mn, ref, e, alias, nil
	}
	// 改写一个成员的 SQL:在**它自己那个 model** 的上下文里解析 + 打别名限定。
	rewriteMember := func(mn string, e ModelEntry, alias string, sql string) string {
		if len(jp.Edges) == 0 {
			// 单表:保持既有行为(不加别名),避免把现有 SQL 全改一遍。
			return rewriteColumns(sql, resolver)
		}
		return rewriteFor(sql, alias, e.Mapper)
	}

	// ---- 第二步:成员表达式 ----
	var selectList []string
	var groupBy []string
	// orderExprs 记 成员键 → 底层表达式,供 SQL 2008 的 ROW_NUMBER 用
	// (窗口函数的 ORDER BY 里看不到 SELECT 别名,只能用表达式)。
	orderExprs := map[string]string{}
	measureModels := []string{}
	for _, raw := range q.Dimensions {
		mn, ref, e, alias, err := owner(raw)
		if err != nil {
			return nil, err
		}
		d, ok := e.Schema.FindDimension(ref)
		if !ok {
			return nil, &BuildError{Kind: "dimension", Ref: ref,
				Msg: "dimension not found: " + ref + " (on model " + mn + ")"}
		}
		expr := rewriteMember(mn, e, alias, d.SQL)
		// RTRIM 与 Build 一致:SELECT 用 RTRIM(expr),GROUP BY 用裸 expr
		// (T-SQL 同样允许 GROUP BY 原列配 SELECT RTRIM(原列))。
		selectList = append(selectList, fmt.Sprintf("%s AS [%s.%s]", TrimExpr(expr), mn, ref))
		groupBy = append(groupBy, expr)
		orderExprs[mn+"."+ref] = expr
		res.DimRefs = append(res.DimRefs, ref)
	}

	for _, raw := range q.Measures {
		mn, ref, e, alias, err := owner(raw)
		if err != nil {
			return nil, err
		}
		ms, ok := e.Schema.FindMeasure(ref)
		if !ok {
			return nil, &BuildError{Kind: "measure", Ref: ref,
				Msg: "measure not found: " + ref + " (on model " + mn + ")"}
		}
		msSQL := rewriteMember(mn, e, alias, ms.SQL)
		selectList = append(selectList, fmt.Sprintf("%s AS [%s.%s]", msSQL, mn, ref))
		orderExprs[mn+"."+ref] = msSQL
		res.MeasureRefs = append(res.MeasureRefs, ref)
		measureModels = append(measureModels, mn)
	}
	if err := checkFanOut(jp, measureModels); err != nil {
		return nil, err
	}

	// ---- 第三步:过滤 / segment / 时间维度 ----
	var whereParts []string
	for _, f := range q.Filters {
		mn, ref, e, alias, err := owner(f.Member)
		if err != nil {
			return nil, err
		}
		d, ok := e.Schema.FindDimension(ref)
		if !ok {
			return nil, &BuildError{Kind: "filter", Ref: ref,
				Msg: "filter dimension not found: " + ref + " (on model " + mn + ")"}
		}
		expr, args, err := buildFilter(rewriteMember(mn, e, alias, d.SQL), d.CharDate, f.Operator, f.Values, tsqlTime)
		if err != nil {
			return nil, &BuildError{Kind: "filter", Ref: ref,
				Msg: "filter: " + err.Error()}
		}
		whereParts = append(whereParts, expr)
		res.Args = append(res.Args, args...)
	}

	segParts, err := expandSegments(q, schema, func(s string) string { return rewriteColumns(s, resolver) })
	if err != nil {
		return nil, err
	}
	whereParts = append(whereParts, segParts...)

	// 方言按**目标实例**选,不是按 family 猜 —— hbposv7 实测是 SQL Server 2008,
	// DATEFROMPARTS / OFFSET-FETCH 在那里都是语法错。
	dialect := tsqlTime
	if baseModel.LegacyTSQL {
		dialect = tsqlLegacyTime
	}

	// timeDimensions 在 filters 之后处理,whereParts 与 res.Args 的追加顺序
	// 必须严格一致 —— 参数按 @pN 下标绑定,错位会让过滤值串到别的列上。
	timePlan, err := planTimeDimensions(q, schema, modelName,
		func(d *cubeschema.Dimension) string { return rewriteColumns(d.SQL, resolver) },
		quoteBracket, dialect)
	if err != nil {
		return nil, err
	}
	selectList = append(selectList, timePlan.selectList...)
	groupBy = append(groupBy, timePlan.groupBy...)
	whereParts = append(whereParts, timePlan.whereParts...)
	res.Args = append(res.Args, timePlan.args...)
	res.DimRefs = append(res.DimRefs, timePlan.dimRefs...)

	// total 用 window function 而不是第二次查询:live model 直接打思迅生产库,
	// 少一次往返就是少一次压力。COUNT(*) OVER() 在 GROUP BY 之后、
	// ORDER BY/LIMIT 之前求值,语义正是"去掉 limit/offset 后的行数"。
	if q.Total {
		selectList = append(selectList, "COUNT(*) OVER() AS "+quoteBracket(TotalField))
	}

	limit := DefaultLimit
	if q.Limit != nil && *q.Limit > 0 {
		limit = *q.Limit
	}
	offset := q.Offset.First()
	orderBy, err := buildOrder(q, schema, modelName, quoteBracket, timePlan.dimRefs)
	if err != nil {
		return nil, err
	}
	// orderKeys 是 order 的表达式形态(给 ROW_NUMBER 用)。
	orderKeys := orderExprKeys(q, modelName, orderExprs)
	// T-SQL 的两条分页路径**互斥**,不能混用:
	//   - offset == 0 → SELECT TOP n ... ORDER BY ...(TOP 取的是排序后的前 n 行)
	//   - offset  > 0 → 去掉 TOP,改用 ORDER BY ... OFFSET m ROWS FETCH NEXT n ROWS ONLY
	// 同时写 TOP 和 FETCH NEXT 会被 SQL Server 判为语法错。
	baseAlias := jp.Aliases[modelName]
	from := sourceTable
	if len(jp.Edges) > 0 {
		from = sourceTable + " AS " + baseAlias
	}
	var sql string
	// 分页:三条路径,按实例能力与 offset 取舍
	//   offset == 0                 → SELECT TOP n（所有版本都支持）
	//   offset > 0 且 2012+         → ORDER BY ... OFFSET m ROWS FETCH NEXT n ROWS ONLY
	//   offset > 0 且 SQL Server 2008 → ROW_NUMBER() 外层过滤
	//
	// 第三条是实测逼出来的:hbposv7(compat 80)对 OFFSET/FETCH 直接报
	// "'OFFSET' 附近有语法错误"。发一段目标实例必然语法错的 SQL 过去,
	// 表现出来就是"分页功能坏了",而真实原因在数据库版本上。
	if offset > 0 && baseModel.LegacyTSQL {
		sql, err = legacyPagedSelect(selectList, orderKeys, from, whereParts, groupBy,
			jp, reg, offset, limit)
		if err != nil {
			return nil, err
		}
	} else {
		if offset > 0 {
			sql = fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectList, ", "), from)
		} else {
			sql = fmt.Sprintf("SELECT TOP %d %s FROM %s", limit, strings.Join(selectList, ", "), from)
		}
		if len(jp.Edges) > 0 {
			joins, err := joinClauses(jp, reg)
			if err != nil {
				return nil, err
			}
			sql += joins
		}
		if len(whereParts) > 0 {
			sql += " WHERE " + strings.Join(whereParts, " AND ")
		}
		if len(groupBy) > 0 {
			sql += " GROUP BY " + strings.Join(groupBy, ", ")
		}
		sql += orderBy
		if offset > 0 {
			sql += fmt.Sprintf(" OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", offset, limit)
		}
	}
	res.SQL = numberPlaceholders(sql)
	return res, nil
}

// legacyPagedSelect 生成 SQL Server 2008 的分页形态:
//
//	SELECT <投影> FROM (SELECT <投影>, ROW_NUMBER() OVER (ORDER BY ...) AS __cube_rn
//	                   FROM ... WHERE ... GROUP BY ...) AS __cube_page
//	WHERE __cube_rn > m AND __cube_rn <= m+n
//
// ROW_NUMBER 的 ORDER BY 里**不能引用 SELECT 别名**(别名在窗口函数里不可见),
// 必须用底层表达式 —— 所以这里收的是 orderKeys(成员 → 表达式),不是别名。
func legacyPagedSelect(selectList, orderKeys []string, from string, whereParts, groupBy []string,
	jp *joinPlan, reg ModelRegistry, offset, limit int) (string, error) {

	if len(orderKeys) == 0 {
		return "", &BuildError{Kind: "query",
			Msg: "offset > 0 on a SQL Server 2008 instance requires an explicit order"}
	}
	inner := "SELECT " + strings.Join(selectList, ", ") +
		", ROW_NUMBER() OVER (ORDER BY " + strings.Join(orderKeys, ", ") + ") AS " + rowNumberAlias
	inner += " FROM " + from
	if len(jp.Edges) > 0 {
		joins, err := joinClauses(jp, reg)
		if err != nil {
			return "", err
		}
		inner += joins
	}
	if len(whereParts) > 0 {
		inner += " WHERE " + strings.Join(whereParts, " AND ")
	}
	if len(groupBy) > 0 {
		inner += " GROUP BY " + strings.Join(groupBy, ", ")
	}
	// 外层只投影业务列,不带 __cube_rn(它是分页的实现细节,不该漏给调用方)。
	outer := "SELECT " + strings.Join(outerColumns(selectList), ", ") +
		" FROM (" + inner + ") AS __cube_page" +
		fmt.Sprintf(" WHERE %s > %d AND %s <= %d", rowNumberAlias, offset, rowNumberAlias, offset+limit)
	return outer, nil
}

// rowNumberAlias 是分页内部列名。带前缀避免与业务列撞名。
const rowNumberAlias = "__cube_rn"

// outerColumns 从 SELECT 列表里取出所有 `... AS [alias]`,按原顺序。
//
// 用的是"最后一个 AS [ 之后的 ]"来定位 —— 我们的成员 SQL 里不会出现
// 方括号字面量(方言是 T-SQL,但成员 SQL 是我们自己在 schema 里写的)。
func outerColumns(selectList []string) []string {
	out := make([]string, 0, len(selectList))
	for _, item := range selectList {
		i := strings.LastIndex(item, " AS [")
		if i < 0 || !strings.HasSuffix(item, "]") {
			// 没有别名的(理论上不该有),原样带上,宁可多列也不丢数据。
			out = append(out, item)
			continue
		}
		// ⚠️ 必须**连方括号一起**保留:内层别名是 `[settlement.id]` 这种
		// "带点的单列名",外层写成裸的 `settlement.id` 会被 SQL Server
		// 当成"表.列"的两段式标识符,报"无法绑定由多个部分组成的标识符"
		// (实测报 4104)。
		out = append(out, item[i+len(" AS "):])
	}
	return out
}

// orderExprKeys 把 q.Order 翻译成"成员底层表达式 + 方向"。
//
// 与 buildOrder 的差别:buildOrder 产出的是给最终 SQL 的 ORDER BY(用别名),
// 这里产出的是给 ROW_NUMBER() OVER(ORDER BY ...) 的 —— **窗口函数里看不到
// SELECT 别名**,必须用底层表达式。
func orderExprKeys(q *Query, modelName string, exprs map[string]string) []string {
	var out []string
	for _, o := range q.Order {
		ref := strings.TrimPrefix(o.ID, modelName+".")
		expr, ok := exprs[o.ID]
		if !ok {
			expr, ok = exprs[modelName+"."+ref]
		}
		if !ok {
			// 排序成员没被 select(时间粒度合成的成员等)—— 交给 buildOrder 报错,
			// 这里跳过即可。
			continue
		}
		dir, ok := normalizeOrderDir(o.Order)
		if !ok {
			continue
		}
		out = append(out, expr+" "+dir)
	}
	return out
}

// joinClauses 渲染 JOIN 子句。
//
// ON 条件里的每个列名都过一遍"必须是该 model 声明过的 dimension"校验 ——
// 这条限制换来的是 ON 条件不含任何未经 schema 校验的文本。
//
// 渲染失败**一律返回错误**,绝不退化成 `ON 1=0` 之类的占位条件:
// 那会让查询合法执行、返回空集,表现为"这个关联查不到数据",
// 而真实原因是 schema 写错了 —— 又一次静默把配置错误伪装成业务事实。
func joinClauses(jp *joinPlan, reg ModelRegistry) (string, error) {
	var b strings.Builder
	resolve := func(model, member string) (string, error) {
		e, ok := reg.Get(model)
		if !ok || e.Schema == nil {
			return "", &BuildError{Kind: "model", Ref: model,
				Msg: "join condition references unknown model " + model}
		}
		d, ok := e.Schema.FindDimension(member)
		if !ok {
			return "", &BuildError{Kind: "model", Ref: member,
				Msg: fmt.Sprintf("join condition column %q must be a declared dimension of %q"+
					"(raw expressions in `on:` are not allowed)", member, model)}
		}
		src := d.SQL
		if e.Mapper != nil {
			if r, ok := e.Mapper.Resolve(src); ok {
				src = r
			}
		}
		return src, nil
	}
	for _, e := range jp.Edges {
		on, err := renderOn(e, jp, resolve)
		if err != nil {
			return "", err
		}
		// 物理表名来自目标 model 的 SourceTable,**不是 model 名** ——
		// `purchase_sheet` 这个 model 打的是 `t_pm_sheet_master`。
		// 早先直接写 e.To,三条 join 用例全挂在"对象名 purchase_sheet 无效"。
		target, ok := reg.Get(e.To)
		if !ok || strings.TrimSpace(target.SourceTable) == "" {
			return "", &BuildError{Kind: "model", Ref: e.To,
				Msg: "join target " + e.To + " has no physical table (config table_*)"}
		}
		b.WriteString(" " + joinClauseKind(e.Relationship) + " " + target.SourceTable +
			" AS " + jp.Aliases[e.To] + on)
	}
	return b.String(), nil
}

// rewriteColumns 把 SQL 表达式里的 canonical 列名替换成源库列名。
//
// 先把字符串字面抠出来保护起来,否则 `CASE WHEN status = 'RO' THEN ...`
// 里的 'RO' 会被当成标识符处理。SQL 关键字不在 targets 里,天然不替换。
func rewriteColumns(sql string, resolver ColumnResolver) string {
	if resolver == nil || sql == "" {
		return sql
	}
	protected, literals := protectLiterals(sql)
	protected = identRE.ReplaceAllStringFunc(protected, func(ident string) string {
		if src, ok := resolver.Resolve(ident); ok && src != ident {
			return src
		}
		return ident
	})
	return restoreLiterals(protected, literals)
}

// numberPlaceholders 把 `?` 依次换成 `@p1 @p2 ...` —— go-mssqldb 用这种命名参数。
// buildFilter 产生的占位符一律走参数绑定,值永远不进 SQL 文本。
func numberPlaceholders(sql string) string {
	protected, literals := protectLiterals(sql)
	parts := strings.Split(protected, "?")
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			fmt.Fprintf(&b, "@p%d", i)
		}
		b.WriteString(p)
	}
	return restoreLiterals(b.String(), literals)
}

// protectLiterals 把单引号字面量换成不可与标识符冲突的占位符。
func protectLiterals(sql string) (string, []string) {
	var literals []string
	out := stringLitRE.ReplaceAllStringFunc(sql, func(lit string) string {
		literals = append(literals, lit)
		return fmt.Sprintf("\x00lit%d\x00", len(literals)-1)
	})
	return out, literals
}

func restoreLiterals(sql string, literals []string) string {
	for i, lit := range literals {
		sql = strings.ReplaceAll(sql, fmt.Sprintf("\x00lit%d\x00", i), lit)
	}
	return sql
}
