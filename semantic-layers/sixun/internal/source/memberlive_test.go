package source_test

// 会员 / 会员类别模型的**真源库**验证。
//
// # 为什么要有这条测试
//
// 前面那些单测证明的是"我们生成了我们以为会生成的映射";而会员这块有三个
// 事实只能在**真库**上验出来:
//
//  1. **行数没有被 row_limit 截断**。
//     截断是**静默**的:`SELECT TOP n *` 没有 ORDER BY,丢的是随机行,
//     表现是"某些会员查不到",而 healthz 全绿、没有任何报错。
//     历史上商品表就因为硬编码 TOP 10000 丢过 63%(ysx)/ 77%(hbposv7)。
//
//  2. **一行都没被 mapping 转换失败而丢掉**。
//     dataloader.mapRows 对转换失败的行是**静默丢弃**的(continue,不是报错),
//     而 member 的 balance_yuan 源列 residual_amt 是带脏值的 varchar
//     ('20,,' / ':84420..' / '86220.-')—— 若把它按 decimal 处理,
//     ParseFloat 报错 ⇒ 整张表的行数会悄悄少一大截。
//     这条测试锁住"balance_yuan 必须是 string"。
//
//  3. **值没有被语义归一化**。
//     card_status 必须是源值 '0'/'1'/'2'/'3'/'5',
//     use_num 必须保留 999998/999999 这种"不限次"哨兵,
//     residual_amt 必须保留脏字符串。
//     归一化留在 schema meta + BI 层(AGENTS.md §4),mapping 里做就是越界。
//
// # 跑法
//
//	# ysx 的源库本机连不上,先起 ssh 隧道,再指定 host:port
//	ssh -N -L 11433:192.168.1.200:1433 gyy
//	$env:CUBE_TEST_YSX_HOSTPORT = "127.0.0.1:11433"
//	go test ./internal/source/ -run TestMemberLiveAgainstSource -v
//
// hbposv7 本机直连,不需要隧道(不设 CUBE_TEST_HBPOSV7_HOSTPORT 即可)。
//
// # 源库连不上 / config.yaml 不存在 ⇒ skip 而不是 fail
//
// config.yaml 含源库口令、不在版本库,只有打包机上有;
// 源库也可能只在隧道后面。skip 的语义是"这次没验",不是"验过了"。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/hbposv7"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source/ysx"

	models "github.com/YunBright/cube/sixun-models"
)

// liveConfig 是本测试需要的配置子集(与 cmd/sixun-*/main.go 的 appConfig 同构)。
type liveConfig struct {
	Source struct {
		DSN             string `yaml:"dsn"`
		TableMember     string `yaml:"table_member"`
		TableMemberType string `yaml:"table_member_type"`
		// 会员模型的派生列来源表(余额 / 累计消费 / 消费次数)。
		TableMemberConsume    string         `yaml:"table_member_consume"`
		TableMemberPaylist    string         `yaml:"table_member_paylist"`
		TableMemberSavingPlus string         `yaml:"table_member_savingplus"`
		RowLimit              int            `yaml:"row_limit"`
		RowLimits             map[string]int `yaml:"row_limits"`
	} `yaml:"source"`
}

// memberFamily 描述一个 family 的会员相关事实。
type memberFamily struct {
	name string
	// cmdDir 是 cmd/sixun-<name> 相对 internal/source 的路径。
	cmdDir string
	// memberRows / memberTypeRows 是 2026-10-21 对**生产源库**实测的 COUNT(*)。
	memberRows     int
	memberTypeRows int
	// hostPortEnv 是覆盖 DSN host:port 的环境变量名(ssh 隧道用)。
	hostPortEnv string
	// newConn 构造该 family 的 connector。
	newConn func(liveConfig, *source.RowLimits) (memberFetcher, error)
}

// memberFetcher 是两个 connector 共用的最小接口。
type memberFetcher interface {
	FetchMember(ctx context.Context) ([]map[string]any, error)
	FetchMemberType(ctx context.Context) ([]map[string]any, error)
	Close() error
}

func TestMemberLiveAgainstSource(t *testing.T) {
	families := []memberFamily{
		{
			name:           "ysx",
			cmdDir:         filepath.Join("..", "..", "cmd", "sixun-ysx"),
			memberRows:     4128,
			memberTypeRows: 3,
			hostPortEnv:    "CUBE_TEST_YSX_HOSTPORT",
			newConn: func(cfg liveConfig, rl *source.RowLimits) (memberFetcher, error) {
				return ysx.New(ysx.Options{
					DSN:             cfg.Source.DSN,
					TableMember:     cfg.Source.TableMember,
					TableMemberType: cfg.Source.TableMemberType,
					// ⚠️ 派生列来源表:缺任何一张 ⇒ 余额/消费算不出来
					// (MemberSQL 会直接报错,不会"悄悄少一列")。
					TableMemberConsume:    cfg.Source.TableMemberConsume,
					TableMemberPaylist:    cfg.Source.TableMemberPaylist,
					TableMemberSavingPlus: cfg.Source.TableMemberSavingPlus,
					RowLimits:             rl,
				})
			},
		},
		{
			name:           "hbposv7",
			cmdDir:         filepath.Join("..", "..", "cmd", "sixun-hbposv7"),
			memberRows:     2642,
			memberTypeRows: 3,
			hostPortEnv:    "CUBE_TEST_HBPOSV7_HOSTPORT",
			newConn: func(cfg liveConfig, rl *source.RowLimits) (memberFetcher, error) {
				return hbposv7.New(hbposv7.Options{
					DSN:             cfg.Source.DSN,
					TableMember:     cfg.Source.TableMember,
					TableMemberType: cfg.Source.TableMemberType,
					// ⚠️ 派生列来源表:缺任何一张 ⇒ 余额/消费算不出来。
					TableMemberConsume:    cfg.Source.TableMemberConsume,
					TableMemberPaylist:    cfg.Source.TableMemberPaylist,
					TableMemberSavingPlus: cfg.Source.TableMemberSavingPlus,
					RowLimits:             rl,
				})
			},
		},
	}

	for _, fam := range families {
		t.Run(fam.name, func(t *testing.T) {
			cfgPath := filepath.Join(fam.cmdDir, "config.yaml")
			if _, err := os.Stat(cfgPath); err != nil {
				t.Skipf("%s 不存在(不在版本库,只有打包机上有),跳过", cfgPath)
			}
			cfg, _, err := appcfg.LoadFrom[liveConfig](cfgPath)
			if err != nil {
				t.Fatalf("加载 %s 失败: %v", cfgPath, err)
			}
			if cfg.Source.DSN == "" {
				t.Skipf("%s 里没有 source.dsn,跳过", cfgPath)
			}
			if hp := strings.TrimSpace(os.Getenv(fam.hostPortEnv)); hp != "" {
				cfg.Source.DSN = overrideHostPort(cfg.Source.DSN, hp)
			}
			if cfg.Source.TableMember == "" || cfg.Source.TableMemberType == "" {
				t.Fatalf("%s 缺少 source.table_member / table_member_type —— "+
					"该 model 会静默不可查", cfgPath)
			}
			// ⚠️ 派生列来源表缺一列 ⇒ MemberSQL 直接报错(不会"悄悄少一列"),
			//   但错误信息只说"哪个表没配" ⇒ 这里先自己查清楚,报错才指得准。
			for name, v := range map[string]string{
				"source.table_member_consume":    cfg.Source.TableMemberConsume,
				"source.table_member_paylist":    cfg.Source.TableMemberPaylist,
				"source.table_member_savingplus": cfg.Source.TableMemberSavingPlus,
			} {
				if strings.TrimSpace(v) == "" {
					t.Fatalf("%s 缺少 %s —— 会员的余额/累计消费/消费次数都从这三张表派生",
						cfgPath, name)
				}
			}

			conn, err := fam.newConn(cfg, source.NewRowLimits(cfg.Source.RowLimit, cfg.Source.RowLimits))
			if err != nil {
				t.Skipf("连不上源库(没起隧道?): %v", err)
			}
			defer conn.Close()

			ctx := context.Background()
			memberMapper := loadMapper(t, fam.cmdDir, "member")
			typeMapper := loadMapper(t, fam.cmdDir, "member_type")

			// ---- member:行数必须等于实测值(没被 row_limit 截断)----
			rows, err := conn.FetchMember(ctx)
			if err != nil {
				t.Fatalf("FetchMember: %v", err)
			}
			if len(rows) != fam.memberRows {
				t.Fatalf("member 拉到 %d 行,实测是 %d 行 —— "+
					"等于某个整数上限就说明被 row_limit 截断了(静默,零报错)",
					len(rows), fam.memberRows)
			}
			t.Logf("%s member: 拉到 %d 行(与源库实测一致,未截断)", fam.name, len(rows))

			// ---- member:一行都不能被 mapping 转换失败丢掉 ----
			mapped := mapRowsForTest(t, memberMapper, rows)
			if len(mapped) != len(rows) {
				t.Fatalf("mapping 转换后只剩 %d 行(拉回来 %d 行)—— "+
					"有行被 fieldmapping.convert 拒绝了(dataloader.mapRows 是静默丢行的)",
					len(mapped), len(rows))
			}

			// ---- member:物理列集合必须与共享 schema 的 dimension 完全一致 ----
			assertTargetsCoverSchema(t, "member", memberMapper, rows[0], mapped[0])

			// ---- member:值没有被语义归一化 ----
			assertMemberNotNormalized(t, mapped)

			// ---- member_type ----
			typeRows, err := conn.FetchMemberType(ctx)
			if err != nil {
				t.Fatalf("FetchMemberType: %v", err)
			}
			if len(typeRows) != fam.memberTypeRows {
				t.Fatalf("member_type 拉到 %d 行,实测是 %d 行", len(typeRows), fam.memberTypeRows)
			}
			t.Logf("%s member_type: 拉到 %d 行", fam.name, len(typeRows))
			typeMapped := mapRowsForTest(t, typeMapper, typeRows)
			if len(typeMapped) != len(typeRows) {
				t.Fatalf("member_type mapping 转换后只剩 %d 行(拉回来 %d 行)",
					len(typeMapped), len(typeRows))
			}
			assertTargetsCoverSchema(t, "member_type", typeMapper, typeRows[0], typeMapped[0])
			assertMemberTypeNotNormalized(t, typeMapped)
		})
	}
}

// assertTargetsCoverSchema 断言 mapping 的 target 集合 == schema 的 dimension 名集合。
//
// 少一条 ⇒ DuckDB 里没有那一列 ⇒ 查询报 column not found;
// 多一条 ⇒ DuckDB 里多一列,schema 却不认识它。
//
// 抽样行里某个 dimension 没有键,是**允许**的:前提是它的源列在源库里不存在
// (hbposv7 的 t_rm_vip_info 没有 acc_num / dec_num ⇒ points_accumulated /
// points_used 恒为 NULL)。DuckDB 那一列仍然存在(由 mapping 的 target 建出来),
// 只是值恒为 NULL —— 这正是"照实映射"要的结果。
func assertTargetsCoverSchema(t *testing.T, model string, m *fieldmapping.Mapper,
	rawRow, mappedRow map[string]any) {
	t.Helper()
	schemaBytes, err := models.ReadSchema(model)
	if err != nil {
		t.Fatalf("读 schema %s: %v", model, err)
	}
	schema, err := cubeschema.Load(schemaBytes)
	if err != nil {
		t.Fatalf("解析 schema %s: %v", model, err)
	}
	want := make([]string, 0, len(schema.Dimensions))
	for _, d := range schema.Dimensions {
		want = append(want, d.Name)
	}
	got := make([]string, 0)
	for _, f := range m.TargetsWithType() {
		got = append(got, f.Target)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Fatalf("model %q 的 mapping target 与 schema dimension 不一致:\n  schema    = %v\n  mapping  = %v",
			model, want, got)
	}
	// 源列存在的 dimension,抽样行里必须有键(否则会被写成 NULL)。
	for _, name := range want {
		if _, ok := mappedRow[name]; ok {
			continue
		}
		src, ok := m.SourceOf(name)
		if !ok {
			t.Errorf("dimension %q 在 mapping 里没有源列", name)
			continue
		}
		if _, inRaw := rawRow[src]; inRaw {
			t.Errorf("dimension %q 的源列 %q 在行里存在,但映射后没有这个键", name, src)
			continue
		}
		t.Logf("dimension %q 的源列 %q 在源库里不存在 ⇒ 该列恒为 NULL(照实映射)", name, src)
	}
}

// assertMemberNotNormalized 锁住"没做语义归一化"这个事实。
//
// ⚠️ 2026-10-21 更新:balance_yuan **不再是**主表那列脏字符串。
//
//	它现在是**派生列**(Σ充值 − Σ支付,见 source/member_sql.go),
//	源表的脏原值改以 residual_amt_raw 保留(仅供审计)。
//	⇒ 这条测试现在同时锁两件事:
//	  1. balance_yuan 是**数字**(不是脏字符串)且**非负**;
//	  2. residual_amt_raw 仍是**原始字符串**(没被清洗)。
func assertMemberNotNormalized(t *testing.T, rows []map[string]any) {
	t.Helper()

	allowedStatus := map[string]bool{"0": true, "1": true, "2": true, "3": true, "5": true}
	statuses := map[string]int{}
	sentinels := 0
	dirtyRaw := 0
	positiveBalance := 0
	negativeBalance := 0
	totalBalance := 0.0
	samples := 0

	for _, r := range rows {
		st := fmt.Sprint(r["status"])
		statuses[st]++
		if !allowedStatus[st] {
			t.Errorf("status 出现非源值 %q —— 期望是源库的 '0'/'1'/'2'/'3'/'5'", st)
		}
		// use_num 的哨兵必须原样保留(不转 0、不转 NULL)。
		if n, ok := r["limited_use_count"].(int64); ok && n >= 999998 {
			sentinels++
		}
		// balance_yuan 现在是派生数字:必须能当数字读,且不为负
		// (负余额 = 充值算错了,宁可让这条测试红)。
		switch bal := r["balance_yuan"].(type) {
		case float64:
			if bal < 0 {
				negativeBalance++
			}
			if bal > 0 {
				positiveBalance++
			}
			totalBalance += bal
		case int64:
			if bal < 0 {
				negativeBalance++
			}
			if bal > 0 {
				positiveBalance++
			}
			totalBalance += float64(bal)
		default:
			t.Errorf("balance_yuan 不是数字(%T)—— 它是**派生列**(Σ充值−Σ支付),"+
				"必须是数字;脏字符串原值在 residual_amt_raw 里", r["balance_yuan"])
		}
		// 源库原值仍要是**原始字符串**(没被清洗)。
		raw, ok := r["residual_amt_raw"].(string)
		if !ok {
			t.Errorf("residual_amt_raw 不是字符串(%T)—— 源列 residual_amt 是 varchar,"+
				"且带脏值,按 decimal 处理会让 mapRows 静默丢行", r["residual_amt_raw"])
		} else {
			if _, err := strconv.ParseFloat(raw, 64); err != nil {
				dirtyRaw++
			}
		}
		if samples < 3 {
			samples++
			t.Logf("样本行: id=%v status=%v limited_use_count=%v balance_yuan=%v residual_amt_raw=%q points_accumulated=%v points_used=%v",
				r["id"], r["status"], r["limited_use_count"], r["balance_yuan"], raw,
				r["points_accumulated"], r["points_used"])
		}
	}
	t.Logf("card_status 分布=%v;不限次哨兵(>=999998)=%d 行;脏原值(解析不出数字)=%d 行",
		statuses, sentinels, dirtyRaw)
	t.Logf("派生余额: 正余额 %d 行 / 负余额 %d 行 / 合计 %.2f 元",
		positiveBalance, negativeBalance, totalBalance)
	if negativeBalance > 0 {
		t.Errorf("有 %d 行余额为负 —— 充值/支付的口径算错了(Σ充值 − Σ支付 不可能为负)", negativeBalance)
	}
	if dirtyRaw == 0 {
		t.Log("提示:本库没有解析不出数字的原值(不代表被清洗过,只是源数据恰好干净)")
	}
}

// assertMemberTypeNotNormalized 锁住会员类别字典"原样给"。
func assertMemberTypeNotNormalized(t *testing.T, rows []map[string]any) {
	t.Helper()
	for _, r := range rows {
		// discount 是 numeric → mapping 必须是 decimal,否则会拿到字节码。
		d, ok := r["discount_rate"].(float64)
		if !ok {
			t.Errorf("discount_rate 不是数字(%T=%v)—— mapping 的 type 必须写 decimal,"+
				"写 string 会拿到 \"[49 46 48 48 48 48 48]\" 这样的字节码",
				r["discount_rate"], r["discount_rate"])
			continue
		}
		if d != 1.0 {
			t.Errorf("discount_rate=%v,实测三行都是 1.0000 —— 多半被换算过(不该由 cube 决定口径)", d)
		}
		// saving_flag / acc_flag 是 char(1) 的 '0'/'1' → 必须是字符串,不是 bool。
		for _, k := range []string{"allow_stored_value", "allow_points_pay"} {
			v, ok := r[k].(string)
			if !ok {
				t.Errorf("%s 不是字符串(%T)—— char(1) 的 '0'/'1' 必须原样给字符串", k, r[k])
				continue
			}
			if v != "0" && v != "1" {
				t.Errorf("%s=%q,期望是源库的 '0'/'1'", k, v)
			}
		}
		t.Logf("样本行: id=%q name=%q discount_rate=%v allow_stored_value=%v allow_points_pay=%v",
			r["id"], r["name"], r["discount_rate"], r["allow_stored_value"], r["allow_points_pay"])
	}
}

// mapRowsForTest 复刻 dataloader.mapRows 的行为,但把"丢行"变成显式失败。
func mapRowsForTest(t *testing.T, m *fieldmapping.Mapper, rows []map[string]any) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(rows))
	for i, row := range rows {
		mr, err := m.Apply(row)
		if err != nil {
			t.Fatalf("第 %d 行 mapping 转换失败:%v —— dataloader 会**静默丢**这一行", i, err)
		}
		out = append(out, mr)
	}
	return out
}

// loadMapper 从 family 的 mapping 目录读某个 model 的映射。
func loadMapper(t *testing.T, cmdDir, model string) *fieldmapping.Mapper {
	t.Helper()
	l, err := fieldmapping.NewLoader(filepath.Join(cmdDir, "mapping"))
	if err != nil {
		t.Fatalf("加载 mapping 目录失败: %v", err)
	}
	m, ok := l.Get(model)
	if !ok {
		t.Fatalf("mapping 目录里没有 model %q", model)
	}
	return m
}

// overrideHostPort 把 DSN 里的 host:port 换掉,其余原样保留。
//
// 做法:只在 '?' 之前的部分里找最后一个 '@',避免密码里的 '@' 干扰。
func overrideHostPort(dsn, newHostPort string) string {
	head, tail := dsn, ""
	if i := strings.Index(dsn, "?"); i >= 0 {
		head, tail = dsn[:i], dsn[i:]
	}
	at := strings.LastIndex(head, "@")
	if at < 0 {
		return dsn // 不是标准形态,别瞎改
	}
	return head[:at+1] + newHostPort + tail
}
