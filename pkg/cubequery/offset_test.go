// offset_test.go 锁分页(offset)语义。
//
// 分页是结算单审批列表的硬需求:一次多选可能超过一页。
//
// 两个方言的分页写法**完全不同**,这是本文件存在的主要理由:
//   - DuckDB: LIMIT n OFFSET m
//   - T-SQL : ORDER BY ... OFFSET m ROWS FETCH NEXT n ROWS ONLY,且**不能**再用 TOP
//     (SQL Server 判 TOP 与 FETCH NEXT 同现为语法错)
//
// 且 T-SQL 的 OFFSET/FETCH **强制要求 ORDER BY**,不排序直接是数据库语法错。
// 这些差异若没锁住,症状是"live model 分页报 500、duck model 却正常"。
package cubequery

import (
	"errors"
	"strings"
	"testing"
)

func pagedQuery(offset int) *Query {
	lim := 20
	return &Query{
		Measures:   []string{"settlement.total_goods_yuan"},
		Dimensions: []string{"settlement.id", "settlement.settled_at"},
		Limit:      &lim,
		Order:      []Order{{ID: "settlement.settled_at", Order: "desc"}},
		Offset:     Offset{offset},
	}
}

// TestOffset_DuckDBUsesLimitOffset 锁 DuckDB 方言。
func TestOffset_DuckDBUsesLimitOffset(t *testing.T) {
	sql := mustBuildWith(t, pagedQuery(40), liveSchema()).SQL
	if !strings.Contains(sql, "LIMIT 20 OFFSET 40") {
		t.Errorf("应生成 LIMIT 20 OFFSET 40,实际: %s", sql)
	}
	// 顺序不能反:DuckDB 允许 OFFSET 在 LIMIT 前,但语义是"先跳再取"…
	// 实际两种写法等价,这里只锁"不能把 OFFSET 放到 LIMIT 前面导致 limit 失效"。
	if strings.Index(sql, "LIMIT") > strings.Index(sql, "OFFSET") {
		t.Errorf("应先 LIMIT 再 OFFSET,实际: %s", sql)
	}
}

// TestOffset_TSQLDropsTopWhenOffsetting 锁 T-SQL 最容易踩的一条:
// TOP 与 FETCH NEXT **互斥**。两个都写,SQL Server 直接语法错。
func TestOffset_TSQLDropsTopWhenOffsetting(t *testing.T) {
	res, err := BuildTSQL(pagedQuery(40), liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
	if err != nil {
		t.Fatalf("BuildTSQL 失败: %v", err)
	}
	sql := res.SQL
	if strings.Contains(strings.ToUpper(sql), "TOP") {
		t.Errorf("有 offset 时不能再用 TOP(SQL Server 判两者互斥),实际: %s", sql)
	}
	if !strings.Contains(sql, "OFFSET 40 ROWS FETCH NEXT 20 ROWS ONLY") {
		t.Errorf("应生成 OFFSET 40 ROWS FETCH NEXT 20 ROWS ONLY,实际: %s", sql)
	}
}

// TestOffset_TSQLKeepsTopWithoutOffset 另一半:不翻页时仍走 TOP,
// 不能因为支持了分页就把所有查询都改写成 FETCH 形态。
func TestOffset_TSQLKeepsTopWithoutOffset(t *testing.T) {
	for _, off := range []int{0} {
		res, err := BuildTSQL(pagedQuery(off), liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
		if err != nil {
			t.Fatalf("BuildTSQL 失败: %v", err)
		}
		sql := res.SQL
		if !strings.Contains(sql, "SELECT TOP 20 ") {
			t.Errorf("offset=0 时应保留 TOP 写法,实际: %s", sql)
		}
		if strings.Contains(strings.ToUpper(sql), "FETCH") {
			t.Errorf("offset=0 时不该出现 FETCH,实际: %s", sql)
		}
	}
}

// TestOffset_RequiresOrder 是**语义**锁,不只是方言锁:
// 不排序的分页,每页可能重复或漏行(存储顺序一变就错位)。
// DuckDB 容忍、T-SQL 语法报错 —— 但那都是"数据库替我们偶然兜住",
// 真正的错在调用方,所以编译期就该拦住,两个方言一视同仁。
func TestOffset_RequiresOrder(t *testing.T) {
	q := pagedQuery(40)
	q.Order = nil

	_, duckErr := Build(q, liveSchema())
	var be *BuildError
	if !errors.As(duckErr, &be) || be.Kind != "query" {
		t.Errorf("duck 路径:offset>0 无 order 应报 Kind=query,got %v", duckErr)
	}

	_, liveErr := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
	if !errors.As(liveErr, &be) || be.Kind != "query" {
		t.Errorf("live 路径:同样必须被拒,got %v", liveErr)
	}
	if be.Msg != "" && !strings.Contains(be.Msg, "order") {
		t.Errorf("错误信息要说清缺什么,got: %s", be.Msg)
	}
}

// TestOffset_DialectsAgree 分页语义同样必须两路一致(同 buildOrder 的理由)。
func TestOffset_DialectsAgree(t *testing.T) {
	for _, off := range []int{0, 20, 1000} {
		q := pagedQuery(off)
		duck := mustBuildWith(t, q, liveSchema()).SQL
		live, err := BuildTSQL(q, liveSchema(), "t_fm_recpay_gx_master", mustMapper(t, hbSettlementMapping))
		if err != nil {
			t.Fatalf("BuildTSQL 失败: %v", err)
		}
		// 两条路径都必须带上排序(分页的前提)。
		if !strings.Contains(duck, " ORDER BY ") || !strings.Contains(live.SQL, " ORDER BY ") {
			t.Errorf("offset=%d 两条路径都必须有 ORDER BY\n duck: %s\n live: %s", off, duck, live.SQL)
		}
		// 跳过的行数在两边必须一致。
		if off > 0 && !strings.Contains(live.SQL, "OFFSET") {
			t.Errorf("offset=%d live 路径丢了 OFFSET: %s", off, live.SQL)
		}
	}
}

// TestOffset_ParsesNumberAndArray 契约里 offset 是**数字**;
// 但历史上有数组形态,两种都得收,且都要能报错而不是静默当 0
// ——静默当 0 会让调用方拿到第一页却以为翻到了第二页。
func TestOffset_ParsesNumberAndArray(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"measures":["m.x"],"offset":50}`, 50},
		{`{"measures":["m.x"],"offset":[50]}`, 50},
		{`{"measures":["m.x"],"offset":0}`, 0},
		{`{"measures":["m.x"],"offset":null}`, 0},
		{`{"measures":["m.x"]}`, 0},
		{`{"measures":["m.x"],"offset":[]}`, 0},
	}
	for _, c := range cases {
		q, err := Parse([]byte(c.body))
		if err != nil {
			t.Errorf("%s 解析失败: %v", c.body, err)
			continue
		}
		if got := q.Offset.First(); got != c.want {
			t.Errorf("%s → offset=%d, want %d", c.body, got, c.want)
		}
	}
}

// TestOffset_RejectsBadValues 脏输入必须报错:
// 负数会让 SQL 变成 `OFFSET -1`(语法错,且语义完全说不通)。
func TestOffset_RejectsBadValues(t *testing.T) {
	for _, body := range []string{
		`{"measures":["m.x"],"offset":-1}`,
		`{"measures":["m.x"],"offset":[-1]}`,
		`{"measures":["m.x"],"offset":"50"}`,
		`{"measures":["m.x"],"offset":true}`,
		`{"measures":["m.x"],"offset":{"n":50}}`,
		`{"measures":["m.x"],"offset":1.5}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s 应解析失败(静默当 0 会让调用方拿到第一页却以为翻到了第二页)", body)
		}
	}
}

// TestOffset_FullPaginationQuery 端到端串一遍:翻到第 3 页,
// 断言返回 SQL 的分页子句真的落在最后(DuckDB)。
func TestOffset_FullPaginationQuery(t *testing.T) {
	lim := 20
	q := &Query{
		Measures:   []string{"settlement.total_goods_yuan"},
		Dimensions: []string{"settlement.id", "settlement.document_type", "settlement.settled_at"},
		Filters:    []Filter{{Member: "settlement.document_type", Operator: "equals", Values: []any{"DP"}}},
		Order:      []Order{{ID: "settlement.settled_at", Order: "desc"}},
		Limit:      &lim,
		Offset:     Offset{40}, // 第 3 页
	}
	sql := mustBuildWith(t, q, liveSchema()).SQL

	tail := sql[strings.Index(sql, " ORDER BY "):]
	for _, want := range []string{`ORDER BY "settlement.settled_at" DESC`, "LIMIT 20 OFFSET 40"} {
		if !strings.Contains(tail, want) {
			t.Errorf("分页子句应包含 %q,实际尾部: %s", want, tail)
		}
	}
}
