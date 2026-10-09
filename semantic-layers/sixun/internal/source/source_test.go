package source

import "testing"

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
	if got := rl.For(RoleStock); got != 10000 {
		t.Fatalf("stock = %d, want default 10000", got)
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
		{"hbposv7 t_bd_item_info    (商品)", 44313},
		{"hbposv7 t_im_branch_stock (库存)", 10607},
		{"hbposv7 t_bd_supcust_info (供应商)", 300},
		{"hbposv7 t_bd_item_cls     (分类)", 596},
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
		RoleProduct: bigTable * 4,
		RoleStock:   bigTable * 4,
		RoleSale:    100000, // 流水表才是该单独限流的那个
	})
	for _, role := range []string{RoleProduct, RoleStock} {
		if got := rl.For(role); got < bigTable {
			t.Fatalf("%s 上限 %d < 维表实测 %d,仍会截断", role, got, bigTable)
		}
	}
	if got := rl.For(RoleSale); got != 100000 {
		t.Fatalf("sale 应能单独限流, got %d", got)
	}
}