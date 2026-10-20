package cubequery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/YunBright/cube/pkg/duckdb"
)

// hasDuckDBLib 探测 libduckdb 是否可用。
//
// 与 pkg/duckdb 自己的 helper 同款(那个在 package duckdb_test 里,导不出来)。
// 探测不到就**跳过**而不是失败 —— CI / 开发机不一定装了 libduckdb,
// 而"环境缺库"不该被当成"功能坏了"。
func hasDuckDBLib(t *testing.T) bool {
	t.Helper()
	if v := os.Getenv("DUCKDB_LIBRARY_PATH"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return true
		}
	}
	for _, p := range []string{
		"/usr/local/lib/libduckdb/libduckdb.so",
		"/usr/local/lib/libduckdb.so",
		"/opt/homebrew/lib/libduckdb.dylib",
		`C:\Program Files\duckdb\duckdb.dll`,
	} {
		if _, err := os.Stat(p); err == nil {
			t.Logf("found libduckdb at %s", p)
			return true
		}
	}
	t.Log("libduckdb not found; set DUCKDB_LIBRARY_PATH")
	return false
}

// TestBuild_ContainsCI_EndToEndInDuckDB 真跑一次 DuckDB。
//
// # 为什么 SQL 层的断言不够
//
// `TestBuild_ContainsCIIsCaseInsensitive` 只证明"生成了什么 SQL",
// 证明不了"这个 SQL 在 DuckDB 里真的大小写无关" —— 而后者才是功能本身。
// DuckDB 的 LIKE 默认大小写敏感(`ILIKE` 才不敏感),而本编译器不暴露 ILIKE。
//
// 所以这里把 **Build() 编译出来的 SQL 原样**丢进真 DuckDB 跑,
// 断言的是"用户搜 face 能不能搜到 Face"这个**功能事实**。
//
// # 用真实数据形态
//
// 灌的行刻意还原了思迅 char 列的**尾随空格**(RTRIM 的存在理由)
// 与商品名里混拉丁品牌名的形态(洁柔Face / C&S / H01-04)。
func TestBuild_ContainsCI_EndToEndInDuckDB(t *testing.T) {
	if !hasDuckDBLib(t) {
		t.Skip("libduckdb not available; skipping DuckDB integration test")
	}

	dir := t.TempDir()
	eng, err := duckdb.Open(filepath.Join(dir, "containsci.duckdb"))
	if err != nil {
		t.Fatalf("打开 DuckDB 失败: %v", err)
	}
	defer eng.Close()

	mustExec := func(sql string) {
		t.Helper()
		if err := eng.Exec(sql); err != nil {
			t.Fatalf("执行失败: %v\nSQL: %s", err, sql)
		}
	}
	mustExec(`CREATE TABLE product (id VARCHAR, name VARCHAR)`)
	// 注意 '洁柔Face...' 后面带了尾随空格 —— 思迅 char 列就是这个形态。
	mustExec(`INSERT INTO product VALUES
		('1', '洁柔Face黑软抽纸130抽（8包装）102-08   '),
		('2', '清风金装120*10抽纸'),
		('3', 'face小样'),
		('4', 'C&S厨房纸巾')`)

	cases := []struct {
		name  string
		op    string
		value string
		want  []string // 期望命中的 product.id
	}{
		{"contains 大小写敏感:小写只命中小写行", "contains", "face", []string{"3"}},
		{"contains 大小写敏感:大写只命中大写行", "contains", "Face", []string{"1"}},
		{"containsCI 小写命中大小写两种", "containsCI", "face", []string{"1", "3"}},
		{"containsCI 大写命中大小写两种", "containsCI", "FACE", []string{"1", "3"}},
		{"containsCI 混合大小写", "containsCI", "FaCe", []string{"1", "3"}},
		{"containsCI 命中尾随空格行(RTRIM 仍然生效)", "containsCI", "102-08", []string{"1"}},
		{"containsCI 中文照常", "containsCI", "抽纸", []string{"1", "2"}},
		{"containsCI 品牌名含 &", "containsCI", "c&s", []string{"4"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &Query{
				Measures:   []string{"product.count"},
				Dimensions: []string{"product.id"},
				Filters: []Filter{{
					Member:   "product.name",
					Operator: tc.op,
					Values:   []any{tc.value},
				}},
			}
			res := mustBuild(t, q)
			rows, err := eng.QueryMap(res.SQL, res.Args...)
			if err != nil {
				t.Fatalf("查询失败: %v\nSQL: %s", err, res.SQL)
			}
			got := make([]string, 0, len(rows))
			for _, r := range rows {
				got = append(got, asStr(r["product.id"]))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("命中 %v,期望 %v\nSQL: %s", got, tc.want, res.SQL)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("命中 %v,期望 %v\nSQL: %s", got, tc.want, res.SQL)
				}
			}
		})
	}
}

// asStr 把查询结果里的任意值转成字符串(DuckDB 的 VARCHAR 回来是 string)。
func asStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
