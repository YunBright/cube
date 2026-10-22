package source_test

// 会员 live 取数**片段**的守卫。
//
// # 为什么要有这条
//
// 2026-10-23 第一次上线时,片段少了一个 `SELECT` 关键字:
//
//	FROM (  v.*, c.vip_cons_amount AS memb_consume_amount, … ) AS t0
//	                                ^^^^^^^^ MSSQL: '*' 附近有语法错误
//
// 症状在**生产**才暴露(单测只比对了字符串),
// 而报错信息("'*' 附近有语法错误")看起来像**源库坏了**,
// 排查方向整个跑偏 —— 真相只是少了一个词。
//
// ⇒ 这条测试把"片段是完整的 SELECT"钉住。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
)

// memberTables 是片段需要的四张表(思迅 v10 / v7 同名)。
func memberTables() source.MemberTables {
	return source.MemberTables{
		Member:               "t_rm_vip_info",
		Consume:              "t_rm_vip_consume",
		Paylist:              "t_rm_card_paylist",
		SavingPlus:           "t_rm_saving_plus_record",
		StoredPayType:        "2",
		PointsAccumulatedCol: "acc_num",
		PointsUsedCol:        "dec_num",
	}
}

// TestMemberFromFragmentIsCompleteSelect 锁"片段是完整的 SELECT 子查询"。
func TestMemberFromFragmentIsCompleteSelect(t *testing.T) {
	frag, err := source.MemberFromFragment(memberTables())
	require.NoError(t, err)

	// ① 必须以 "(SELECT" 开头。
	//    少了 SELECT ⇒ `FROM ( v.*, … )` ⇒ MSSQL 报 "'*' 附近有语法错误"。
	assert.True(t, strings.HasPrefix(frag, "(SELECT"),
		"片段必须以 (SELECT 开头(少了 SELECT 会变成 FROM ( v.*, … ) ⇒ 语法错):\n%s", frag)

	// ② 必须有 FROM 与主表(否则查不到会员)。
	assert.Contains(t, frag, "FROM t_rm_vip_info v",
		"片段必须从会员主表取数:\n%s", frag)

	// ③ 三张派生表都要在(余额 / 累计消费 / 消费次数)。
	for _, want := range []string{
		"t_rm_vip_consume",       // 累计消费 / 消费次数
		"t_rm_saving_plus_record", // 充值
		"t_rm_card_paylist",       // 储值支付
	} {
		assert.Contains(t, frag, want, "片段必须 JOIN %s:\n%s", want, frag)
	}

	// ④ 派生列用 **memb_\* 别名** —— mapping.yaml 靠它把 canonical 名
	//    (balance_yuan / consumed_amount_yuan / consumed_count / remaining_points)
	//    重写成源列名;别名对不上 ⇒ 查询那一刻 column not found。
	for _, want := range []string{
		"memb_balance", "memb_consume_amount", "memb_consume_count", "memb_remaining_points",
	} {
		assert.Contains(t, frag, want, "片段必须提供派生列 %s(mapping 靠它重写 canonical 名):\n%s",
			want, frag)
	}
}

// TestMemberFromFragmentRejectsBadTableName 锁"表名要过标识符白名单"。
//
// ⚠️ 表名会**拼进 SQL 文本**(不是参数绑定能解决的 —— 表名不能占位)。
// 配置是人写的:写成 "t_rm_vip_info; DROP TABLE x" 时没有这道校验就是一条注入。
func TestMemberFromFragmentRejectsBadTableName(t *testing.T) {
	tbl := memberTables()
	tbl.Member = "t_rm_vip_info; DROP TABLE x"
	_, err := source.MemberFromFragment(tbl)
	require.Error(t, err, "非法表名必须报错,不能拼进 SQL")
}

// TestMemberFragmentHasNoTop 锁"片段里没有 TOP"。
//
// TOP 是**落 DuckDB 那条路**的(loader 一次拉一页);
// live 路的分页由 cube 的 limit/offset 负责 ⇒ 片段里带 TOP 会把行数**截断**,
// 而且是静默的(少了很多行,没有任何报错)。
func TestMemberFragmentHasNoTop(t *testing.T) {
	frag, err := source.MemberFromFragment(memberTables())
	require.NoError(t, err)
	assert.NotContains(t, strings.ToUpper(frag), "TOP ",
		"live 片段不该带 TOP(分页由 cube 的 limit/offset 负责;带 TOP 会静默截断):\n%s", frag)
}

// TestMemberFragmentAlwaysProvidesDerivedColumns 锁"四个派生列**永远在**"。
//
// ⚠️ 2026-10-23 生产实测:hbposv7 的 t_rm_vip_info **没有** acc_num / dec_num
// ⇒ 当时的片段在"没配积分列"时**整列省掉**
// ⇒ 派生表里没有 memb_remaining_points
// ⇒ 编译期把 canonical 名 remaining_points 重写成 memb_remaining_points
// ⇒ 源库报「列名 'memb_remaining_points' 无效」⇒ **整个 member 查询 500**
// (而 ysx 侧一切正常 ⇒ 症状是"一家店能查会员、另一家不能")。
//
// ⇒ 结论:**「这一列没值」与「这一列不存在」是两件事** ——
// 前者照实给 NULL,后者会让整张 model 查不了。
func TestMemberFragmentAlwaysProvidesDerivedColumns(t *testing.T) {
	// ① 配了积分列。
	frag, err := source.MemberFromFragment(memberTables())
	require.NoError(t, err)
	for _, want := range []string{
		"memb_balance", "memb_consume_amount", "memb_consume_count", "memb_remaining_points",
	} {
		assert.Contains(t, frag, want, "片段必须提供 %s:\n%s", want, frag)
	}

	// ② **没配**积分列(hbposv7 的情形):列仍然要在,只是值为 NULL。
	noPoints := memberTables()
	noPoints.PointsAccumulatedCol = ""
	noPoints.PointsUsedCol = ""
	frag2, err := source.MemberFromFragment(noPoints)
	require.NoError(t, err)
	for _, want := range []string{
		"memb_balance", "memb_consume_amount", "memb_consume_count", "memb_remaining_points",
	} {
		assert.Contains(t, frag2, want,
			"没配积分列时也必须给出 %s(给 NULL,而不是整列省掉 —— 省掉会让整个 member 查询 500):\n%s",
			want, frag2)
	}
	assert.Contains(t, frag2, "NULL AS "+source.MemberRemainingPointsAlias,
		"没配积分列时 remaining_points 应照实给 NULL:\n%s", frag2)
}
