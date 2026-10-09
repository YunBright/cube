package source_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"
)

// 结算审批场景(2026-10-09)新增了 5 个共享 model。这套 model 的 schema 是
// **family 级共用**的(sixun-models/),列名与类型靠各 family 的 mapping.yaml 对齐。
//
// 历史上栽过的两次跟头都指向同一个根因:
//   - hbposv7 那份残缺 queryHandler 静默丢弃 filters,返回错误数据(已修)
//   - 共享 schema 加了字段却只改一个 family 的 mapping,另一边查询直接 column not found
//
// 所以这里锁两条**真实约束**,不是锁当前实现:
//  1. 两个 family 对同一 model 的 target 集合必须完全一致
//     —— 不一致时,共用的 schema 必然有一边查不了
//  2. schema 里每条 measure 引用的列,必须在 mapping 的 target 里
//     —— mapping 是白名单(AGENTS.md §4 / playbook 铁律 #2),漏映射只在查询那一刻报错
func repoPaths(t *testing.T) (ysxMapping, hbMapping, modelsDir string) {
	t.Helper()
	ysxMapping = filepath.Join("..", "..", "cmd", "sixun-ysx", "mapping")
	hbMapping = filepath.Join("..", "..", "cmd", "sixun-hbposv7", "mapping")
	modelsDir = filepath.Join("..", "..", "..", "..", "sixun-models")
	for _, p := range []string{ysxMapping, hbMapping, modelsDir} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("路径不存在 %s: %v(测试依赖仓库内相对路径)", p, err)
		}
	}
	return ysxMapping, hbMapping, modelsDir
}

func loadLoader(t *testing.T, dir string) *fieldmapping.Loader {
	t.Helper()
	l, err := fieldmapping.NewLoader(dir)
	if err != nil {
		t.Fatalf("load mapping %s: %v", dir, err)
	}
	return l
}

// targetsOf 返回 model 的 target 列表(排序后);model 不存在时返回 nil。
func targetsOf(l *fieldmapping.Loader, model string) []string {
	m, ok := l.Get(model)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m.TargetsWithType()))
	for _, f := range m.TargetsWithType() {
		out = append(out, f.Target)
	}
	sort.Strings(out)
	return out
}

// TestMappingTargetsAreIdenticalAcrossFamilies 是本次改动的核心回归锁。
func TestMappingTargetsAreIdenticalAcrossFamilies(t *testing.T) {
	ysxDir, hbDir, _ := repoPaths(t)
	ysxL, hbL := loadLoader(t, ysxDir), loadLoader(t, hbDir)

	models := map[string]bool{}
	for _, n := range ysxL.Models() {
		models[n] = true
	}
	for _, n := range hbL.Models() {
		models[n] = true
	}
	if len(models) == 0 {
		t.Fatal("没读到任何 model,检查 mapping 目录路径")
	}

	for _, model := range sortedKeys(models) {
		ysxT := targetsOf(ysxL, model)
		hbT := targetsOf(hbL, model)
		switch {
		case ysxT == nil:
			t.Errorf("model %q 只有 hbposv7 有 mapping,共享 schema 下 ysx 会 column not found", model)
		case hbT == nil:
			t.Errorf("model %q 只有 ysx 有 mapping,共享 schema 下 hbposv7 会 column not found", model)
		case strings.Join(ysxT, ",") != strings.Join(hbT, ","):
			t.Errorf("model %q 两 family target 不一致:\n  ysx     = %v\n  hbposv7 = %v", model, ysxT, hbT)
		}
	}
}

// TestSchemaMeasureColumnsAreMapped 锁"measure 引用的列必须在 mapping 白名单里"。
func TestSchemaMeasureColumnsAreMapped(t *testing.T) {
	ysxDir, hbDir, modelsDir := repoPaths(t)
	ysxL, hbL := loadLoader(t, ysxDir), loadLoader(t, hbDir)

	identRE := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	// 先剥掉单引号字符串字面量,否则 'R' / 'S' 这类枚举值会被当成列名误报。
	stringLitRE := regexp.MustCompile(`'[^']*'`)
	sqlKeywords := map[string]bool{
		"SELECT": true, "FROM": true, "WHERE": true, "SUM": true, "AVG": true,
		"COUNT": true, "MIN": true, "MAX": true, "CASE": true, "WHEN": true,
		"THEN": true, "ELSE": true, "END": true, "AND": true, "OR": true,
		"IN": true, "AS": true, "NULL": true, "LEFT": true, "RTRIM": true,
	}

	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		t.Fatalf("read models dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		schemaPath := filepath.Join(modelsDir, e.Name(), "schema.yaml")
		raw, err := os.ReadFile(schemaPath)
		if err != nil {
			continue
		}
		model, err := cubeschema.Load(raw)
		if err != nil {
			t.Errorf("schema %s 解析失败: %v", schemaPath, err)
			continue
		}
		targets := map[string]bool{}
		for _, tg := range targetsOf(ysxL, model.Name) {
			targets[tg] = true
		}
		for _, tg := range targetsOf(hbL, model.Name) {
			targets[tg] = true
		}
		if len(targets) == 0 {
			t.Errorf("model %q 有 schema 但两个 family 都没有 mapping", model.Name)
			continue
		}
		for _, ms := range model.Measures {
			sql := stringLitRE.ReplaceAllString(ms.SQL, "''")
			for _, ident := range identRE.FindAllString(sql, -1) {
				if sqlKeywords[strings.ToUpper(ident)] || targets[ident] {
					continue
				}
				t.Errorf("model %q 的 measure %q 引用了未映射的列 %q(只在查询那一刻报 column not found)",
					model.Name, ms.Name, ident)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("一个 schema.yaml 都没校验到,检查 models dir 路径")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
