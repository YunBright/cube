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
type BuildError struct {
	Kind string // "dimension" / "measure" / "filter" / "filter_operator"
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
		expr, args, err := buildFilter(d.SQL, f.Operator, f.Values)
		if err != nil {
			return nil, &BuildError{Kind: "filter", Ref: ref,
				Msg: "filter: " + err.Error()}
		}
		whereParts = append(whereParts, expr)
		res.Args = append(res.Args, args...)
	}

	sql := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectList, ", "), schema.SQLTable)
	if len(whereParts) > 0 {
		sql += " WHERE " + strings.Join(whereParts, " AND ")
	}
	if len(groupBy) > 0 {
		sql += " GROUP BY " + strings.Join(groupBy, ", ")
	}
	limit := DefaultLimit
	if q.Limit != nil && *q.Limit > 0 {
		limit = *q.Limit
	}
	sql += fmt.Sprintf(" LIMIT %d", limit)
	res.SQL = sql
	return res, nil
}

// buildFilter 把 filter 编译成 SQL 片段。列名两侧都 RTRIM,否则 char 补空格
// 会让"看起来一样的条码"匹配不上。
//
// ⚠️ operator 语义必须与**上线前 ysx 的实现逐字一致** —— 这段代码原本只存在于
// sixun-ysx/main.go,抽到共用包时最容易出的错就是顺手"优化"语义:
//   - contains = **两侧**通配 `%v%`(不是前缀!ysx 另有 startsWith 才是前缀)
//   - 少写一个 operator 会让 supertrade 的查询静默变成 400,而不是给出错答案
func buildFilter(columnSQL, op string, values []any) (string, []any, error) {
	col := TrimExpr(columnSQL)
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
	default:
		return "", nil, fmt.Errorf("unsupported operator: %s", op)
	}
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
