// order_test.go 锁 ORDER BY 语义。
//
// 这个特性的存在理由:结算单列表场景。ysx 有 4348 张结算单、hbposv7 有 799 张,
// limit=1000 时 TOP/LIMIT 会**任意截取**,返回哪 1000 行完全由存储顺序决定。
// 对"展示日期与金额"的审批列表来说,那是不可用的输出。
//
// 两条编译路径(Build → DuckDB / BuildTSQL → T-SQL)必须给出**同一套**排序语义,
// 所以本文件的核心不是"各自能排序",而是 TestOrder_DialectsAgree 那一条:
// 同一条 query 过两条路径,ORDER BY 的**成员、方向、顺序**必须一致。
//
// ⚠️ 一致的是**语义**,不是逐字文本(2026-10-23 改):
// 两条路的投影形态不同 ⇒ ORDER BY 的渲染形态也必须不同
// (duck 按别名排、live 按底层表达式排,详见 TestOrder_DialectsAgree 的注释)。
package cubequery

import (
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// orderByOf 抠出 SQL 里的 ORDER BY 片段。
func orderByOf(t *testing.T, sql string) string {
	t.Helper()
	i := strings.Index(sql, " ORDER BY ")
	if i < 0 {
		t.Fatalf("SQL 里没有 ORDER BY: %s", sql)
	}
	frag := sql[i:]
	if j := strings.Index(frag, " LIMIT "); j >= 0 {
		frag = frag[:j]
	}
	return frag
}

// mustBuildWith 让同一份 query 能过指定 schema —— 跨方言一致性测试要用
// liveSchema() 同时喂 Build 和 BuildTSQL(Build 不校验 storage,能编译 live schema)。
func mustBuildWith(t *testing.T, q *Query, schema *cubeschema.Model) *Result {
	t.Helper()
	res, err := Build(q, schema)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	return res
}

// TestBuild_EmitsOrderByBeforeLimit 锁一个"SQL 合法但语义错"的坑:
// DuckDB 允许 `... LIMIT 10 ORDER BY x`(顺序无约束),但那样是**先截断再排序** ——
// 返回的是任意 1000 行里最大的 10 个,而不是全部数据里最大的 10 个。
func TestBuild_EmitsOrderByBeforeLimit(t *testing.T) {
	lim := 10
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Limit:      &lim,
		Order:      []Order{{ID: "product.id", Order: "asc"}},
	}
	sql := mustBuild(t, q).SQL

	orderIdx := strings.Index(sql, " ORDER BY ")
	limitIdx := strings.Index(sql, " LIMIT ")
	if orderIdx < 0 || limitIdx < 0 {
		t.Fatalf("应同时有 ORDER BY 与 LIMIT,实际: %s", sql)
	}
	if orderIdx > limitIdx {
		t.Fatalf("ORDER BY 必须在 LIMIT 之前,否则是\"先截断再排序\",实际: %s", sql)
	}
}

// TestBuildTSQL_EmitsOrderByAtEnd 锁 T-SQL 位置:T-SQL 没有 LIMIT,
// TOP 在 SELECT 里就已定好行数,但 TOP 取的是**排序后**的前 n 行,所以语义正确。
func TestBuildTSQL_EmitsOrderByAtEnd(t *testing.T) {
	lim := 20
	q := &Query{
		Measures:   []string{"settlement.total_goods_yuan"},
		Dimensions: []string{"settlement.id", "settlement.settled_at"},
		Limit:      &lim,
		Order:      []Order{{ID: "settlement.settled_at", Order: "desc"}},
	}
	res, err := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	if !strings.Contains(sql, "SELECT TOP 20 ") {
		t.Errorf("应保留 TOP,实际: %s", sql)
	}
	if strings.Contains(strings.ToUpper(sql), "LIMIT") {
		t.Errorf("T-SQL 不支持 LIMIT,实际: %s", sql)
	}
	frag := orderByOf(t, sql)
	// ⚠️ live 路按**底层表达式**排,不是按 SELECT 别名:
	// 它的投影一律 RTRIM ⇒ 别名是**文本** ⇒ 按别名排 = 按字典序排
	// (datetime 排成 "Jan  1 2025  4:11PM"、number 排成 "100" < "9")。
	// ⇒ 排序必须落在底层 datetime 列上。
	if !strings.Contains(frag, "settle_date DESC") {
		t.Errorf("应按底层 datetime 表达式 DESC 排序,实际: %s", frag)
	}
	if strings.Contains(frag, "[settlement.settled_at]") {
		t.Errorf("ORDER BY 不该用 RTRIM 后的别名(那是文本,排序退化成字典序),实际: %s", frag)
	}
	// ⚠️ 但**响应体的 key 不变**:SELECT 侧仍投影别名(返回明文文本,下游零改动)。
	if !strings.Contains(sql, "AS [settlement.settled_at]") {
		t.Errorf("SELECT 侧必须仍投影别名(响应体的 key),实际: %s", sql)
	}
}

// TestOrder_DialectsAgree 是本文件最重要的一条:同一份契约被两个编译器消费,
// 语义必须一致 —— 这正是 build.go 顶部 hbposv7 残缺实现的教训。
//
// 这里刻意用**同一个 schema**(liveSchema)过两条路径:Build 不校验 storage,
// 所以它能编译,输出的 FROM 会是 schema.SQLTable 而不是 config 里的源表名。
//
// # 比较的是**语义**,不是渲染形态
//
// 两条路径的 ORDER BY **成员、方向、顺序**必须一致;
// 但**渲染形态可以不同**,而且必须不同:
//
//	duck(本地副本那条路):按 SELECT **别名** —— 它的投影是类型感知的
//	  (datetime 不包 RTRIM)⇒ 别名里就是真 datetime ⇒ 按别名排 = 按时间排。
//	live(生产在跑的那条):按**底层表达式** —— 它的投影一律 RTRIM
//	  ⇒ 别名是**文本** ⇒ 按别名排 = 按字典序排(见 tsql.go 的 orderTermsTSQL)。
//
// ⇒ 逐字比对会把"渲染形态不同"误判成"语义分叉"(2026-10-23 之前就是逐字比对的,
// live 路改成按表达式排之后它红了 —— 那次红是**正确的**:live 路原先的渲染是错的)。
func TestOrder_DialectsAgree(t *testing.T) {
	cases := []struct {
		name  string
		order []Order
	}{
		{"单个维度降序", []Order{{ID: "settlement.settled_at", Order: "desc"}}},
		{"省略方向=升序", []Order{{ID: "settlement.id"}}},
		{"按度量排序", []Order{{ID: "settlement.total_goods_yuan", Order: "desc"}}},
		{"多字段排序", []Order{
			{ID: "settlement.document_type", Order: "asc"},
			{ID: "settlement.settled_at", Order: "desc"},
		}},
		{"无排序", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lim := 30
			q := &Query{
				Measures:   []string{"settlement.total_goods_yuan", "settlement.count"},
				Dimensions: []string{"settlement.id", "settlement.document_type", "settlement.settled_at"},
				Limit:      &lim,
				Order:      c.order,
			}
			duckSQL := mustBuildWith(t, q, liveSchema()).SQL
			tsqlSQL, err := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
			if err != nil {
				t.Fatalf("BuildTSQL 失败: %v", err)
			}

			// 排序成员的**权威清单**:两条路径的校验与归一都出自它。
			members, err := resolveOrder(q, liveSchema(), "settlement", nil)
			if err != nil {
				t.Fatalf("resolveOrder 失败: %v", err)
			}
			duckTerms := orderTermsOf(t, duckSQL)
			liveTerms := orderTermsOf(t, tsqlSQL.SQL)

			if len(duckTerms) != len(members) || len(liveTerms) != len(members) {
				t.Fatalf("两条路径的 ORDER BY 项数与成员数不一致\n 成员: %d\n duck: %d %v\n live: %d %v",
					len(members), len(duckTerms), duckTerms, len(liveTerms), liveTerms)
			}
			for i, m := range members {
				// 方向必须逐项一致(这是"翻页顺序"真正依赖的东西)。
				if !strings.HasSuffix(duckTerms[i], " "+m.Dir) {
					t.Errorf("第 %d 项方向应是 %s,duck 实际: %q", i, m.Dir, duckTerms[i])
				}
				if !strings.HasSuffix(liveTerms[i], " "+m.Dir) {
					t.Errorf("第 %d 项方向应是 %s,live 实际: %q", i, m.Dir, liveTerms[i])
				}
				// duck 那条路只有别名可用(它的投影是类型感知的)⇒ 逐字必须是别名。
				if want := `"` + m.Alias + `" ` + m.Dir; duckTerms[i] != want {
					t.Errorf("第 %d 项 duck 应逐字是 %q,实际: %q", i, want, duckTerms[i])
				}
			}
			// 防"两边都忽略 order 也算一致"的空跑:
			// 排序成员一个都没给时片段必须为空,给了就必须非空。
			if len(c.order) == 0 && len(duckTerms) != 0 {
				t.Fatalf("没给 order 却生成了 ORDER BY: %v", duckTerms)
			}
			if len(c.order) > 0 && len(duckTerms) == 0 {
				t.Fatal("给了 order 却没生成 ORDER BY")
			}
		})
	}
}

// groupByOf 抠出 SQL 里的 GROUP BY 片段。
func groupByOf(t *testing.T, sql string) string {
	t.Helper()
	i := strings.Index(sql, " GROUP BY ")
	if i < 0 {
		t.Fatalf("SQL 里没有 GROUP BY: %s", sql)
	}
	frag := sql[i+len(" GROUP BY "):]
	if j := strings.Index(frag, " ORDER BY "); j >= 0 {
		frag = frag[:j]
	}
	return frag
}

// orderTermsOf 拆出 SQL 里 ORDER BY 的每一项(按**顶层逗号**拆 ——
// 表达式里可能带逗号,如 SUM(a, b))。
func orderTermsOf(t *testing.T, sql string) []string {
	t.Helper()
	i := strings.Index(sql, " ORDER BY ")
	if i < 0 {
		return nil
	}
	frag := sql[i+len(" ORDER BY "):]
	if j := strings.Index(frag, " LIMIT "); j >= 0 {
		frag = frag[:j]
	}
	var out []string
	depth, start := 0, 0
	for i, r := range frag {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(frag[start:i]))
				start = i + 1
			}
		}
	}
	if s := strings.TrimSpace(frag[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// TestBuildTSQL_OrdersByUnderlyingExpression 锁 2026-10-23 修的那条真缺陷:
//
// live 路的 SELECT 一律 RTRIM ⇒ 别名是**文本** ⇒ 按别名排 = 按**字典序**排:
//
//	datetime:RTRIM(datetime) 的文本形如 "Jan  1 2025  4:11PM"
//	  ⇒ 1 月排在 2 月前面;同一天里 " 4:11PM" 排在 " 9:02AM" 前面
//	    (单数日的小时前面多一个空格)
//	number:RTRIM(number) 的文本形如 "100" ⇒ "100" < "9"
//
// 实测症状(生产):促销方案列表"按制单日期排"⇒ 顺序是乱的,而且**零报错**。
//
// ⇒ ORDER BY 必须落在**底层表达式**上(与 GROUP BY 里排的是同一份)。
func TestBuildTSQL_OrdersByUnderlyingExpression(t *testing.T) {
	lim := 30
	q := &Query{
		Measures:   []string{"settlement.count"},
		Dimensions: []string{"settlement.id", "settlement.settled_at"},
		Limit:      &lim,
		Order:      []Order{{ID: "settlement.settled_at", Order: "desc"}},
	}
	res, err := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	terms := orderTermsOf(t, res.SQL)
	if len(terms) != 1 {
		t.Fatalf("应只有一项排序,实际: %v", terms)
	}
	// 底层 datetime 列(源列名 settle_date)—— 不是 RTRIM 后的别名。
	if !strings.Contains(terms[0], "settle_date") {
		t.Errorf("应按底层 datetime 列排序,实际: %q", terms[0])
	}
	if strings.Contains(terms[0], "[settlement.settled_at]") {
		t.Errorf("不能按 RTRIM 后的别名排(那是文本 ⇒ 字典序),实际: %q", terms[0])
	}
	// ⚠️ GROUP BY 与 ORDER BY 必须是**同一份表达式** ——
	// 否则 T-SQL 会报 "not a grouped column"。
	groupBy := groupByOf(t, res.SQL)
	if !strings.Contains(groupBy, "settle_date") {
		t.Errorf("GROUP BY 应排裸列(与 ORDER BY 同源),实际: %q", groupBy)
	}
	if strings.Contains(groupBy, "RTRIM") {
		t.Errorf("GROUP BY 不该包 RTRIM(与 ORDER BY 不同源),实际: %q", groupBy)
	}
}

// TestBuildTSQL_OrdersNumberDimensionNumerically 锁数值维度的排序也是数值序。
//
// number 维度的 SELECT 侧**照旧**包 RTRIM(返回值保持明文文本,下游零改动),
// 但 ORDER BY 必须用底层数值列 ⇒ 否则 "100" 会排在 "9" 前面。
func TestBuildTSQL_OrdersNumberDimensionNumerically(t *testing.T) {
	lim := 30
	q := &Query{
		Measures:   []string{"settlement.count"},
		Dimensions: []string{"settlement.id", "settlement.amount_yuan"},
		Limit:      &lim,
		Order:      []Order{{ID: "settlement.amount_yuan", Order: "desc"}},
	}
	res, err := BuildTSQL(q, numFilterSchema(), "t_fm_recpay_gx_master", numFilterMapper(t))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	terms := orderTermsOf(t, res.SQL)
	if len(terms) != 1 {
		t.Fatalf("应只有一项排序,实际: %v", terms)
	}
	if !strings.Contains(terms[0], "sheet_amt") {
		t.Errorf("应按底层数值列排序,实际: %q", terms[0])
	}
	if strings.Contains(terms[0], "RTRIM") {
		t.Errorf("数值列的排序不能经 RTRIM(会把数字转文本 ⇒ 字典序),实际: %q", terms[0])
	}
}

// TestOrder_DefaultsToAsc 方向归一:BI 工具常只给 id 不给 order。
func TestOrder_DefaultsToAsc(t *testing.T) {
	for _, in := range []string{"", "asc", "ASC", " asc "} {
		q := &Query{
			Measures:   []string{"product.count"},
			Dimensions: []string{"product.id"},
			Order:      []Order{{ID: "product.id", Order: in}},
		}
		frag := orderByOf(t, mustBuild(t, q).SQL)
		if !strings.HasSuffix(frag, `"product.id" ASC`) {
			t.Errorf("order=%q 应归一成 ASC,实际片段: %s", in, frag)
		}
	}
	// 大写的 DESC 也要接受 —— cube 契约本身是小写,别因为大小写把人挡在门外。
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Order:      []Order{{ID: "product.id", Order: "DESC"}},
	}
	if frag := orderByOf(t, mustBuild(t, q).SQL); !strings.HasSuffix(frag, `"product.id" DESC`) {
		t.Errorf("order=DESC 应归一成 DESC,实际片段: %s", frag)
	}
}

// TestOrder_RejectsUnknownDirection 方向是**用户可控**的,不在白名单就必须拒。
// 放任透传等于给排序子句开了一个 SQL 文本注入口。
func TestOrder_RejectsUnknownDirection(t *testing.T) {
	for _, bad := range []string{"desc; DROP TABLE t_fm_recpay_gx_master", "1", "asc, id", "(SELECT 1)"} {
		q := &Query{
			Measures:   []string{"product.count"},
			Dimensions: []string{"product.id"},
			Order:      []Order{{ID: "product.id", Order: bad}},
		}
		_, err := Build(q, testSchema())
		var be *BuildError
		if !errors.As(err, &be) || be.Kind != "order" {
			t.Errorf("order=%q 应被拒绝且 Kind=order,got err=%v", bad, err)
		}
	}
}

// TestOrder_RejectsMemberNotInSchema 防注入:order.id 是用户可控的,
// 而它会(经 quote)进 SQL 文本。没在 schema 里就必须拒,而不是当裸字符串拼。
func TestOrder_RejectsMemberNotInSchema(t *testing.T) {
	for _, bad := range []string{
		`product.id" ; DROP TABLE product --`,
		`product.id] ; DROP TABLE product --`,
		`other_model.id`,
		`product.no_such_member`,
	} {
		q := &Query{
			Measures:   []string{"product.count"},
			Dimensions: []string{"product.id"},
			Order:      []Order{{ID: bad, Order: "asc"}},
		}
		res, err := Build(q, testSchema())
		var be *BuildError
		if !errors.As(err, &be) || be.Kind != "order" {
			t.Errorf("order.id=%q 应被拒绝且 Kind=order,got err=%v", bad, err)
			continue
		}
		// 双保险:即便将来校验被改松,SQL 里也不能出现这段原文。
		if res != nil && strings.Contains(res.SQL, "DROP TABLE") {
			t.Errorf("用户输入混进了 SQL 文本: %s", res.SQL)
		}
	}
}

// TestOrder_RequiresMemberToBeSelected 锁一条容易踩的语义:
// ORDER BY 只能引用输出列。选了 measure 却按一个没选的维度排,DB 层会报
// "not a grouped column" —— 那是调用方看不懂的 500。这里在编译期就给出
// "把它加进 dimensions/measures"的可执行提示。
func TestOrder_RequiresMemberToBeSelected(t *testing.T) {
	q := &Query{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Order:      []Order{{ID: "product.name", Order: "asc"}}, // name 是 schema 里的,但没被 select
	}
	_, err := Build(q, testSchema())
	var be *BuildError
	if !errors.As(err, &be) || be.Kind != "order" {
		t.Fatalf("未 select 的排序成员应报错且 Kind=order,got err=%v", err)
	}
	if !strings.Contains(be.Msg, "must also be selected") {
		t.Errorf("错误信息要告诉调用方怎么改,got: %s", be.Msg)
	}
}

// TestOrder_SortsByMeasureWithoutDimensions 纯聚合(无 GROUP BY)也能排序 ——
// "总览页看金额最大的供应商"就是这种查询。
func TestOrder_SortsByMeasureWithoutDimensions(t *testing.T) {
	lim := 5
	q := &Query{
		Measures: []string{"product.avg_price_yuan"},
		Limit:    &lim,
		Order:    []Order{{ID: "product.avg_price_yuan", Order: "desc"}},
	}
	duck := mustBuild(t, q)
	frag := orderByOf(t, duck.SQL)
	if !strings.Contains(frag, `"product.avg_price_yuan" DESC`) {
		t.Errorf("应能按度量别名排序,实际: %s", frag)
	}
	if strings.Contains(duck.SQL, "GROUP BY") {
		t.Errorf("没有 dimension 时不应出现 GROUP BY,实际: %s", duck.SQL)
	}
}
