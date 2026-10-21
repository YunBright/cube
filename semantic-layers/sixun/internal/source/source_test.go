package source

import (
	"strings"
	"testing"
)

func TestRowLimitsFallback(t *testing.T) {
	// nil receiver 必须也能给出可用的上限,否则 connector 漏配 RowLimits 会直接
	// 拼出 "SELECT TOP 0"(或 panic),把配置错误伪装成"源库没有数据"。
	var nilLimits *RowLimits
	if got := nilLimits.For(RoleProduct); got != DefaultRowLimit {
		t.Fatalf("nil RowLimits.For(product) = %d, want DefaultRowLimit %d", got, DefaultRowLimit)
	}

	// 未配置角色 → 走默认上限
	rl := NewRowLimits(0, nil)
	if got := rl.For(RoleProduct); got != DefaultRowLimit {
		t.Fatalf("unconfigured role = %d, want %d", got, DefaultRowLimit)
	}

	// defaultLimit <= 0 → 回落到 DefaultRowLimit
	if got := NewRowLimits(-1, nil).For(RoleProduct); got != DefaultRowLimit {
		t.Fatalf("negative default = %d, want %d", got, DefaultRowLimit)
	}
}

func TestRowLimitsPerRoleOverride(t *testing.T) {
	rl := NewRowLimits(10000, map[string]int{RoleProduct: 200000})

	if got := rl.For(RoleProduct); got != 200000 {
		t.Fatalf("product override = %d, want 200000", got)
	}
	// 覆盖只影响指定角色,其余角色保持默认
	if got := rl.For(RoleCategory); got != 10000 {
		t.Fatalf("category = %d, want default 10000", got)
	}
	if got := rl.For(RoleSupplier); got != 10000 {
		t.Fatalf("supplier = %d, want default 10000", got)
	}
}

// 非正值覆盖必须被忽略:config.yaml 里写 row_limits: {product: 0} 是常见笔误,
// 若直接采信会退化成 SELECT TOP 0,源库明明有数据却拉回空集。
func TestRowLimitsRejectsNonPositiveOverride(t *testing.T) {
	rl := NewRowLimits(10000, map[string]int{RoleProduct: 0, RoleSale: -5})
	if got := rl.For(RoleProduct); got != 10000 {
		t.Fatalf("zero override = %d, want default 10000", got)
	}
	if got := rl.For(RoleSale); got != 10000 {
		t.Fatalf("negative override = %d, want default 10000", got)
	}
}

// 回归锁:连接器曾把 "SELECT TOP 10000 *" 硬编码,导致商品表被静默截断
// (ysx 丢 63% / hbposv7 丢 77%),**库存表也丢 57%** —— 盘点账面数量直接是错的,
// 而服务 / sidecar / 健康检查全绿,没有任何报错。
//
// 这条锁的方向:**默认值必须高于所有维表的真实行数**。
// 早先它断言的是反方向(默认 < 真实行数,用来"提醒默认值不安全"),
// 那是把已知缺陷钉死成了规格;真正的修法是把默认值抬上去,
// 于是 2026-10-09 把它翻了过来 —— 数据被截断时必须有人能看见,
// 而静默截断比不过载危险得多:过载会报错,截断只会让大半商品凭空消失。
//
// 实测值来自 2026-10-09 对两个源库 COUNT(*) 的直接查询。
func TestDefaultRowLimitCoversRealTableSizes(t *testing.T) {
	cases := []struct {
		table string
		rows  int
	}{
		{"ysx  t_bd_item_info    (商品)", 27299},
		{"ysx  t_im_branch_stock (库存)", 23576},
		{"ysx  t_bd_supcust_info (供应商)", 218},
		{"ysx  t_bd_item_cls     (分类)", 186},
		{"ysx  t_rm_vip_info     (会员)", 4128},
		{"ysx  t_rm_vip_type     (会员类别)", 3},
		{"hbposv7 t_bd_item_info    (商品)", 44313},
		{"hbposv7 t_im_branch_stock (库存)", 10607},
		{"hbposv7 t_bd_supcust_info (供应商)", 300},
		{"hbposv7 t_bd_item_cls     (分类)", 596},
		{"hbposv7 t_rm_vip_info     (会员)", 2642},
		{"hbposv7 t_rm_vip_type     (会员类别)", 3},
	}
	for _, c := range cases {
		if DefaultRowLimit < c.rows {
			t.Fatalf("DefaultRowLimit %d < 实测行数 %d (%s):会静默截断维表,"+
				"表现为条码扫不出来且零报错", DefaultRowLimit, c.rows, c.table)
		}
	}
}

// RowLimits 必须让每个角色都能单独放宽 —— 这是"截断只能是显式决定"的落地方式:
// 想限流某个表就单独配,而不是让所有表共用一个可能不够用的数。
func TestRowLimitsCanRelaxEachRoleIndependently(t *testing.T) {
	const bigTable = 44313
	rl := NewRowLimits(DefaultRowLimit, map[string]int{
		RoleProduct:  bigTable * 4,
		RoleCategory: bigTable * 4,
		RoleSale:     100000, // 流水表才是该单独限流的那个
	})
	for _, role := range []string{RoleProduct, RoleCategory} {
		if got := rl.For(role); got < bigTable {
			t.Fatalf("%s 上限 %d < 维表实测 %d,仍会截断", role, got, bigTable)
		}
	}
	if got := rl.For(RoleSale); got != 100000 {
		t.Fatalf("sale 应能单独限流, got %d", got)
	}
}

// FilterRowsByColumn 必须只留"等于目标值"的行 —— 而**不是**"非空的行"。
//
// 背景:t_bd_base_code 是通用基础代码字典,同一张表里 code_id 会跨 type_no
// 重复('1' 在职称/民族/学历里都有)。整张表灌进 sale_way model 的话,
// primary_key 撞车 ⇒ 某些行被静默吞掉,code_id→code_name 的对应关系是错的。
//
// ⇒ 过滤必须精确到 type_no='SW'。这条用例盯的就是"精确",不是"大概留了点"。
func TestFilterRowsByColumnKeepsOnlyExactMatches(t *testing.T) {
	rows := []map[string]any{
		{"type_no": "SW", "code_id": "A", "code_name": "购销"},
		{"type_no": "SW", "code_id": "C", "code_name": "联营"},
		{"type_no": "40", "code_id": "1", "code_name": "经理"},
		{"type_no": "CT", "code_id": "1", "code_name": "国有"},
		{"type_no": "SW ", "code_id": "F", "code_name": "扣率代销"},
		{"type_no": nil, "code_id": "Z", "code_name": "空 type_no"},
		{"code_id": "Y"}, // 连 type_no 这一列都没有
	}

	got := FilterRowsByColumn(rows, "type_no", "SW")
	// 3 条 SW(含 'SW ' 尾随空格)+ 2 条非 SW。
	// ⚠️ type_no 为 NULL / 缺列的**不保留**:"type_no 为空"不是 SW。
	if len(got) != 3 {
		t.Fatalf("过滤后行数 = %d,want 3 —— %v", len(got), got)
	}
	for _, r := range got {
		tn, _ := r["type_no"].(string)
		if strings.TrimSpace(tn) != "SW" {
			t.Errorf("留下了一行 type_no=%q,只有精确等于 SW 的行能留下", tn)
		}
	}
}

// 大小写与空格不该改变结果:思迅的 type_no 是 CHAR 定长(会带尾随空格),
// 而字典里也可能写成 'sw'。两边都归一化,免得"看起来有值却查不到"。
func TestFilterRowsByColumnIgnoresCaseAndSpace(t *testing.T) {
	rows := []map[string]any{
		{"type_no": "sw", "code_id": "A"},
		{"type_no": " SW", "code_id": "B"},
		{"type_no": "Sw ", "code_id": "C"},
		{"type_no": "SWX", "code_id": "D"},
	}
	got := FilterRowsByColumn(rows, "type_no", "SW")
	if len(got) != 3 {
		t.Fatalf("过滤后行数 = %d,want 3 —— %v", len(got), got)
	}
	for _, r := range got {
		if r["code_id"] == "D" {
			t.Errorf("'SWX' 不该被当成 'SW'(前缀匹配会把别的类型也拖进来)")
		}
	}
}

// 列缺失或为 NULL 的行**不保留**:过滤条件是"等于某个值"。
//
// "type_no 为空的行"不是 SW 行,留着会在字典表里多出没有名字的条目。
func TestFilterRowsByColumnDropsRowsWithMissingColumn(t *testing.T) {
	rows := []map[string]any{
		{"type_no": "SW", "code_id": "A"},
		{"type_no": nil, "code_id": "B"},
		{"code_id": "C"},
	}
	got := FilterRowsByColumn(rows, "type_no", "SW")
	if len(got) != 1 {
		t.Fatalf("过滤后行数 = %d,want 1(空值行要丢掉)—— %v", len(got), got)
	}
}

// RequireRows:过滤后为空必须**报错**,不能返回一个空字典。
//
// 症状对照:空字典表 → App 里所有供应商都落进"未分类" →
// 看起来像"数据没维护",真因却是取数逻辑写错了,排查会被带偏很久。
func TestRequireRowsRejectsEmptyResult(t *testing.T) {
	if _, err := RequireRows("sale_way", nil); err == nil {
		t.Fatal("空结果必须报错 —— 静默的空字典会被当成'数据没维护'")
	}
	rows := []map[string]any{{"a": 1}}
	got, err := RequireRows("sale_way", rows)
	if err != nil {
		t.Fatalf("非空结果不该报错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("返回值行数 = %d,want 1", len(got))
	}
}
