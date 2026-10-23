package embedcheck

import (
	"strings"
	"testing"
	"testing/fstest"

	models "github.com/YunBright/cube/sixun-models"
)

// allModelNames 返回内嵌 schema 的全部 model 名。
func allModelNames(t *testing.T) []string {
	t.Helper()
	names, err := models.ModelNames()
	if err != nil {
		t.Fatalf("models.ModelNames(): %v", err)
	}
	return names
}

// realModel 是内嵌 schema 里一定存在的 model(任取一个,用于构造 mapping 侧)。
// 断言只针对"mapping 侧的问题",所以不需要 schema 侧可控。
const realModel = "product"

// goodMapping 是一个合法 mapping:至少映射一个字段。
const goodMapping = `version: 1
model: ` + realModel + `
mappings:
  - source: item_no
    target: product_id
    type: string
`

// emptyMapping 是"文件在但没内容"的 mapping —— 语法合法、加载成功、零字段。
// 这是最阴的一种:查询能跑通,列名一个都没重写,结果是错的。
const emptyMapping = `version: 1
model: ` + realModel + `
mappings: []
`

// TestCheck_CatchesMissingMapping 锁住核心场景:model 有 schema 但没 mapping。
//
// 这条如果失效,症状就是生产上"某个 model 静默查不了"且启动零报错 ——
// 正是 embedcheck 存在的理由,所以必须证明它真的会红。
func TestCheck_CatchesMissingMapping(t *testing.T) {
	// 只给一个孤儿 mapping,product 本身没有 mapping。
	fsys := fstest.MapFS{
		"mapping-supplier.yaml": &fstest.MapFile{Data: []byte(strings.ReplaceAll(
			goodMapping, realModel, "supplier"))},
	}
	err := Check(fsys)
	if err == nil {
		t.Fatal("Check 应当因为 " + realModel + " 缺 mapping 而失败,但通过了 —— 守卫失效了")
	}
	if !strings.Contains(err.Error(), "没有 mapping") {
		t.Fatalf("错误信息应指出缺 mapping,实际: %v", err)
	}
}

// TestCheck_CatchesEmptyMapping 锁住"mapping 文件在但为空"。
func TestCheck_CatchesEmptyMapping(t *testing.T) {
	// 除 product 外的 model 全给上合法 mapping,product 给空的,
	// 这样错误只会来自"product 的 mapping 是空的"这一条。
	fsys := fstest.MapFS{"mapping-" + realModel + ".yaml": &fstest.MapFile{Data: []byte(emptyMapping)}}
	// 其余内嵌 model 给合法 mapping,避免被"缺 mapping"掩盖。
	fillValidMappings(t, fsys, realModel)

	err := Check(fsys)
	if err == nil {
		t.Fatal("Check 应当因为空 mapping 而失败,但通过了 —— 守卫失效了")
	}
	if !strings.Contains(err.Error(), "mapping 是空的") {
		t.Fatalf("错误信息应指出空 mapping,实际: %v", err)
	}
}

// TestCheck_CatchesOrphanMapping 锁住反向:拼错 model 名的 mapping 没人认领。
func TestCheck_CatchesOrphanMapping(t *testing.T) {
	fsys := fstest.MapFS{}
	fillValidMappings(t, fsys, "")
	fsys["mapping-typo_prodcut.yaml"] = &fstest.MapFile{Data: []byte(goodMapping)}

	err := Check(fsys)
	if err == nil {
		t.Fatal("Check 应当因为孤儿 mapping 而失败,但通过了 —— 守卫失效了")
	}
	if !strings.Contains(err.Error(), "没有对应 schema") {
		t.Fatalf("错误信息应指出孤儿 mapping,实际: %v", err)
	}
}

// fillValidMappings 给除 skip 外的所有内嵌 model 补一份合法 mapping。
func fillValidMappings(t *testing.T, fsys fstest.MapFS, skip string) {
	t.Helper()
	for _, m := range allModelNames(t) {
		if m == skip {
			continue
		}
		body := strings.ReplaceAll(goodMapping, realModel, m)
		fsys["mapping-"+m+".yaml"] = &fstest.MapFile{Data: []byte(body)}
	}
}

// schemaWithStorage 造一份"只差 storage"的最小 schema。
//
// 用来证明 **storage 契约这道门真的会红** ——
// 2026-10-23 之前没有这道门:`storage: duck` 的 schema **静默加载成功**,
// 直到查询那一刻才炸 "model X is not a live model"。
func schemaWithStorage(storage string) []byte {
	return []byte("name: demo\nsql_table: demo\n" + storage +
		"dimensions:\n  - name: id\n    sql: id\n    type: string\n")
}

// TestCheckSchema_RejectsNonLiveStorage 锁住"非 live 的 storage 被抓住"。
func TestCheckSchema_RejectsNonLiveStorage(t *testing.T) {
	for _, storage := range []string{"storage: duck\n", "storage: postgres\n", ""} {
		problems := checkSchema("demo", schemaWithStorage(storage))
		if len(problems) == 0 {
			t.Fatalf("storage=%q 必须被抓住(否则它会静默加载成功,直到查询才炸)", storage)
		}
		if !strings.Contains(problems[0], "demo") {
			t.Errorf("错误信息应指出是哪个 model,实际: %s", problems[0])
		}
	}
}

// TestCheckSchema_AcceptsLiveStorage 锁住"live 通过"。
func TestCheckSchema_AcceptsLiveStorage(t *testing.T) {
	if problems := checkSchema("demo", schemaWithStorage("storage: live\n")); len(problems) != 0 {
		t.Fatalf("storage: live 必须通过,实际: %v", problems)
	}
}

// TestCheck_AllEmbeddedSchemasAreLive 锁住"内嵌的 24 个 schema 全部是 live 且能解析"。
//
// 这条是**永远会跑**的那道门(两个 family 的 embed_test.go 各跑一次)——
// 不像 config_test.go 那样"机器上没有 config.yaml 就 skip"。
func TestCheck_AllEmbeddedSchemasAreLive(t *testing.T) {
	names := allModelNames(t)
	if len(names) == 0 {
		t.Fatal("一个内嵌 schema 都没有")
	}
	for _, name := range names {
		b, err := models.ReadSchema(name)
		if err != nil {
			t.Errorf("读 schema %q 失败: %v", name, err)
			continue
		}
		if problems := checkSchema(name, b); len(problems) > 0 {
			t.Errorf("内嵌 schema 不合规: %v", problems)
		}
	}
}
