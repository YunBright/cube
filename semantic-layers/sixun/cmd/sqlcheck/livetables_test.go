// livetables_test.go 锁"live model 的源表名有没有真的接上"。
//
// 为什么需要这条:一个 storage: live 的 model 少配一条 source.table_*,
// **不会有任何报错**,只会:
//
//	cmd/sixun-<family>/main.go:232   打一行 Info 日志,然后 continue
//	semantic-layers/sixun/cmd/sqlcheck/main.go:548  打一行 WARN,然后 continue
//
// 该 model 从此"不可查",而 healthz 照样绿、sqlcheck 照样打印 "N passed"。
// 症状是"按类别出报表查不到数据",而没有任何一层会红。
//
// ⚠️ 这套映射有**三份拷贝**(两个 family 的 main.go + sqlcheck),
//
//	三份必然漂移。本文件把三份与 schema 声明的 storage 一起锁住 ———
//	判据是"集合相等",不是"包含":包含会让多出来的陈旧条目永远合法,
//	而陈旧条目正是下一次改名时的错配源。
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"

	models "github.com/YunBright/cube/sixun-models"
)

// liveModelNames 返回所有 storage: live 的 model 名(来自 go:embed 的 schema)。
func liveModelNames(t *testing.T) []string {
	t.Helper()
	names, err := models.ModelNames()
	if err != nil {
		t.Fatalf("ModelNames: %v", err)
	}
	var live []string
	for _, n := range names {
		b, err := models.ReadSchema(n)
		if err != nil {
			t.Fatalf("ReadSchema(%s): %v", n, err)
		}
		m, err := cubeschema.Load(b)
		if err != nil {
			t.Fatalf("Load(%s): %v", n, err)
		}
		if m.EffectiveStorage() == cubeschema.StorageLive {
			live = append(live, n)
		}
	}
	if len(live) == 0 {
		t.Fatal("没有任何 storage: live 的 model —— 是 embed 或判据坏了,不是配置坏了")
	}
	return live
}

// mapKeyRe 抓 liveTables / srcTables 这种 map 字面量里的 `"<model>":` 键。
var mapKeyRe = regexp.MustCompile(`^\s*"([a-z0-9_]+)":`)

// keysFromGoSource 扫 Go 源码里指定函数的 map 字面量键。
func keysFromGoSource(t *testing.T, path, funcName string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	start := strings.Index(src, "func "+funcName+"(")
	if start < 0 {
		t.Fatalf("%s 里找不到 func %s", path, funcName)
	}
	// 函数体到下一个顶层 "func " 为止。
	body := src[start:]
	if next := strings.Index(body[1:], "\nfunc "); next >= 0 {
		body = body[:next+1]
	}
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if m := mapKeyRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s 的 %s 里没扫到任何 map 键 —— 解析器坏了,不是真的空", path, funcName)
	}
	return out
}

func equalSets(t *testing.T, a, b map[string]bool, whatA, whatB string) {
	t.Helper()
	for k := range a {
		if !b[k] {
			t.Errorf("%s 有 %q 而 %s 没有 —— 少接一条,该 model 会静默不可查", whatA, k, whatB)
		}
	}
	for k := range b {
		if !a[k] {
			t.Errorf("%s 有 %q 而 %s 没有 —— 多出来的是陈旧条目,下次改名就会错配", whatB, k, whatA)
		}
	}
}

// TestLiveModelsHaveSourceTableEverywhere 是本文件的主体。
func TestLiveModelsHaveSourceTableEverywhere(t *testing.T) {
	live := liveModelNames(t)
	want := map[string]bool{}
	for _, n := range live {
		want[n] = true
	}

	// ---- 1. 每个 live model 都有 mapping 文件(两个 family 各一份)----
	//
	// mapping 缺了不会编译失败:main.go 只打一行 "mapping missing" 就跳过。
	// 而 schema 的成员名与 mapping 的 canonical 名必须逐字一致,
	// 所以两份 mapping 不能互相复用 —— 这里只检查文件存在与 model 名对齐。
	for _, fam := range []string{"sixun-hbposv7", "sixun-ysx"} {
		for _, n := range live {
			p := filepath.Join("..", fam, "mapping", "mapping-"+n+".yaml")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Errorf("%s 缺 mapping-%s.yaml: %v", fam, n, err)
				continue
			}
			// mapping 自报的 model 名必须与文件名一致 —— 否则 loader.Get(name)
			// 在 main() 里返回 false,而报错只有一行 "mapping missing"。
			if !strings.Contains(string(b), "model: "+n+"\n") {
				t.Errorf("%s/mapping-%s.yaml 里的 model: 不是 %q", fam, n, n)
			}
		}
	}

	// ---- 2. 两个 family 的 liveTables() 键集合 == live model 集合 ----
	for _, fam := range []string{"sixun-hbposv7", "sixun-ysx"} {
		got := keysFromGoSource(t, filepath.Join("..", fam, "main.go"), "liveTables")
		equalSets(t, want, got, "schema 的 live model", fam+"/main.go liveTables()")
	}

	// ---- 3. sqlcheck 的 srcTables 键集合 == live model 集合 ----
	//
	// 这一条最容易漏:sqlcheck 是"上线前的真库验证工具",
	// 它少接一个 model 时照样打印 "N passed" —— 验证覆盖面少一块而**全绿**。
	gotCheck := keysFromGoSource(t, "main.go", "loadSchemas")
	equalSets(t, want, gotCheck, "schema 的 live model", "cmd/sqlcheck loadSchemas srcTables")
}

// TestLiveModelsHaveNonEmptyTableInConfig 检查两份 config.yaml 的表名都非空。
//
// 光有键不等于有值:table_sales_report: 留空与没写这一行,后果完全一样
// (main.go 里 `src == ""` 就跳过该 model)。
func TestLiveModelsHaveNonEmptyTableInConfig(t *testing.T) {
	live := liveModelNames(t)
	for _, fam := range []string{"sixun-hbposv7", "sixun-ysx"} {
		b, err := os.ReadFile(filepath.Join("..", fam, "config.yaml"))
		if err != nil {
			t.Fatalf("read %s/config.yaml: %v", fam, err)
		}
		nonEmpty := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			ln := strings.TrimSpace(line)
			if strings.HasPrefix(ln, "#") {
				continue // 注释里的示例不算配置
			}
			i := strings.Index(ln, ":")
			if i <= 0 || !strings.HasPrefix(ln, "table_") {
				continue
			}
			key := strings.TrimSpace(ln[:i])
			val := strings.TrimSpace(ln[i+1:])
			if idx := strings.Index(val, "#"); idx >= 0 {
				val = strings.TrimSpace(val[:idx])
			}
			if val == "" {
				continue // 有键但没给值 = 等于没配
			}
			nonEmpty[key] = true
		}
		for _, n := range live {
			key := "table_" + n
			if !nonEmpty[key] {
				t.Errorf("%s/config.yaml 的 %s 没有非空值 —— 该 model 会被静默跳过", fam, key)
			}
		}
	}
}

// TestItemClsAndParentShareOnePhysicalTable 锁住"父类别靠同一张表的第二个 model"。
//
// ⚠️ 这不是洁癖,是口径问题:item_cls 取的是**末级**类别,item_cls_parent 取的
//
//	是**父类别**。两者配成不同表名不会有任何报错,报表只会静默算成另一个层级。
//	实测按父类才对得上既有报表(生鲜联营 4994.70 / 百货联营 2340.70)。
func TestItemClsAndParentShareOnePhysicalTable(t *testing.T) {
	for _, fam := range []string{"sixun-hbposv7", "sixun-ysx"} {
		b, err := os.ReadFile(filepath.Join("..", fam, "config.yaml"))
		if err != nil {
			t.Fatalf("read %s/config.yaml: %v", fam, err)
		}
		tbl := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			ln := strings.TrimSpace(line)
			if strings.HasPrefix(ln, "#") || !strings.HasPrefix(ln, "table_item") {
				continue
			}
			i := strings.Index(ln, ":")
			if i <= 0 {
				continue
			}
			key := strings.TrimSpace(ln[:i])
			val := strings.TrimSpace(ln[i+1:])
			if idx := strings.Index(val, "#"); idx >= 0 {
				val = strings.TrimSpace(val[:idx])
			}
			tbl[key] = val
		}
		leaf, parent := tbl["table_item_cls"], tbl["table_item_cls_parent"]
		if leaf == "" || parent == "" {
			t.Errorf("%s/config.yaml 的 table_item_cls=%q / table_item_cls_parent=%q,有一个是空",
				fam, leaf, parent)
			continue
		}
		if leaf != parent {
			t.Errorf("%s/config.yaml:table_item_cls=%q 与 table_item_cls_parent=%q 必须相同 —— "+
				"它们是同一张 t_bd_item_cls 的两个 model,配成不同值不会报错,只会把类别口径换成另一个层级",
				fam, leaf, parent)
		}
	}
}

// TestSalesReportNeverMapsBranchNo 锁住"源库门店区域永不进模型"。
//
// branch 在思迅源库里是**门店区域**(X-Branch-ID 的下一层),与门店是两个
// 无关编码。业务 2026-10-15 明确要求忽略它。一旦它变成可分组维度,
// 用户选的是门店、报表却按区域拆行,两份"销售报表"对不上数而没人说得清哪份对。
func TestSalesReportNeverMapsBranchNo(t *testing.T) {
	for _, fam := range []string{"sixun-hbposv7", "sixun-ysx"} {
		for _, m := range []string{"sales_report", "item", "item_cls", "item_cls_parent"} {
			p := filepath.Join("..", fam, "mapping", "mapping-"+m+".yaml")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Errorf("%s 缺 %s: %v", fam, p, err)
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				ln := strings.TrimSpace(line)
				if strings.HasPrefix(ln, "#") {
					continue
				}
				if strings.Contains(ln, "branch_no") {
					t.Errorf("%s 把 branch_no 映射了:%q —— 门店区域不是门店,"+
						"按它过滤会让'今天卖了多少'少一块", p, ln)
				}
			}
		}
	}
}
