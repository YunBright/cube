// join.go 多表 join 的解析与 SQL 拼装。
//
// # 难点不在 JOIN 语法,在列名解析
//
// BuildTSQL 现有的 rewriteColumns 是"整段 SQL 里的标识符盲替换":单表时安全
// (所有标识符都来自同一张表),一旦 JOIN,SQL 里同时出现多张表的标识符,
// 盲替换就分不清哪个属于哪张表。实测过一次 segment 子查询
// `item_id IN (SELECT item_id FROM t_bd_item_info)` 两个 item_id 都被替换 ——
// 这次碰巧改对了,但它本质是错的。
//
// 所以这里改成**按 model 分派**:一个成员的 SQL 只在**它自己那个 model 的上下文**
// 里解析(这正是 cube 的语义),解析完再打上该表的别名限定。同名列与
// 不同表的同名列因此天然分得开。
//
// # 一个刻意的限制:join 目前只支持 live model
//
// duck model 的表里存的是 canonical 列名(见 Build 里"不做列名重写"),
// 而 live 走 T-SQL 要经 mapper 换成物理列名。两套列名空间的限定方式不同,
// 一次做对两套的风险大于收益。审批链路要用到的表
// (settlement / settlement_line / purchase_sheet / sale_day / stock)
// **全部是 live**,所以这个限制不挡任何实际用例。
// 遇到跨 storage 或 duck 的 join 时,这里给明确报错而不是生成半对的 SQL。
//
// # 扇出(fan-out)是这里唯一真正危险的东西
//
// settlement(1) ⋈ settlement_line(N):join 进来 N 行后,settlement 的度量
// 被复制 N 份,SUM 出来就是 N 倍。SQL 完全合法、查询 200 OK、金额凭空翻倍 ——
// 与 build.go 顶部记录的 hbposv7 丢 filters 同一类事故(返回错误的数据)。
//
// 规则:one_to_many 会放大"一"那一侧的度量。被放大的 model 的度量 → 编译期
// 报错并指出该用哪一侧。链子深度不设限,但数字不能悄悄错。
package cubequery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// joinEdge 是链上的一条关联。
type joinEdge struct {
	From, To     string // 相邻的两个 model
	Relationship string // 从 From 看过去的方向
	On           string // schema 里写的 ON 条件
}

// joinPlan 是一次查询实际用到的 join 集合。
type joinPlan struct {
	Edges []joinEdge
	// Amplified 是被 one_to_many 放大的 model —— 它们的度量不能用于本查询。
	Amplified map[string]bool
	// Aliases model 名 → SQL 别名。
	Aliases map[string]string
}

// Edge 从 From 出发到达 To 的边。
func (p *joinPlan) edge(from, to string) (joinEdge, bool) {
	for _, e := range p.Edges {
		if e.From == from && e.To == to {
			return e, true
		}
	}
	return joinEdge{}, false
}

func (p *joinPlan) has(target string) bool {
	for _, e := range p.Edges {
		if e.To == target {
			return true
		}
	}
	return false
}

// planJoins 算出查询需要的 join 集合。
//
// referenced 是查询里出现过的 model 名集合(含 base 自己)。
//
// 只走 schema **显式声明**的 joins。不做"猜同名表/猜外键"的自动关联 ——
// 那才是 JIT,也正是会拖垮思迅生产库的那种查询。
func planJoins(base *cubeschema.Model, referenced map[string]bool, reg ModelRegistry) (*joinPlan, error) {
	plan := &joinPlan{Amplified: map[string]bool{}, Aliases: map[string]string{}}

	need := map[string]bool{}
	for m := range referenced {
		if m != base.Name {
			need[m] = true
		}
	}
	if len(need) == 0 {
		plan.Aliases[base.Name] = "t0"
		return plan, nil
	}

	entry, ok := reg.Get(base.Name)
	if !ok || entry.Schema == nil {
		return nil, &BuildError{Kind: "model", Msg: "base model not in registry: " + base.Name}
	}

	// BFS 找从 base 到每个 need 的最短路径,记录父边。
	// 用最短路径而不是"全部可达边":多连一张没人用的表就是白白拖慢源库查询。
	parentOf := map[string]joinEdge{}
	visited := map[string]bool{base.Name: true}
	queue := []string{base.Name}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		curEntry, ok := reg.Get(cur)
		if !ok || curEntry.Schema == nil {
			return nil, &BuildError{Kind: "model", Msg: "join target model not found: " + cur}
		}
		for _, j := range curEntry.Schema.Joins {
			if visited[j.Name] {
				continue
			}
			// 声明了一条 join 但目标 model 在 registry 里不存在(典型:live model
			// 声明了指向 duck model 的关联,如 sale_day → product/supplier)。
			// 这**不是错误** —— 只要本次查询没引用它,那条关联就该被安静地跳过。
			// 早先我在这里直接报错,结果是:查 sale_day 的任何查询都因为
			// "join target model not found: supplier" 而失败。
			if _, avail := reg.Get(j.Name); !avail {
				if need[j.Name] {
					return nil, &BuildError{Kind: "model", Ref: j.Name,
						Msg: fmt.Sprintf("query references model %q but it is not available"+
							"(duck model 或未加载);joins 目前只支持 storage: live", j.Name)}
				}
				continue
			}
			visited[j.Name] = true
			parentOf[j.Name] = joinEdge{From: cur, To: j.Name, Relationship: j.Relationship, On: j.On}
			queue = append(queue, j.Name)
		}
	}

	// 沿父边回溯,收集路径上的边。
	keep := map[string]joinEdge{}
	for m := range need {
		if !visited[m] {
			return nil, &BuildError{Kind: "model", Ref: m,
				Msg: fmt.Sprintf("query references model %q but no join chain from %q reaches it"+
					"(declare it under joins: in %s/schema.yaml)", m, base.Name, base.Name)}
		}
		for cur := m; cur != base.Name; {
			e, ok := parentOf[cur]
			if !ok {
				return nil, &BuildError{Kind: "model", Ref: m,
					Msg: "join chain from " + base.Name + " to " + m + " is broken"}
			}
			keep[e.From+"->"+e.To] = e
			cur = e.From
		}
	}

	// 别名按 (From, To) 排序后依次分配 —— 序号而非 model 名:
	//   零注入风险(别名不含任何 schema 字符),且两个 model 映射到同一张物理表时
	// 不会撞车(用 model 名做别名就会)。
	keys := make([]string, 0, len(keep))
	for k := range keep {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	plan.Aliases[base.Name] = "t0"
	for i, k := range keys {
		e := keep[k]
		plan.Edges = append(plan.Edges, e)
		plan.Aliases[e.To] = fmt.Sprintf("t%d", i+1)
		if e.Relationship == "one_to_many" {
			// 被放大的正是"一"那一侧(From)。
			//
			// ⚠️ **不做链式传播**:one_to_many 只复制 From 的行,To(多的一侧)
			// 以及 To 之后连的表都没有被复制。我一开始在这里传播了一轮,
			// 结果把"多的一侧"的度量也标成被放大 —— 而那恰恰是最该放行的
			// 正确用法,等于把唯一能用的形式也拦了。
			plan.Amplified[e.From] = true
		}
	}
	return plan, nil
}

// checkFanOut 扇出的静态判定。
func checkFanOut(plan *joinPlan, measureModels []string) error {
	if len(plan.Amplified) == 0 {
		return nil
	}
	for _, mm := range measureModels {
		if plan.Amplified[mm] {
			amplified := make([]string, 0, len(plan.Amplified))
			for a := range plan.Amplified {
				amplified = append(amplified, a)
			}
			sort.Strings(amplified)
			return &BuildError{Kind: "query", Ref: mm,
				Msg: fmt.Sprintf("measure from %q would be multiplied by a one_to_many join"+
					" in this query (SUM over duplicated rows silently inflates the total;"+
					" amplified models: %s) — use the measure on the many side, or split"+
					" into two queries", mm, strings.Join(amplified, ", "))}
		}
	}
	return nil
}

// checkStorage 拒绝跨 storage / duck 的 join(见文件头说明)。
func checkStorage(base *cubeschema.Model, plan *joinPlan, reg ModelRegistry) error {
	if len(plan.Edges) == 0 {
		return nil
	}
	if base.EffectiveStorage() != cubeschema.StorageLive {
		return &BuildError{Kind: "model",
			Msg: fmt.Sprintf("joins are only supported on storage: live models"+
				" (got %q with storage=%s)", base.Name, base.EffectiveStorage())}
	}
	for _, e := range plan.Edges {
		entry, ok := reg.Get(e.To)
		if !ok || entry.Schema == nil {
			continue
		}
		if entry.Schema.EffectiveStorage() != cubeschema.StorageLive {
			return &BuildError{Kind: "model", Ref: e.To,
				Msg: fmt.Sprintf("cannot join %q into %q: it is storage: %s, joins are live-only"+
					"(mixing storage would query two different engines in one statement)",
					e.To, base.Name, entry.Schema.EffectiveStorage())}
		}
	}
	return nil
}

// rewriteFor 把成员 SQL 里的 canonical 列名替换成 `别名.物理列名`。
//
// 与 rewriteColumns 的区别:后者是**全局盲替换**,前者只在**该成员自己那个
// model 的上下文**里替换,并补上别名限定 —— 这才是 JOIN 场景下正确的做法。
//
// ⚠️ 即使源列名与 canonical 名相同(`amount → amount`),**也必须打上别名**:
// 不打就会落成裸列名,JOIN 多表时 SQL Server 无法判定它属于哪张表。
func rewriteFor(sql, alias string, mapper ColumnResolver) string {
	if sql == "" {
		return sql
	}
	protected, literals := protectLiterals(sql)
	protected = identRE.ReplaceAllStringFunc(protected, func(ident string) string {
		if mapper == nil {
			return ident
		}
		src, ok := mapper.Resolve(ident)
		if !ok {
			return ident
		}
		return alias + "." + src
	})
	return restoreLiterals(protected, literals)
}

// renderOn 渲染 ON 条件。schema 里写 `{CUBE}.col = other.col`。
//
// 两侧都必须是**声明过的 dimension** —— 不能是任意标识符。
// 这条限制换来的是:ON 条件里的每个列名都过了 schema 校验,
// 不存在"表达式里塞一段用户文本"的口子。
//
// 单趟扫描,不做事后字符串替换:先前一版先把 `{CUBE}` 换成别名,再回头扫
// `<model>.<member>`,结果把**刚生成的** `t0.id` 当成 model 名去找,
// 报 "join condition references unknown model t0"。替换与扫描必须同时进行。
func renderOn(e joinEdge, plan *joinPlan, resolve func(model, member string) (string, error)) (string, error) {
	s := strings.TrimSpace(e.On)
	fromAlias, ok := plan.Aliases[e.From]
	if !ok {
		return "", &BuildError{Kind: "model", Msg: "no alias for " + e.From}
	}
	const cubeToken = "{CUBE}."
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], cubeToken) {
			// {CUBE} 后面跟的是**裸成员名**(没有 model. 前缀),
			// 所以这里必须扫单个标识符,不能复用 scanQualified 的 ident.ident 形式。
			if member, n := scanIdent(s[i+len(cubeToken):]); n > 0 {
				col, err := resolve(e.From, member)
				if err != nil {
					return "", err
				}
				b.WriteString(fromAlias + "." + col)
				i += len(cubeToken) + n
				continue
			}
			return "", &BuildError{Kind: "model",
				Msg: "join condition for " + e.From + "->" + e.To + ": {CUBE} must be followed by a column name"}
		}
		if isIdentStart(s[i]) {
			if model, member, n := scanQualified(s[i:]); n > 0 {
				col, err := resolve(model, member)
				if err != nil {
					return "", err
				}
				alias, ok := plan.Aliases[model]
				if !ok {
					return "", &BuildError{Kind: "model", Ref: model,
						Msg: "join condition references model " + model + " which is not in the join chain"}
				}
				b.WriteString(alias + "." + col)
				i += n
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return " ON " + b.String(), nil
}

// scanIdent 解析一个裸标识符,返回 (名字, 消耗的字节数)。
func scanIdent(s string) (name string, n int) {
	i := 0
	for i < len(s) && isIdentChar(s[i]) {
		i++
	}
	if i == 0 {
		return "", 0
	}
	return s[:i], i
}

// scanQualified 解析 `ident.ident`,返回 (model, member, 消耗的字节数)。
// 不是合法限定名时 n == 0。
func scanQualified(s string) (model, member string, n int) {
	i := 0
	for i < len(s) && isIdentChar(s[i]) {
		i++
	}
	if i == 0 || i >= len(s) || s[i] != '.' {
		return "", "", 0
	}
	j := i + 1
	for j < len(s) && isIdentChar(s[j]) {
		j++
	}
	if j == i+1 {
		return "", "", 0
	}
	return s[:i], s[i+1 : j], j
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// joinClauseKind 决定这条边用 LEFT JOIN 还是 INNER JOIN。
//
// many_to_one 是"查档案":左表每行都应该有一份档案,没有就是数据问题,
// 用 INNER 会让这些行凭空消失(又一次"静默少数据")。所以默认 LEFT。
func joinClauseKind(rel string) string {
	if rel == "one_to_many" || rel == "one_to_one" {
		return "INNER JOIN"
	}
	return "LEFT JOIN"
}
