package source

import (
	"fmt"
	"strings"
)

// SaleDetailTables 是 sale_detail 模型的取数参数(**family 私有事实**)。
//
// ⚠️ 为什么逐笔流水要片段而不是普通表名
//
//	这张表的"退货"**不是独立行**,而是**同一行里的一列**(ysx 的 ret_qnty):
//	实测 ysx 3,327,514 行里只有 1,939 行 ret_qnty ≠ 0,且该值恒等于同行的 sale_qnty
//	(卖 2.545 件、其中 2.545 件被退)。
//	⇒ 行本身区分不出"销售行/退货行" ⇒ **净额只能由列算出来**。
//
// ⚠️ 而 ret_qnty 是 **ysx 独有**的列(hbposv7 的 t_rm_saleflow 没有这一列,
// 实测 2026-10-23)⇒ 它**不能进共享 schema**(两个 family 的 mapping target 集合
// 必须逐字一致,否则 mapping parity 测试直接红)。
//
// ⇒ 于是"净额"这件事只能放在 **family 私有的片段**里:
//	ysx       : 净额 = (sale_qnty − ret_qnty) × sale_price
//	hbposv7   : 净额 = sale_qnty × sale_price(该库根本没有退货行 ⇒ 不扣)
//	⇒ 两个 family 的"净额"**语义一致**,只是算法按源库事实不同。
//
// ⚠️ 绝对不要写成"负数行代表退货" —— 实测两个库 sale_qnty / sale_money / sale_price
//	的负值行都是 **0 行** ⇒ 那种写法在真库上恒等于"全部是销售"。
type SaleDetailTables struct {
	// Table 是逐笔流水表(思迅两库同名:t_rm_saleflow)。
	Table string
	// ReturnQtyCol 是"本行被退掉的数量"列名。
	//
	// ⚠️ **空 = 该库没有这一列**(hbposv7)⇒ 片段按"不扣退货"生成。
	// ⚠️ 这是 family 私有事实,所以走 config 而不是 mapping
	// (mapping 的 target 集合两个 family 必须一致)。
	ReturnQtyCol string
}

// 思迅逐笔流水表里几个固定列名(两个 family 实测同名)。
const (
	saleDetailQtyCol   = "sale_qnty"
	saleDetailPriceCol = "sale_price"
)

// SaleDetailFragment 构造 sale_detail 模型的 live 取数片段(派生表)。
//
// # 列名约定
//
//   - **源列**(sale_qnty / sale_price / …):沿用源库列名 ——
//     mapping.yaml 会把 canonical 名(qty / sale_price)重写成它们,列名对得上。
//   - **派生列**(memb_net_qty / memb_net_amount):用 **memb_ 前缀** ——
//     mapping.yaml 把它们钉成 canonical 名(memb_net_qty → net_qty、
//     memb_net_amount → net_amount),
//     于是"两个 family 的 target 集合必须逐字一致"这条规矩仍然成立。
//
// # 为什么要有 memb_net_amount 这个中间列
//
//	度量要的是"净额合计" = SUM(每行的净额金额)。
//	⇒ 每行的净额金额得先在行内算出来(片段的列)⇒ 度量才能对它求和。
//	若把算式直接写进度量(SUM(sale_price * (sale_qnty - ISNULL(ret_qnty,0)))),
//	那 ret_qnty 就进了共享 schema ⇒ hbposv7 那边直接 column not found。
func SaleDetailFragment(t SaleDetailTables) (string, error) {
	if err := assertIdent("table_sale_detail", t.Table); err != nil {
		return "", err
	}
	retCol := strings.TrimSpace(t.ReturnQtyCol)
	if retCol != "" {
		if err := assertIdent("sale_detail_return_qty_col", retCol); err != nil {
			return "", err
		}
		// 有退货列:净额 = (卖出 − 退回) × 单价。
		//
		// ⚠️ 派生列用 **memb_ 前缀**(与 member 的 memb_balance 同款):
		// mapping 里把它们钉成 canonical 名(memb_net_qty → net_qty),
		// 于是"两个 family 的 target 集合必须逐字一致"这条规矩仍然成立
		// (mapping parity 测试靠它)。
		return fmt.Sprintf(`(SELECT v.*,
  v.%s - ISNULL(v.%s, 0) AS memb_net_qty,
  v.%s * (v.%s - ISNULL(v.%s, 0)) AS memb_net_amount
FROM %s v)`,
			saleDetailQtyCol, retCol,
			saleDetailPriceCol, saleDetailQtyCol, retCol,
			t.Table), nil
	}
	// 没有退货列:该库的逐笔流水里**没有退货**(实测 sale_qnty/sale_money 负值 0 行)
	// ⇒ 净额 = 原销售金额。
	return fmt.Sprintf(`(SELECT v.*,
  v.%s AS memb_net_qty,
  v.%s * v.%s AS memb_net_amount
FROM %s v)`,
		saleDetailQtyCol, saleDetailPriceCol, saleDetailQtyCol, t.Table), nil
}
