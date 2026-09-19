// duckdb_test 集成测试:在 sandbox 里尝试打开内存 DuckDB + 跑 SQL。
//
// 注意:此测试要求运行环境有 libduckdb 可用。
//   - Linux: 装 libduckdb.so 到 /usr/local/lib 或设 DUCKDB_LIBRARY_PATH
//   - macOS: brew install duckdb 或 DUCKDB_LIBRARY_PATH 指向 libduckdb.dylib
//   - Windows: 把 duckdb.dll 放到 PATH 或设 DUCKDB_LIBRARY_PATH
//
// 如果 libduckdb 不可用,测试会 skip 而不是 fail。
package duckdb_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
)

// hasDuckDBLib 检查 libduckdb 是否可用(快速环境探测)。
func hasDuckDBLib(t *testing.T) bool {
	t.Helper()
	if v := os.Getenv("DUCKDB_LIBRARY_PATH"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return true
		}
	}
	// 探测常见路径
	candidates := []string{
		"/usr/local/lib/libduckdb.so",
		"/opt/homebrew/lib/libduckdb.dylib",
		"C:\\Program Files\\duckdb\\duckdb.dll",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			t.Logf("found libduckdb at %s", p)
			return true
		}
	}
	t.Log("libduckdb not found in common paths; set DUCKDB_LIBRARY_PATH")
	return false
}

func TestOpenAndQuery(t *testing.T) {
	if !hasDuckDBLib(t) {
		t.Skip("libduckdb not available; skipping integration test")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "test.duckdb")

	eng, err := duckdb.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer eng.Close()

	// 建表 + 灌数据
	if err := eng.Exec("CREATE TABLE supplier (id VARCHAR, name VARCHAR)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := eng.Exec("INSERT INTO supplier VALUES ('S1', '可口可乐'), ('S2', '百事可乐')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 查询
	rows, err := eng.QueryMap("SELECT id, name FROM supplier ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0]["id"] != "S1" || rows[0]["name"] != "可口可乐" {
		t.Errorf("row 0: %v", rows[0])
	}
}

func TestBuildPreAgg(t *testing.T) {
	if !hasDuckDBLib(t) {
		t.Skip("libduckdb not available")
	}

	dir := t.TempDir()
	eng, err := duckdb.Open(filepath.Join(dir, "preagg.duckdb"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer eng.Close()

	// BuildPreAgg:CREATE OR REPLACE TABLE
	if err := eng.BuildPreAgg("supplier_summary",
		"SELECT 'all' AS category, COUNT(*) AS cnt FROM (VALUES ('a'), ('b'), ('c')) AS t(x)"); err != nil {
		t.Fatalf("build preagg: %v", err)
	}

	exists, err := eng.TableExists("supplier_summary")
	if err != nil {
		t.Fatalf("table exists: %v", err)
	}
	if !exists {
		t.Errorf("supplier_summary should exist")
	}

	rows, err := eng.QueryMap("SELECT cnt FROM supplier_summary")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if cnt, ok := rows[0]["cnt"].(int64); !ok || cnt != 3 {
		t.Errorf("cnt want int64(3), got %T(%v)", rows[0]["cnt"], rows[0]["cnt"])
	}
}

func TestLoadFrom(t *testing.T) {
	if !hasDuckDBLib(t) {
		t.Skip("libduckdb not available")
	}

	dir := t.TempDir()
	eng, err := duckdb.Open(filepath.Join(dir, "loadfrom.duckdb"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer eng.Close()

	columns := []fieldmapping.FieldDef{
		{Target: "id", Type: fieldmapping.TypeString},
		{Target: "name", Type: fieldmapping.TypeString},
	}
	rows := [][]any{
		{"S1", "可口可乐"},
		{"S2", "百事可乐"},
		{"S3", "雪碧"},
	}
	if err := eng.LoadFrom("supplier_raw", columns, rows); err != nil {
		t.Fatalf("load from: %v", err)
	}

	got, err := eng.QueryMap("SELECT id, name FROM supplier_raw ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 rows, got %d", len(got))
	}
}