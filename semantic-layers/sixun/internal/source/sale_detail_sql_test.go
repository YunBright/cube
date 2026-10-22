package source_test

// 逐笔流水 live 片段的守卫。
//
// # 为什么要有这条
//
// 逐笔流水的**退货不是独立行**,而是同一行里的"被退掉的数量"列
// (ysx 的 ret_qnty)⇒ 净额只能由 **family 私有的片段**算。
//
// ⚠️ 旧写法 `SUM(CASE WHEN qty < 0 THEN -sale_price*qty ELSE sale_price*qty END)`
// 是**假的**:实测两个库的 sale_qnty / sale_money / sale_price 负值行都是 0 行
// ⇒ 它恒等于 total_amount_yuan ⇒ "净额"骗人(而报告照常成功,零报错)。
//
// ⇒ 这条测试把"两种 family 形态"钉住。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
)

// TestSaleDetailFragment_WithReturnQty 锁"有退货列的形态(ysx)"。
func TestSaleDetailFragment_WithReturnQty(t *testing.T) {
	frag, err := source.SaleDetailFragment(source.SaleDetailTables{
		Table:        "t_rm_saleflow",
		ReturnQtyCol: "ret_qnty",
	})
	require.NoError(t, err)

	// ① 净额数量 = 卖出 − 退回。
	assert.Contains(t, frag, "v.sale_qnty - ISNULL(v.ret_qnty, 0) AS memb_net_qty",
		"净额数量应是 卖出 − 退回:\n%s", frag)
	// ② 净额金额 = 净额数量 × 单价。
	assert.Contains(t, frag, "AS memb_net_amount",
		"净额金额必须按净额数量算:\n%s", frag)
	// ③ 派生列用 **canonical 名**(不在 mapping 里 ⇒ 编译器不重写)。
	assert.Contains(t, frag, "AS memb_net_qty", "派生列名必须是 memb_ 前缀(mapping 靠它钉住 canonical 名):\n%s", frag)
	// ④ 源列保持源库列名(mapping 会把 canonical 名重写成它们)。
	assert.Contains(t, frag, "v.sale_qnty", "源列必须用源库列名:\n%s", frag)
	// ⑤ 片段是派生表 ⇒ 整体包在括号里。
	assert.True(t, strings.HasPrefix(frag, "(SELECT"), "片段必须是括号包起来的子查询:\n%s", frag)
}

// TestSaleDetailFragment_WithoutReturnQty 锁"没有退货列的形态(hbposv7)"。
//
// ⚠️ 该库逐笔流水里**没有退货行**(负值 0 行)⇒ 净额 = 原销售金额。
func TestSaleDetailFragment_WithoutReturnQty(t *testing.T) {
	frag, err := source.SaleDetailFragment(source.SaleDetailTables{
		Table:        "t_rm_saleflow",
		ReturnQtyCol: "",
	})
	require.NoError(t, err)

	assert.Contains(t, frag, "v.sale_qnty AS memb_net_qty",
		"没有退货列时净额 = 原销售数量:\n%s", frag)
	assert.NotContains(t, frag, "ret_qnty",
		"没配退货列就不该出现它(那会让源库报列名无效):\n%s", frag)
	assert.Contains(t, frag, "AS memb_net_amount", "净额金额列仍然要在:\n%s", frag)
}

// TestSaleDetailFragmentRejectsBadIdentifier 锁"表名/列名要过标识符白名单"。
//
// ⚠️ 表名会**拼进 SQL 文本**(不是参数绑定能解决的 —— 表名不能占位)。
// 配置是人写的 ⇒ 拼错就是一条注入。
func TestSaleDetailFragmentRejectsBadIdentifier(t *testing.T) {
	_, err := source.SaleDetailFragment(source.SaleDetailTables{
		Table:        "t_rm_saleflow; DROP TABLE x",
		ReturnQtyCol: "",
	})
	require.Error(t, err, "非法表名必须报错,不能拼进 SQL")

	_, err = source.SaleDetailFragment(source.SaleDetailTables{
		Table:        "t_rm_saleflow",
		ReturnQtyCol: "ret_qnty) AS x, (SELECT 1",
	})
	require.Error(t, err, "非法列名必须报错,不能拼进 SQL")
}
