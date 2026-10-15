// joinorder_test.go 锁 JOIN 的**输出顺序**,不锁列名解析。
//
// 背景:2026-10-15 接销售报表时踩到的一个真缺陷 ——
//
//	sales_report ──→ item ──→ item_cls ──→ item_cls_parent
//
// 编译出来的 SQL 是:
//
//	FROM t_rm_daysum AS t0 LEFT JOIN t_bd_item_cls AS t1 ON t3.item_clsno = ...
//
// t3 那一刻还没在 FROM 子句里出现过,思迅源库直接回
// "无法绑定由多个部分组成的标识符 t3.item_clsno"。
//
// 根因是别名与边按 From->To **字典序**排,而 T-SQL 的 FROM 是从左到右
// 求值的 —— 字典序恰好把子节点排到了父节点前面:
//
//	"item->item_cls"              <  "item_cls->item_cls_parent"  <  "sales_report->item"
//
// ⚠️ 既有两条边以内的用例**全绿**,因为只有一条边时字典序与拓扑序恰好一致。
//
//	所以这不是"新功能没测到",是**老用例的覆盖面不足**:没人写过三段链。
package cubequery

import (
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// salesReportChain 复刻真实链路的四个 model。
//
// ⚠️ 命名是**故意的**:只有当子 model 名排在父 model 名**之后**(字典序倒挂)
// 时,这个缺陷才会显形。把名字换成 a→b→c→d 这类"好读"的命名,
// 字典序与拓扑序恰好一致,测试就变成了永远绿的装饰。
func salesReportChain() (*cubeschema.Model, map[string]ModelEntry) {
	salesReport := &cubeschema.Model{
		Name: "sales_report", SQLTable: "sales_report", Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "item_no", SQL: "item_no", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
		Joins: []cubeschema.Join{{
			Name: "item", Relationship: "many_to_one",
			On: "{CUBE}.item_no = item.id",
		}},
	}
	item := &cubeschema.Model{
		Name: "item", SQLTable: "item", Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "clsno", SQL: "clsno", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
		Joins: []cubeschema.Join{{
			Name: "item_cls", Relationship: "many_to_one",
			On: "{CUBE}.clsno = item_cls.id",
		}},
	}
	itemCls := &cubeschema.Model{
		Name: "item_cls", SQLTable: "item_cls", Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "parent_no", SQL: "parent_no", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
		Joins: []cubeschema.Join{{
			Name: "item_cls_parent", Relationship: "many_to_one",
			On: "{CUBE}.parent_no = item_cls_parent.id",
		}},
	}
	// 刻意与 item_cls **同一张物理表**:这是"两个 model 映射到同一张表"
	// 这条官方机制(join.go 别名分配处的注释),也是自关联取父类别的唯一走法。
	itemClsParent := &cubeschema.Model{
		Name: "item_cls_parent", SQLTable: "item_cls_parent", Storage: cubeschema.StorageLive,
		Dimensions: []cubeschema.Dimension{
			{Name: "id", SQL: "id", Type: cubeschema.TypeString},
			{Name: "name", SQL: "name", Type: cubeschema.TypeString},
		},
		Measures: []cubeschema.Measure{
			{Name: "count", SQL: "COUNT(*)", Type: cubeschema.TypeNumber},
		},
	}
	entries := map[string]ModelEntry{
		"sales_report":    {Schema: salesReport, SourceTable: "t_rm_daysum"},
		"item":            {Schema: item, SourceTable: "t_bd_item_info"},
		"item_cls":        {Schema: itemCls, SourceTable: "t_bd_item_cls"},
		"item_cls_parent": {Schema: itemClsParent, SourceTable: "t_bd_item_cls"},
	}
	return salesReport, entries
}

// TestJoin_EdgesAreEmittedInTopologicalOrder 是本文件的核心用例。
//
// 判据:**每条 JOIN 的 ON 里出现的每个别名,必须已经在 FROM 子句里定义过。**
//
// 为什么不用"断言 SQL 文本等于某个期望串":那会把别名编号也钉死,
// 而本缺陷的本质是**顺序**,不是具体编号。直接按 FROM 里的出现位置
// 检查 ON 引用的合法性,才能在别名策略变化时继续守住同一个性质。
func TestJoin_EdgesAreEmittedInTopologicalOrder(t *testing.T) {
	base, entries := salesReportChain()
	reg := NewRegistry(entries)
	lim := 10
	q := &Query{
		Measures:   []string{"sales_report.count"},
		Dimensions: []string{"item_cls_parent.id", "item_cls_parent.name"},
		Limit:      &lim,
	}
	res, err := BuildTSQLJoin(q, base, "t_rm_daysum", nil, reg)
	if err != nil {
		t.Fatalf("BuildTSQLJoin: %v", err)
	}
	from := strings.Index(res.SQL, "FROM ")
	if from < 0 {
		t.Fatalf("SQL 里没有 FROM: %s", res.SQL)
	}
	tail := res.SQL[from:]

	// 已出现的别名 → 它在 FROM 子句里的定义位置。
	defined := map[string]int{}
	// 每读到一次 "<tbl> AS <alias>",记下位置。
	// 物理表名是唯一标识(item_cls / item_cls_parent 共用 t_bd_item_cls),
	// 所以用表名而不是 model 名做键 —— 这也顺带验证了两个别名不撞车。
	for _, m := range regexpAsAlias(tail) {
		defined[m.alias] = m.pos
	}
	if len(defined) != 4 {
		t.Fatalf("期望 4 个别名(t0..t3),实际 %d 个: %v", len(defined), defined)
	}

	// 逐条 JOIN 子句检查:ON 里引用的每个别名,必须已经出现在这条子句的
	// " ON " **之前**(含本条子句自己的 "<表> AS tN")。
	//
	// ⚠️ 判据不能写成"在 JOIN 关键字之前":目标表的别名本来就定义在
	//   `LEFT JOIN t_bd_item_info AS t1 ON ...` 里、位于 JOIN 关键字**之后**。
	//   那样写这条断言会对正确 SQL 也报错 —— 判据本身错了会让人去改正确代码。
	for _, jc := range joinClausesInOrder(tail) {
		for _, used := range aliasesIn(jc.on) {
			pos, ok := defined[used]
			if !ok {
				t.Errorf("JOIN 子句的 ON 引用了从未定义的别名 %q:%s", used, jc.on)
				continue
			}
			if pos > jc.pos {
				t.Errorf("JOIN 子句的 ON 引用了**后面才定义**的别名 %q"+
					"(别名位置 %d,ON 位置 %d):%s\n"+
					"T-SQL 的 FROM 从左到右求值,这条 SQL 在源库会直接语法错。"+
					"实测报 '无法绑定由多个部分组成的标识符 %s.x'。",
					used, pos, jc.pos, jc.on, used)
			}
		}
	}
}

// TestJoin_TopologicalOrderIsStable 锁住"同样输入永远得到同一份 SQL"。
//
// 别名是序号,顺序一变 SQL 文本就变 —— 而 SQL 文本是要进日志做回归比对的,
// 漂了就没法 diff 出"这次改了什么"。同深度之间的次序由 From->To 兜底,
// 这条用例让那份兜底规则显式化,免得以后有人"顺手改成 map 遍历"。
func TestJoin_TopologicalOrderIsStable(t *testing.T) {
	base, entries := salesReportChain()
	reg := NewRegistry(entries)
	lim := 10
	q := &Query{
		Measures:   []string{"sales_report.count"},
		Dimensions: []string{"item_cls_parent.id", "item_cls_parent.name"},
		Limit:      &lim,
	}
	first, err := BuildTSQLJoin(q, base, "t_rm_daysum", nil, reg)
	if err != nil {
		t.Fatalf("BuildTSQLJoin: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := BuildTSQLJoin(q, base, "t_rm_daysum", nil, reg)
		if err != nil {
			t.Fatalf("BuildTSQLJoin #%d: %v", i, err)
		}
		if got.SQL != first.SQL {
			t.Fatalf("第 %d 次编译出的 SQL 与第一次不同 —— 别名分配不是确定性的:\n1: %s\n%d: %s",
				i+1, first.SQL, i+2, got.SQL)
		}
	}
}

// TestJoin_SingleEdgeUnaffected 单边查询不受影响(回归护栏)。
//
// 这是"修顺序"最容易踩的坑:为了拓扑序改坏了最常见的一跳查询。
func TestJoin_SingleEdgeUnaffected(t *testing.T) {
	base, entries := salesReportChain()
	reg := NewRegistry(entries)
	lim := 10
	q := &Query{
		Measures:   []string{"sales_report.count"},
		Dimensions: []string{"item.id"},
		Limit:      &lim,
	}
	res, err := BuildTSQLJoin(q, base, "t_rm_daysum", nil, reg)
	if err != nil {
		t.Fatalf("BuildTSQLJoin: %v", err)
	}
	from := strings.Index(res.SQL, "FROM ")
	tail := res.SQL[from:]
	iItem := strings.Index(tail, "t_bd_item_info AS ")
	if iItem < 0 {
		t.Fatalf("没有 join 到 t_bd_item_info: %s", tail)
	}
	iOn := strings.Index(tail, " ON ")
	if iOn < 0 || iOn < iItem {
		t.Fatalf("ON 出现在被 join 的表之前: %s", tail)
	}
	// 一跳查询只应引入**一个**别名(加 base 共 2 个)。
	if n := strings.Count(tail, " AS t"); n != 2 {
		t.Errorf("一跳查询引入了 %d 个别名(want 2 —— 多连一张没人用的表就是白拖慢源库): %s", n, tail)
	}
}

type aliasRef struct {
	alias string
	pos   int
}

type joinClause struct {
	on  string
	pos int // 该子句 " ON " 在 FROM 子句里的位置
}

func regexpAsAlias(s string) []aliasRef {
	var out []aliasRef
	for i := 0; i < len(s); i++ {
		if !strings.HasPrefix(s[i:], " AS t") {
			continue
		}
		j := i + len(" AS t")
		k := j
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k == j {
			continue
		}
		out = append(out, aliasRef{alias: s[i+4 : k], pos: i})
		i = k
	}
	return out
}

// joinClausesInOrder 切出 FROM 子句里每条 "<KW> JOIN <tbl> AS tN ... ON <cond>"。
//
// pos 记的是 " ON " 自己在 tail 里的绝对位置 —— 那才是 T-SQL 开始按
// From 顺序解析这条 JOIN 条件的时刻。
func joinClausesInOrder(from string) []joinClause {
	var out []joinClause
	for i := 0; i < len(from); i++ {
		if !strings.HasPrefix(from[i:], "JOIN ") {
			continue
		}
		on := strings.Index(from[i:], " ON ")
		if on < 0 {
			break
		}
		cond := from[i+on+len(" ON "):]
		// ON 条件到下一个子句关键字为止。
		// ⚠️ 只按 " JOIN " 截会把结尾的 LEFT / GROUP BY / ORDER BY 也吃进来,
		//   结果条件里混进无关 token,断言就变成在给噪声报错。
		end := len(cond)
		for _, stop := range []string{" JOIN ", " WHERE ", " GROUP ", " ORDER ", " HAVING "} {
			if n := strings.Index(cond, stop); n >= 0 && n < end {
				end = n
			}
		}
		out = append(out, joinClause{on: strings.TrimSpace(cond[:end]), pos: i + on})
		i += on
	}
	return out
}

func aliasesIn(cond string) []string {
	var out []string
	for i := 0; i < len(cond); i++ {
		if !strings.HasPrefix(cond[i:], "t") {
			continue
		}
		j := i + 1
		k := j
		for k < len(cond) && cond[k] >= '0' && cond[k] <= '9' {
			k++
		}
		if k == j {
			continue
		}
		out = append(out, cond[i:k])
		i = k
	}
	return out
}
