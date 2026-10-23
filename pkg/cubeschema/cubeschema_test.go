package cubeschema

import (
	"strings"
	"testing"
)

// liveSchemaYAML 是一份最小的、**合法**的 live model。
//
// 形态照抄真实 schema.yaml 的必要字段:name / sql_table / storage / dimensions。
const liveSchemaYAML = `
name: product
sql_table: product
storage: live
dimensions:
  - name: id
    sql: id
    type: string
measures:
  - name: count
    sql: COUNT(*)
    type: number
`

// TestLoad_AcceptsLive 是最基本的一条:storage: live 必须加载成功。
func TestLoad_AcceptsLive(t *testing.T) {
	m, err := Load([]byte(liveSchemaYAML))
	if err != nil {
		t.Fatalf("storage: live 必须加载成功,got %v", err)
	}
	if m.Storage != StorageLive {
		t.Errorf("Storage = %q, want %q", m.Storage, StorageLive)
	}
}

// TestLoad_RejectsDuckAtLoadTime 是本次去本地副本留下的**核心回归锁**。
//
// # 为什么这条必须存在
//
// 2026-10-23 之前:`storage` 没写 ⇒ 默认按 duck 处理 ⇒ schema **静默加载成功**;
// 一直到**查询那一刻**才炸("model X is not a live model")——
// 报错点离真正的原因(有人写了 `storage: duck`)隔了
// "加载 → 注册 → 编译 → 打源库"整条链路,排查成本极高;
// 而且症状是"某个 model 查不了",很容易先怀疑数据、怀疑权限、怀疑源库,
// 没人会想到去看 schema 里 storage 那一行。
//
// ⇒ 现在必须在**加载期**就拒掉,并说清楚"为什么只有 live"。
func TestLoad_RejectsDuckAtLoadTime(t *testing.T) {
	duck := strings.Replace(liveSchemaYAML, "storage: live", "storage: duck", 1)
	_, err := Load([]byte(duck))
	if err == nil {
		t.Fatal("storage: duck 必须在**加载期**被拒绝 —— 它以前会静默通过," +
			"直到查询那一刻才炸(报错点离原因隔了整条链路)")
	}
	msg := err.Error()
	for _, want := range []string{"product", "duck", "live"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里应出现 %q,实际: %s", want, msg)
		}
	}
}

// TestLoad_RejectsMissingStorage 锁"漏写 storage"也被拒。
//
// 漏写比写错更常见,而它以前是**零症状**的(默认 duck ⇒ 静默通过)。
func TestLoad_RejectsMissingStorage(t *testing.T) {
	missing := strings.Replace(liveSchemaYAML, "storage: live\n", "", 1)
	_, err := Load([]byte(missing))
	if err == nil {
		t.Fatal("漏写 storage 必须在加载期被拒 —— 以前默认 duck ⇒ 静默加载成功")
	}
	if !strings.Contains(err.Error(), "live") {
		t.Errorf("错误信息应说明唯一合法值是 live,实际: %s", err.Error())
	}
}

// TestLoad_RejectsUnknownStorage 锁住"别的值也拒"。
//
// ⚠️ 注意 YAML 语义:`storage: live `(**不带引号**、尾随空格)会被 YAML 自己
// 去掉尾随空格 ⇒ 它就是 `live`,**应该**通过。
// 真正要拒的是**值本身**不等于 "live" 的写法。
func TestLoad_RejectsUnknownStorage(t *testing.T) {
	for _, bad := range []string{"LIVE", "postgres", "duckdb", `"live "`, `" live"`} {
		badSchema := strings.Replace(liveSchemaYAML, "storage: live", "storage: "+bad, 1)
		if _, err := Load([]byte(badSchema)); err == nil {
			t.Errorf("storage: %s 必须被拒绝", bad)
		}
	}
	// YAML 尾随空格(不带引号)由 YAML 规范化成 "live" ⇒ 应当通过。
	trailing := strings.Replace(liveSchemaYAML, "storage: live", "storage: live   ", 1)
	if _, err := Load([]byte(trailing)); err != nil {
		t.Errorf("storage: live(尾随空格,不带引号)应被 YAML 规范化成 live 并通过,got %v", err)
	}
}

// TestLoad_StorageDuckConstantIsGone 说明"StorageDuck 常量必须不存在"。
//
// 这里**不能**直接写 `cubeschema.StorageDuck` 来断言 —— 那会编译失败。
// 事实上这正是最强的保证:**常量删了以后,任何写
// `Storage: cubeschema.StorageDuck` 的代码都编译不过**,
// 于是"duck 是一个合法取值"这件事在类型层面就不成立了。
// 剩下的唯一入口是 YAML 里的字面量 `storage: duck`,由上面的
// TestLoad_RejectsDuckAtLoadTime 锁住。

// TestLoad_PrimaryKeyStillValidated 锁住"primary_key 校验还在"。
//
// primary_key 现在**没有读取方**(增量 upsert 已删),但字段保留
// (已有 7 个 schema.yaml 声明了 `primary_key: id`,删字段会让它们
// 变成被 yaml 静默忽略的僵尸配置)。
// ⇒ 保留字段就必须保留校验:写错维度名照样当场报错。
func TestLoad_PrimaryKeyStillValidated(t *testing.T) {
	bad := liveSchemaYAML + "\nprimary_key: not_a_dimension\n"
	if _, err := Load([]byte(bad)); err == nil {
		t.Fatal("primary_key 指向不存在的 dimension 时必须报错(字段虽已无读取方,但校验要留)")
	}
	ok := liveSchemaYAML + "\nprimary_key: id\n"
	if _, err := Load([]byte(ok)); err != nil {
		t.Fatalf("primary_key: id 必须能加载,got %v", err)
	}
}
