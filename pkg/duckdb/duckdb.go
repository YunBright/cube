// Package duckdb 封装嵌入 DuckDB,用于预聚合 + 查询执行。
//
// 设计要点(P0-3):
//   - 每个 dapr cube app 一个独立 .duckdb 文件(独立 PV 挂载,WAL 锁零冲突)
//   - 预聚合表由 preagg.go 生成,运行期通过 SQL 查
//   - 复杂运算(JOIN / 窗口函数)由 DuckDB 承担,Go 层只发 SQL
//
// 实现细节:
//   - 使用 github.com/fpt/go-pduckdb(PureGo DuckDB driver),无需 CGO
//   - 不需要装 gcc / MinGW / msys2;只要求运行时机器上有 libduckdb(.so / .dylib / .dll)
//   - 通过环境变量 DUCKDB_LIBRARY_PATH 指定库路径;Linux 还可用 LD_LIBRARY_PATH
//   - 库文件从 https://github.com/duckdb/duckdb/releases 下载
package duckdb

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "github.com/fpt/go-pduckdb" // 匿名 import 注册 duckdb driver

	"github.com/YunBright/cube/pkg/fieldmapping"
)

// Engine 是 DuckDB 嵌入实例。
type Engine struct {
	db   *sql.DB
	path string
	mu    sync.Mutex // DuckDB 单 writer 锁
}

// Open 打开(或创建)一个 .duckdb 文件。
func Open(path string) (*Engine, error) {
	if path == "" {
		return nil, errors.New("duckdb: empty path")
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("duckdb: open %s: %w", path, err)
	}
	// 校验连接(go-pduckdb 在 Open 时不真正连,需要 Ping 触发)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("duckdb: ping %s: %w (libduckdb 装了?设 DUCKDB_LIBRARY_PATH)", path, err)
	}
	return &Engine{db: db, path: path}, nil
}

// Close 关闭连接。
func (e *Engine) Close() error {
	if e.db == nil {
		return nil
	}
	return e.db.Close()
}

// Path 返回当前 .duckdb 文件路径。
func (e *Engine) Path() string { return e.path }

// Exec 执行写操作(单写锁)。
func (e *Engine) Exec(query string, args ...any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.db == nil {
		return errors.New("duckdb: engine not opened")
	}
	_, err := e.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("duckdb: exec: %w (sql: %s)", err, truncate(query, 200))
	}
	return nil
}

// Query 执行读操作。
func (e *Engine) Query(query string, args ...any) (*sql.Rows, error) {
	if e.db == nil {
		return nil, errors.New("duckdb: engine not opened")
	}
	rows, err := e.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("duckdb: query: %w (sql: %s)", err, truncate(query, 200))
	}
	return rows, nil
}

// QueryMap 一次性查询返回 []map[string]any(用于 JSON 输出 / 测试)。
func (e *Engine) QueryMap(query string, args ...any) ([]map[string]any, error) {
	rows, err := e.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// LoadFrom 灌入外部数据到一张临时表(DuckDB 跨源查询)。
//
// dapr cube app 拿到思迅原始数据 → 写临时表 → DuckDB 内做预聚合 → 返回。
// TODO:P2 改用 DuckDB Appender API(性能更好)。
//
// cols 接收带类型的 FieldDef 列表,LoadFrom 根据 Type 生成正确的 DuckDB 列类型,
// 避免 SUM(quantity) 这种聚合在 VARCHAR 列上失败。
//
// 即使 rows 为空也会建表(列结构保留),这样下游 CREATE TABLE AS SELECT *
// 不会因为源表不存在而失败。
//
// 性能:用 transaction + prepared statement,10000 行 INSERT 从 30s+ 降到 ~1s。
func (e *Engine) LoadFrom(table string, cols []fieldmapping.FieldDef, rows [][]any) error {
	schema := ""
	for i, c := range cols {
		if i > 0 {
			schema += ", "
		}
		schema += quoteIdent(c.Target) + " " + duckdbType(c.Type)
	}
	if err := e.Exec(fmt.Sprintf("CREATE OR REPLACE TABLE %s (%s)", quoteIdent(table), schema)); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	colNames := ""
	placeholders := ""
	for i, c := range cols {
		if i > 0 {
			colNames += ", "
			placeholders += ", "
		}
		colNames += quoteIdent(c.Target)
		placeholders += "?"
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quoteIdent(table), colNames, placeholders)

	// 用事务 + prepared statement,比逐行 Exec 快 10-30x
	tx, err := e.db.Begin()
	if err != nil {
		return fmt.Errorf("duckdb: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return fmt.Errorf("duckdb: prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, row := range rows {
		if _, err := stmt.Exec(row...); err != nil {
			return fmt.Errorf("duckdb: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("duckdb: commit: %w", err)
	}
	return nil
}

// duckdbType 把 fieldmapping.FieldType 映射到 DuckDB 列类型。
func duckdbType(t fieldmapping.FieldType) string {
	switch t {
	case fieldmapping.TypeString:
		return "VARCHAR"
	case fieldmapping.TypeInt:
		return "BIGINT"
	case fieldmapping.TypeFloat, fieldmapping.TypeDecimal:
		return "DOUBLE"
	case fieldmapping.TypeBool:
		return "BOOLEAN"
	case fieldmapping.TypeDatetime:
		return "TIMESTAMP"
	default:
		return "VARCHAR"
	}
}

// BuildPreAgg 构建预聚合表(preagg.go 调用此方法)。
func (e *Engine) BuildPreAgg(name, sql string) error {
	return e.Exec("CREATE OR REPLACE TABLE " + quoteIdent(name) + " AS " + sql)
}

// RefreshPreAgg 刷新预聚合表(P2 实现增量)。
func (e *Engine) RefreshPreAgg(name string) error {
	// TODO: 配合 preagg.go 的 RefreshSQL 字段
	_ = name
	return errors.New("duckdb: RefreshPreAgg not implemented yet (P2)")
}

// TableExists 检查表是否存在(用于启动时判断要不要 Build)。
func (e *Engine) TableExists(name string) (bool, error) {
	rows, err := e.Query(
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_name = ?", name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, nil
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- 内部 ----

func quoteIdent(s string) string {
	return `"` + s + `"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}