// Command sqlq 是数据源勘察工具:连思迅源库跑任意只读 SQL,打印列名与结果集。
//
// 与 cmd/inspect 的分工:
//   - cmd/inspect  按 config 里配好的几张表,打印结构 + 样本(建模时看表长什么样)
//   - cmd/sqlq     跑任意临时的只读查询(算行数、找候选列、验证金额口径)
//
// 用法:
//
//	# 单条语句
//	go run ./cmd/sqlq -config <config.yaml> -q "SELECT COUNT(*) FROM t_pm_sheet_master"
//
//	# 多条语句放文件,用单独一行 --@ 分隔
//	go run ./cmd/sqlq -config <config.yaml> -f probe.sql -n 50
//
//	# 源库只能从别的机器访问时(本机连不上),覆盖 DSN 的 host:port 走隧道
//	go run ./cmd/sqlq -config <config.yaml> -dsn-host-port 127.0.0.1:11433 -q "..."
//
// 参数:
//
//	-config         各 family 实例的 config.yaml,从它的 source.dsn 取连接串
//	-f              SQL 文件;一条 `--@` 独占一行分隔多条语句
//	-q              内联 SQL,优先于 -f
//	-dsn-host-port  覆盖 DSN 的 host:port(配合 ssh -L 隧道)
//	-n              每个结果集最多打印多少行(默认 50)
//	-cell           单个单元格最多打印多少字符(默认 100)
//
// 安全:DSN 只从 config 读进内存,**永不打印**(避免口令进日志与终端历史);
// 语句执行前做只读校验,出现写关键字直接拒绝 —— 源库是生产库,勘察阶段只允许读。
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
)

type config struct {
	Source struct {
		DSN     string `yaml:"dsn"`
		Version string `yaml:"version"`
	} `yaml:"source"`
}

// forbidden 挡住一切写操作 —— 源库是生产库,勘察阶段只能 SELECT。
var forbidden = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|alter|create|truncate|merge|exec|execute|grant|revoke|backup|restore)\b`)

var stmtSep = regexp.MustCompile(`(?m)^--@\s*$`)

func main() {
	cfgPath := flag.String("config", "",
		"配置目录或文件(目录则按 appcfg 规则选 config.local.yaml / config.yaml)")
	family := flag.String("family", "hbposv7", "ysx | hbposv7,仅在省略 -config 时用于定位配置目录")
	sqlFile := flag.String("f", "", "SQL 文件(多条语句用一行 --@ 分隔)")
	inline := flag.String("q", "", "内联 SQL,优先于 -f(单条语句最常用)")
	hostPort := flag.String("dsn-host-port", "", "覆盖 DSN 的 host:port(ssh 隧道用)")
	maxRows := flag.Int("n", 50, "每个结果集最多打印多少行")
	maxCell := flag.Int("cell", 100, "单个单元格最多打印多少字符")
	flag.Parse()

	if *sqlFile == "" && *inline == "" {
		fmt.Fprintln(os.Stderr, "需要 -f 或 -q 之一")
		os.Exit(2)
	}

	// -config 可以给目录(按 appcfg 规则选文件),也可以直接给文件。
	// 省略时按 -family 定位 cmd/sixun-<family>/,这样"本地验证"不需要粘路径。
	// 注意 cmd 目录名带 sixun- 前缀,family 只是后半段。
	cmdDir := map[string]string{
		"ysx":     "sixun-ysx",
		"hbposv7": "sixun-hbposv7",
	}[*family]
	if *cfgPath == "" {
		if cmdDir == "" {
			fail("unknown family %q (want ysx | hbposv7)", *family)
		}
		*cfgPath = appcfg.Resolve(filepath.Join("cmd", cmdDir))
	} else if st, err := os.Stat(*cfgPath); err == nil && st.IsDir() {
		*cfgPath = appcfg.Resolve(*cfgPath)
	}

	cfg, usedCfg, err := appcfg.LoadFrom[config](*cfgPath)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("=== config: %s ===\n", usedCfg)
	if cfg.Source.DSN == "" {
		fail("config.source.dsn is empty")
	}

	dsn := cfg.Source.DSN
	if *hostPort != "" {
		dsn = overrideHostPort(dsn, *hostPort)
	}

	sqlText := *inline
	if sqlText == "" {
		raw, err := os.ReadFile(*sqlFile)
		if err != nil {
			fail("read sql: %v", err)
		}
		sqlText = string(raw)
	}

	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		fail("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		fail("ping (DSN wrong / 网络不通?): %v", err)
	}
	fmt.Printf("=== connected: version=%s ===\n", cfg.Source.Version)

	stmts := stmtSep.Split(sqlText, -1)
	for i, s := range stmts {
		s = strings.TrimSpace(stripSQLComments(s))
		if s == "" {
			continue
		}
		if forbidden.MatchString(s) {
			fail("stmt #%d 含写操作关键字,拒绝执行(勘察阶段只允许 SELECT)", i+1)
		}
		fmt.Printf("\n########## stmt #%d ##########\n", i+1)
		runOne(db, s, *maxRows, *maxCell)
	}
}

func runOne(db *sql.DB, stmt string, maxRows, maxCell int) {
	rows, err := db.Query(stmt)
	if err != nil {
		fmt.Printf("!! ERROR: %v\n", err)
		return
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		fmt.Printf("!! cols: %v\n", err)
		return
	}
	fmt.Printf("--- columns: %s\n", strings.Join(cols, " | "))

	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fmt.Printf("!! scan: %v\n", err)
			return
		}
		cells := make([]string, len(cols))
		for i, v := range vals {
			cells[i] = clip(stringify(v), maxCell)
		}
		fmt.Printf("  [%d] %s\n", n+1, strings.Join(cells, " | "))
		n++
		if n >= maxRows {
			fmt.Printf("  ... (达到 -n %d 上限,未继续打印)\n", maxRows)
			break
		}
	}
	if err := rows.Err(); err != nil {
		fmt.Printf("!! rows: %v\n", err)
		return
	}
	if n == 0 {
		fmt.Println("  (0 行)")
	}
}

// overrideHostPort 把 DSN 里的 host:port 换掉,其余(user/password/query)原样保留。
// 做法:只在 '?' 之前的部分里找最后一个 '@',避免密码里的 '@' 干扰。
func overrideHostPort(dsn, newHostPort string) string {
	head, tail := dsn, ""
	if i := strings.Index(dsn, "?"); i >= 0 {
		head, tail = dsn[:i], dsn[i:]
	}
	at := strings.LastIndex(head, "@")
	if at < 0 {
		return dsn // 不是标准形态,别瞎改
	}
	return head[:at+1] + newHostPort + tail
}

// stripSQLComments 去掉整行 -- 注释,便于空语句判断与关键字扫描。
func stripSQLComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// stringify 把驱动返回值转成可读文本。
// go-mssqldb 对 numeric/decimal 返回 []byte,直接 fmt.Sprint 会打成
// "[53 54 52 46 ...]" 这种字节码 —— 金额全部不可读,必须显式转 string。
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	default:
		return fmt.Sprint(v)
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
