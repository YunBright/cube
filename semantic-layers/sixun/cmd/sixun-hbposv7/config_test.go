package main

// 对**实际会部署的那份 config.yaml** 的约束。
//
// # 为什么需要它
//
// config.yaml 不在版本库(.gitignore),而且 deploy-cube.ps1 会用**打包机工作区**
// 里那份覆盖服务器上的。后果是:代码里的默认值和约束单测全绿,但部署出去的配置
// 可能根本没配 refresh 段 —— 此时增量完全没生效,系统安静地退回每 30 分钟全量
// 重拉,而日志只有一行 "incremental_models=0",没人会注意到。
//
// 这类"配置文件与代码约定漂移"的问题,只有**直接断言配置文件本身**才拦得住。
//
// 文件不存在时 skip 而不是 fail:CI 与其他开发机上没有这份文件(含 DSN 口令),
// 在那里断言等于制造假红。

import (
	"os"
	"strings"
	"testing"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/refreshschedule"
	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
)

// measuredProductRows 是 2026-10-10 对**两个生产源库**实测的 COUNT(*)
// (hbposepro = 44316,ysx/hbposv10 = 27304)。
//
// 改成实测值较大的那个:两个 family 的 config 在同一台打包机上,断言必须对
// 两者都成立。**重新实测后请同步更新这里**,否则这条测试会开始保护一个过时的数字。
const measuredProductRows = 44316

func loadDeployedConfig(t *testing.T) appConfig {
	t.Helper()
	const path = "config.yaml"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s 不存在(不在版本库,只有打包机上有),跳过", path)
	}
	cfg, used, err := appcfg.LoadFrom[appConfig](path)
	if err != nil {
		t.Fatalf("加载 %s 失败: %v", path, err)
	}
	if used != path {
		// config.local.yaml 优先级更高,留在服务器上会盖住部署下去的版本
		// (deploy-cube.ps1 的 prune 步骤就是专门删它的)。
		t.Fatalf("实际生效的配置是 %s 而不是 %s —— 本次断言的不是会部署的那份", used, path)
	}
	return cfg
}

// TestDeployedConfigEnablesIncremental 增量必须真的被配上。
//
// 空 map 是一个**合法且安静**的退路(dataloader 会全部走全量),所以它不会以
// "配置错误"的形式暴露,只会在性能上体现 —— 也就是没人会发现。
func TestDeployedConfigEnablesIncremental(t *testing.T) {
	cfg := loadDeployedConfig(t)
	if len(cfg.Refresh.Incremental) == 0 {
		t.Fatal("refresh.incremental 为空 —— 增量刷新完全没启用," +
			"系统会安静地退回每 30 分钟全量重拉 4.4 万行,而没有任何报错")
	}
	for model, col := range cfg.Refresh.Incremental {
		if strings.TrimSpace(col) == "" {
			t.Errorf("model %q 配了空的增量列名", model)
		}
	}
	// 这两个是实测有 modify_date 且行数够大的表,漏配就等于白做增量。
	for _, want := range []string{"product", "supplier"} {
		if _, ok := cfg.Refresh.Incremental[want]; !ok {
			t.Errorf("refresh.incremental 缺少 %q(实测该表有 modify_date)", want)
		}
	}
}

// TestDeployedFullScheduleIsSixFieldCron 锁住 cron 字段数。
//
// 这是本轮真机抓到的一个**静默**陷阱:5 字段会被 Dapr 拒绝(启动不失败、
// mode 照样 dapr-job),于是夜间全量校准再也不会发生,而增量漏的数据永远
// 没有兜底。refreshschedule 侧的 validateSchedule 只在进程启动时兜底,
// 配置文件这一层由本测试守住。
func TestDeployedFullScheduleIsSixFieldCron(t *testing.T) {
	cfg := loadDeployedConfig(t)
	s := cfg.fullSchedule()
	if n := len(strings.Fields(s)); n != 6 {
		t.Fatalf("refresh.full_schedule=%q 是 %d 字段,Dapr 要求 6 字段(秒 分 时 日 月 周)"+
			" —— 实测 5 字段会得到 HTTP 500: expected exactly 6 fields, found 5,"+
			"且主 job 不受影响、夜间校准静默消失", s, n)
	}
	if err := refreshschedule.ValidateSchedule(s); err != nil {
		t.Fatalf("full_schedule 没过 schedule 校验: %v", err)
	}
}

// TestDeployedRowLimitsCoverMeasuredSizes 锁住"不静默截断"。
//
// 兜底 row_limit 是 50000,而 hbposv7 的 product 实测 44316 —— 只剩 12% 余量。
// 商品涨过 5 万时会被静默截断(没有任何报错,只是"某些条码扫不出来")。
// 这个坑历史上已经踩过一次:硬编码 TOP 10000 丢了 77% 商品,零报错。
func TestDeployedRowLimitsCoverMeasuredSizes(t *testing.T) {
	cfg := loadDeployedConfig(t)
	if got := cfg.Source.RowLimits["product"]; got < measuredProductRows {
		t.Fatalf("source.row_limits.product=%d 不高于实测行数 %d —— 会静默截断商品",
			got, measuredProductRows)
	}
	// 兜底值也必须高于所有实测值:将来新增一个 duck model 忘了加进 row_limits,
	// 兜底就是它唯一的保护。
	if cfg.Source.RowLimit < measuredProductRows {
		t.Fatalf("source.row_limit(兜底)=%d 不高于实测行数 %d",
			cfg.Source.RowLimit, measuredProductRows)
	}
}

// TestDeployedIncrementalFallbackIsRealIfPresent 校验回退列**若配了必须是真的**。
//
// 为什么本库允许不配(而 ysx 那边必须配):2026-10-10 实测,本库 modify_date
// 为 NULL 的 12607 行商品,build_date **也全部为 NULL**
// (`modify_date IS NULL AND build_date IS NOT NULL` 返回 0 行),
// 所以 COALESCE 对它零收益 —— 30 天窗口 209 → 209,一行不多。
// 不为零收益的配置增加复杂度。
//
// 但万一将来门店补齐了 build_date、或者有人出于"两个 family 保持一致"
// 的想法顺手配上,就必须配一个**语义确实是建档时间**的列。配错的后果是
// 静默的:SQL 能跑、结果看着正常,只是漏数据或多拉数据。
//
// 判断依据是数据,所以这里**不**断言"必须为空" —— 只断言"配了就得是对的"。
func TestDeployedIncrementalFallbackIsRealIfPresent(t *testing.T) {
	cfg := loadDeployedConfig(t)

	fb := cfg.Refresh.IncrementalFallback["product"]
	if fb == "" {
		return // 实测零收益,不配是正确的
	}
	if fb != "build_date" {
		t.Fatalf("product 的回退列只能是 build_date(源表里唯一语义为建档时间的列),"+
			"实际配了 %q —— 配一个语义不明的列会让增量静默漏数据", fb)
	}
	expr, err := source.IncrementalCols{
		Primary:  cfg.Refresh.Incremental["product"],
		Fallback: fb,
	}.Expression()
	if err != nil {
		t.Fatalf("回退列没通过标识符校验(可能是拼错了列名): %v", err)
	}
	if expr != "COALESCE([modify_date], [build_date])" {
		t.Errorf("增量表达式应是 COALESCE([modify_date], [build_date]),实际 %q", expr)
	}
}

// TestNightlyFullReloadStillRequired 锁住"夜间全量校准不可省"。
//
// 本库有 **12607 行商品(占全部 28%)modify_date 与 build_date 同时为 NULL**,
// 它们是任何基于时间列的增量都永远拉不到的。只要这行存在,全量校准就是
// 唯一兜底 —— 谁把它去掉,商品档案就会开始静默发霉。
func TestNightlyFullReloadStillRequired(t *testing.T) {
	cfg := loadDeployedConfig(t)
	if strings.TrimSpace(cfg.Refresh.FullSchedule) == "" {
		t.Fatal("refresh.full_schedule 为空 —— 没有夜间全量校准。" +
			"本库有 12607 行商品两个时间列都是 NULL,增量永远拉不到它们")
	}
	if len(cfg.Refresh.IncrementalFallback) > 0 {
		// 配了回退列不改变这个结论,这里只是提醒别把它当成全量的替代品。
		t.Log("注意:即使配了 incremental_fallback,12607 行 both-NULL 的商品" +
			"仍然只能靠夜间全量兜底")
	}
}
