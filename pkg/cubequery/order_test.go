// order_test.go 锁 ORDER BY 语义。
//
// 这个特性的存在理由:结算单列表场景。ysx 有 4348 张结算单、hbposv7 有 799 张,
// limit=1000 时 TOP/LIMIT 会**任意截取**,返回哪 1000 行完全由存储顺序决定。
// 对"展示日期与金额"的审批列表来说,那是不可用的输出。
//
// 两条编译路径(Build → DuckDB / BuildTSQL → T-SQL)必须给出**同一套**排序语义,
// 所以本文件的核心不是"各自能排序",而是 TestOrder_DialectsAgree 那一条:
// 同一条 query 过两条路径,ORDER BY 片段逐字相同(只差别名引号)。
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
	// 排序的是**别名**,所以是 [settlement.settled_at] 而不是源列 [settle_date]。
	if !strings.Contains(frag, "[settlement.settled_at] DESC") {
		t.Errorf("应按 SELECT 别名 DESC 排序,实际: %s", frag)
	}
	if strings.Contains(frag, "settle_date") {
		t.Errorf("ORDER BY 不该用源列名(别名才是响应体的 key),实际: %s", frag)
	}
}

// TestOrder_DialectsAgree 是本文件最重要的一条:同一份契约被两个编译器消费,
// 语义必须一致 —— 这正是 build.go 顶部 hbposv7 残缺实现的教训。
//
// 这里刻意用**同一个 schema**(liveSchema)过两条路径:Build 不校验 storage,
// 所以它能编译,输出的 FROM 会是 schema.SQLTable 而不是 config 里的源表名。
// 差异只在 FROM/TOP/LIMIT,ORDER BY 必须逐字相同(只差 "[] → "" 的别名引号)。
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

			// ORDER BY 后面的东西(有没有、是哪些、什么方向)必须一致。
			duckFrag, tsqlFrag := "", ""
			if i := strings.Index(duckSQL, " ORDER BY "); i >= 0 {
				duckFrag = strings.TrimSuffix(duckSQL[i:], "")
				if j := strings.Index(duckFrag, " LIMIT "); j >= 0 {
					duckFrag = duckFrag[:j]
				}
			}
			if i := strings.Index(tsqlSQL.SQL, " ORDER BY "); i >= 0 {
				tsqlFrag = tsqlSQL.SQL[i:]
			}
			// 别名引号两边不同(DuckDB "x" / T-SQL [x]),剥掉引号字符后逐字比对:
			// 留下的是真正要保证一致的东西 —— 有哪些成员、什么方向、什么顺序。
			norm := func(s string) string {
				return strings.NewReplacer(`"`, "", "[", "", "]", "").Replace(s)
			}
			if norm(duckFrag) != norm(tsqlFrag) {
				t.Fatalf("两条路径的 ORDER BY 语义分叉了\n duck: %q\n live: %q", duckFrag, tsqlFrag)
			}
			// 防"两边都忽略 order 也算一致"的空跑:
			// 排序成员一个都没给时片段必须为空,给了就必须非空。
			if len(c.order) == 0 && norm(duckFrag) != "" {
				t.Fatalf("没给 order 却生成了 ORDER BY: %q", duckFrag)
			}
			if len(c.order) > 0 && norm(duckFrag) == "" {
				t.Fatal("给了 order 却没生成 ORDER BY")
			}
		})
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
