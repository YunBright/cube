// Command inspect 是开发工具:连接 SQL Server,输出真实表名 + 字段名 + 样本数据。
//
// 用法(用户本地):
//
//	cd semantic-layers/sixun
//	go run ./cmd/inspect ./cmd/sixun-hbposv7/config.yaml
//
// 输出格式:
//   === ALL TABLES ===
//     table1
//     table2
//   ...
//   === TABLE supplier (sixun7_supplier) ===
//     cups            nvarchar
//     cups_name       nvarchar
//     ...
//     --- samples ---
//     row 1: map[cups:SUP001 cups_name:可口可乐 ...]
//
// 拿到输出后,把字段名贴给 AI,AI 据此写 mapping.yaml。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "github.com/microsoft/go-mssqldb"
	"gopkg.in/yaml.v3"
)

// config 是 inspect 关心的最小 config 子集。
type config struct {
	Source struct {
		DSN           string `yaml:"dsn"`
		Version       string `yaml:"version"`
		TableSupplier string `yaml:"table_supplier"`
		TableProduct  string `yaml:"table_product"`
		TableCategory string `yaml:"table_category"`
		TableSale     string `yaml:"table_sale"`
		TableStock    string `yaml:"table_stock"`
	} `yaml:"source"`
}

type columnInfo struct {
	Name string
	Type string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inspect <config.yaml>")
		os.Exit(2)
	}
	cfgPath := os.Args[1]
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	if cfg.Source.DSN == "" {
		fmt.Fprintln(os.Stderr, "config.source.dsn is empty")
		os.Exit(1)
	}

	db, err := sql.Open("sqlserver", cfg.Source.DSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fmt.Fprintln(os.Stderr, "ping (DSN wrong?):", err)
		os.Exit(1)
	}

	fmt.Printf("=== connected (version=%s) ===\n\n", cfg.Source.Version)

	// 1. 列所有表
	fmt.Println("=== ALL TABLES ===")
	tables, err := listTables(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list tables:", err)
		os.Exit(1)
	}
	for _, t := range tables {
		fmt.Println("  ", t)
	}

	// 2. 对每个目标表,显示字段 + 样本
	targets := []struct {
		label string
		table string
	}{
		{"supplier", cfg.Source.TableSupplier},
		{"product", cfg.Source.TableProduct},
		{"category", cfg.Source.TableCategory},
		{"sale_detail", cfg.Source.TableSale},
		{"stock", cfg.Source.TableStock},
	}

	for _, tgt := range targets {
		if tgt.table == "" {
			continue
		}
		fmt.Printf("\n=== TABLE %s (%s) ===\n", tgt.label, tgt.table)
		if !contains(tables, tgt.table) {
			fmt.Printf("  WARN: table %q NOT FOUND in DB\n", tgt.table)
			continue
		}
		cols, err := listColumns(db, tgt.table)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list columns %s: %v\n", tgt.table, err)
			continue
		}
		for _, c := range cols {
			fmt.Printf("  %-32s %s\n", c.Name, c.Type)
		}
		// 样本 3 行
		fmt.Println("  --- samples ---")
		samples, err := sampleRows(db, tgt.table, 3)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sample %s: %v\n", tgt.table, err)
			continue
		}
		for i, row := range samples {
			fmt.Printf("    row %d: %s\n", i+1, truncate(fmt.Sprintf("%v", row), 300))
		}
	}
}

func listTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
		SELECT TABLE_NAME FROM information_schema.tables
		WHERE TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

func listColumns(db *sql.DB, table string) ([]columnInfo, error) {
	rows, err := db.Query(`
		SELECT COLUMN_NAME, DATA_TYPE
		FROM information_schema.columns
		WHERE TABLE_NAME = @p1
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []columnInfo
	for rows.Next() {
		var c columnInfo
		if err := rows.Scan(&c.Name, &c.Type); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func sampleRows(db *sql.DB, table string, n int) ([]map[string]any, error) {
	// SQL Server:SELECT TOP n * ...
	q := fmt.Sprintf("SELECT TOP %d * FROM %s", n, table)
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
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
	return out, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}