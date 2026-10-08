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

// 回归锁:连接器曾经把 "SELECT TOP 10000 *" 硬编码,导致 hbposv10 的 t_bd_item_info
// (实测 27299 行)被静默截断 63%,商品扫码查不到且无任何报错。
// 这里锁住"默认上限远小于真实商品表行数"这一事实,提醒默认值不是安全网。
func TestDefaultRowLimitIsBelowRealProductTableSize(t *testing.T) {
	const measuredItemRows = 27299 // hbposv10 t_bd_item_info 实测 COUNT(*)
	if DefaultRowLimit >= measuredItemRows {
		t.Fatalf("DefaultRowLimit %d >= measured product rows %d:硬截断会再次丢商品",
			DefaultRowLimit, measuredItemRows)
	}
}