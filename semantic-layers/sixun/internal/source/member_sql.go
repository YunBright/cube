package source

import (
	"fmt"
	"strings"
)

// MemberTables 是 member 模型取数时要用到的四张表(family 私有事实)。
//
// ⚠️ 表名**只能**来自各 family 的 config.yaml(source.table_member*)，
// 不能写死在这里 —— 思迅不同版本(v10 / v7)的会员表名可能不同，
// 而这个包是两个 family 共用的。
type MemberTables struct {
	// Member 是会员档案主表(思迅 V10: t_rm_vip_info)。
	Member string
	// Consume 是会员消费汇总表(t_rm_vip_consume):
	// 累计消费金额(vip_cons_amount)与消费次数(cons_count)只在这里,
	// 主表的 consum_amt / consum_count 实测**几乎全空**
	// (ysx: consum_amt 大量 0.0000 / consum_count 大量 NULL)。
	Consume string
	// Paylist 是储值卡支付流水(t_rm_card_paylist):
	// 储值支付流水 + 每笔之后的**卡内余额**(residual_amt,这里是 numeric)。
	Paylist string
	// SavingPlus 是储值充值记录(t_rm_saving_plus_record):
	// plus_count = 充值金额。
	SavingPlus string
	// StoredPayType 是 paylist.card_type 里表示"**储值支付**"的那个取值。
	//
	// ⚠️ paylist 里**不全是储值支付**:
	// 实测 ysx 会员卡(card_type='0')的 245212 行全是 card_type='1',
	// 那是"刷卡消费/积分抵扣"记录(金额合计 908 万),**与储值余额无关**;
	// 只有 card_type='2' 的行是储值支付(其 residual_amt = 该笔之后的卡内余额)。
	// 不区分就��把 会员卡的消费额 当成"储值消费"扣掉 ⇒ 余额算成负数
	// (实测 2521 张卡余额为负、合计 −907 万)。
	//
	// ✅ 两个 family 实测都是 '2'(hbposv7 的 paylist 只有 '2' 行)。
	// ⇒ 做成**可配置项**:不同门店/版本的思迅若换了这个编码,改配置即可,
	// 不用改代码(也不该为它写死一个魔数)。
	StoredPayType string
	// PointsAccumulatedCol / PointsUsedCol 是"累计积分 / 已用积分"的**源列名**,
	// 用来在源库侧派生「剩余积分」(累计 − 已用)。
	//
	// ⚠️ 这是 family 私有事实(源列名各库不同),所以走配置而不是写死:
	//   ysx     : acc_num / dec_num
	//   hbposv7 : **没有这两列**(只有 now_acc_num 且 2642 行全为 NULL)
	//             ⇒ 留空 ⇒ 该库不派生这一列 ⇒ 恒为 NULL(照实映射)。
	PointsAccumulatedCol string
	PointsUsedCol        string
}

// DefaultStoredPayType 是 StoredPayType 的默认值('2' = 储值支付)。
const DefaultStoredPayType = "2"

// MemberSQL 构造 member 模型的取数 SQL(带三张派生表的 JOIN)。
//
// # 为什么必须 JOIN(而不是只 SELECT * FROM t_rm_vip_info)
//
// 会员档案表里的三个字段**不是会员的真实值**:
//
//  1. **residual_amt(卡内余额)是脏字符串**,实测 ysx 43 种取值:
//     '86220.0' / ':844201-' / '20,,' / ',' / '0' …
//     垃圾字符全在 ASCII 44–64(':' '<' '>' '@' ',' '-' '/' '.'),
//     同一批卡上出现**同一个**残值('86220.x' 覆盖 762 张卡),
//     而这些卡的真实余额(实测 0 ~ 99.6 元)与它**毫无关系**。
//     ⇒ 把它当余额导出去,前端就会显示「余额 86220 元」。
//
//  2. **consum_amt / consum_count 不是累计消费**:主表里 4128 行只有 403 行
//     consum_amt 非 0,consum_count **全为 NULL**。
//     真实的累计消费在 t_rm_vip_consume.vip_cons_amount / .cons_count。
//
//  3. **卡内余额的真值 = Σ充值 − Σ支付**:
//     实测逐卡与 t_rm_card_paylist 最新一行的 residual_amt **精确相等**
//     (900223: 200.00 − 168.40 = 31.60 = paylist 最新 residual_amt 31.60)。
//
// ⇒ 余额、累计消费、消费次数这三列由本 SQL 在**源库侧**算出来
// (相当于源库的一个 view),cube 只负责把算好的值搬出去。
//
// # 为什么余额只对"储值卡"有意义
//
// 充值记录实测**只覆盖 card_type='1'(储值卡)**(1162 张卡),
// 会员卡(card_type='0')一行充值记录都没有
// ⇒ 会员卡的余额自然是 0,不需要按卡类特判。
//
// # 列别名不能与主表重名
//
// 派生列用 memb_ 前缀(alias),否则 SELECT v.* 与 JOIN 列同名时
// MSSQL 会报 "列名重复"。
const (
	// MemberBalanceAlias 是派生列「卡内余额(元)」的别名。
	MemberBalanceAlias = "memb_balance"
	// MemberConsumeAmountAlias 是派生列「累计消费(元)」的别名。
	MemberConsumeAmountAlias = "memb_consume_amount"
	// MemberConsumeCountAlias 是派生列「消费次数」的别名。
	MemberConsumeCountAlias = "memb_consume_count"
	// MemberRemainingPointsAlias 是派生列「剩余积分(累计 − 已用)」的别名。
	//
	// ⚠️ 只有源库**同时有**累计积分列与已用积分列时才派生(hbposv7 没有 ⇒ 该库恒 NULL)。
	MemberRemainingPointsAlias = "memb_remaining_points"
)

// MemberSQL 构造 member 模型的取数 SQL(带三张派生表的 JOIN)。
//
// ⚠️ 这条 SQL 是**落 DuckDB 那条路**用的(loader 拉全表 → 建 _raw 表)。
// live 路(实时透传源库)用 MemberFromFragment。
//
// ⚠️ 表名来自 config.yaml(不是用户输入),但仍做一次**标识符白名单校验**:
// 表名会拼进 SQL 文本,配置写错时宁可报错也不要拼出坏 SQL。
func MemberSQL(t MemberTables, limit int) (string, error) {
	body, err := memberSelectBody(t)
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = DefaultRowLimit
	}
	// ⚠️ LIMIT 用 TOP:源库是 SQL Server 2008(不能 OFFSET-FETCH)。
	return fmt.Sprintf("SELECT TOP %d\n%s", limit, body), nil
}

// MemberFromFragment 构造 member 模型的 **live 取数片段**(派生表)。
//
// # 为什么需要它
//
// member 的三个派生列(卡内余额 / 累计消费 / 消费次数)**在源库里不存在**:
// 它们是"主表 JOIN 三张流水表"算出来的(见本文件顶部的口径说明)。
// 而 live model 的查询是"把 schema 编译成 T-SQL 直打源库",
// 没有"先把表物化下来再算"这一步 ⇒ 派生列必须由**片段**在源库侧算出来。
//
// # 片段的列名约定
//
//   - **源列**(card_id / vip_name / …):沿用源库列名 ——
//     mapping.yaml 会把 canonical 名(如 id / name)重写成源列名,列名对得上。
//   - **派生列**(余额 / 消费 / 积分):沿用 **memb_\* 别名** ——
//     mapping.yaml 里也有这几条(source: memb_balance → target: balance_yuan),
//     所以 canonical 名照样被重写成 memb_balance ⇒ 片段里的列名对得上。
//
// ⇒ **片段的列名 = 源列名(含 memb_\* 别名)**,不需要另造一套 canonical 名。
//
// # 与 MemberSQL 的差别
//
//   - 没有 TOP(分页由 cube 的 limit/offset 负责);
//   - 整体包在括号里 ⇒ 它在 SQL 里是**派生表**,不是表名。
func MemberFromFragment(t MemberTables) (string, error) {
	body, err := memberSelectBody(t)
	if err != nil {
		return "", err
	}
	// ⚠️ **必须带 SELECT**:memberSelectBody 给的是"SELECT 列表 + FROM"这一段,
	// 拼进片段时要自己补上 SELECT —— 漏了会变成 `FROM ( v.*, … )`,
	// 报 "mssql: '*' 附近有语法错误" —— 看起来像源库坏了,其实只是少一个词。
	return "(SELECT\n" + body + ")", nil
}

// memberSelectBody 构造 member 取数 SQL 的 SELECT + FROM 部分(不含 TOP)。
//
// ⚠️ 落 DuckDB(去 DuckDB 之前)与 live 片段**共用这一段** ——
// 两边一旦分叉,同一张卡的余额就会在"物化副本"与"实时透传"里对不上,
// 而这种不一致**没有任何一行报错**。
func memberSelectBody(t MemberTables) (string, error) {
	for name, v := range map[string]string{
		"table_member":            t.Member,
		"table_member_consume":    t.Consume,
		"table_member_paylist":    t.Paylist,
		"table_member_savingplus": t.SavingPlus,
	} {
		if err := assertIdent(name, v); err != nil {
			return "", err
		}
	}
	storedType := strings.TrimSpace(t.StoredPayType)
	if storedType == "" {
		storedType = DefaultStoredPayType
	}
	// ⚠️ 存值支付类型码是**字符串字面量**(不是标识符)⇒ 只能做"引号内无引号"校验。
	if strings.ContainsAny(storedType, "'") {
		return "", fmt.Errorf("source: 储值支付类型码 %q 含引号,拒绝拼进 SQL", storedType)
	}
	// 积分列名(可选):配了才派生「剩余积分」这一列。
	accCol := strings.TrimSpace(t.PointsAccumulatedCol)
	usedCol := strings.TrimSpace(t.PointsUsedCol)
	remainingExpr := ""
	if accCol != "" && usedCol != "" {
		if err := assertIdent("table_member_points_accumulated", accCol); err != nil {
			return "", err
		}
		if err := assertIdent("table_member_points_used", usedCol); err != nil {
			return "", err
		}
		remainingExpr = fmt.Sprintf(",\n  ISNULL(%s, 0) - ISNULL(%s, 0) AS %s",
			accCol, usedCol, MemberRemainingPointsAlias)
	} else {
		// ⚠️ **没配积分列时也必须给出这一列**(值为 NULL)。
		//
		// hbposv7 的 t_rm_vip_info 没有 acc_num / dec_num ⇒ 该库的「剩余积分」恒为 NULL
		// (schema 里写明了这个语义)⇒ 但**列必须在**:
		// 少这一列 ⇒ 派生表里没有 memb_remaining_points
		// ⇒ 编译期把 canonical 名 remaining_points 重写成 memb_remaining_points
		// ⇒ 源库报「列名 'memb_remaining_points' 无效」⇒ **整个 member 查询 500**。
		//
		// ⇒ 也就是说:「这一列没值」与「这一列不存在」是两件事,
		// 前者照实给 NULL,后者会让整张 model 查不了。
		remainingExpr = fmt.Sprintf(",\n  NULL AS %s", MemberRemainingPointsAlias)
	}
	// ⚠️ LIMIT 用 TOP:源库是 SQL Server 2008(不能 OFFSET-FETCH)。
	return fmt.Sprintf(`  v.*,
  c.vip_cons_amount AS %s,
  c.cons_count AS %s,
  ISNULL(p.plus_total, 0) - ISNULL(pay.pay_total, 0) AS %s%s
FROM %s v
LEFT JOIN (SELECT card_id, MAX(vip_cons_amount) AS vip_cons_amount, MAX(cons_count) AS cons_count FROM %s GROUP BY card_id) c
       ON c.card_id = v.card_id
LEFT JOIN (SELECT card_id, SUM(plus_count) AS plus_total FROM %s GROUP BY card_id) p
       ON p.card_id = v.card_id
LEFT JOIN (SELECT card_id, SUM(amount) AS pay_total FROM %s WHERE card_type = '%s' GROUP BY card_id) pay
       ON pay.card_id = v.card_id`,
		MemberConsumeAmountAlias,
		MemberConsumeCountAlias,
		MemberBalanceAlias,
		remainingExpr,
		t.Member, t.Consume, t.SavingPlus, t.Paylist, storedType,
	), nil
}

// assertIdent 校验表名是合法标识符(只允许字母/数字/下划线,不允许空格与符号)。
//
// ⚠️ 表名会**拼进 SQL 文本**(不是参数绑定能解决的 —— 表名不能占位)。
// 配置里的表名不是外部输入,但配置是**人写的**:
// 写成 "t_rm_vip_info; DROP TABLE x" 时,没有这道校验就是一条注入。
func assertIdent(name, v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("source: %s 未配置", name)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return fmt.Errorf("source: %s=%q 不是合法表名标识符", name, v)
		}
	}
	return nil
}
