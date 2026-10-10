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
// (ysx/hbposv10 = 27304,hbposepro = 44316)。
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

// TestDeployedIncrementalFallbackCoversNewItems 锁住增量回退列。
//
// 实测依据(2026-10-10):`modify_date IS NULL` 的语义是"建档后从未被修改过",
// 那 `build_date` 就是这条记录最后一次被写入的时间。按
// `build_date >= DATEADD(day,-7,GETDATE())` 筛出的 45 个近 7 天新建商品里,
// **36 个(80%)modify_date 是 NULL**。
//
// 删掉这个回退列的代价是实打实的:30 天窗口从 549 行掉回 410 行,少覆盖 34%
// 的新增商品 —— 而这些恰恰是最该被拉到的(旧数据早被上一次全量带进来了)。
//
// ⚠️ 它**不能替代夜间全量校准**:本库仍有 31 行商品两个时间列同时为 NULL。
func TestDeployedIncrementalFallbackCoversNewItems(t *testing.T) {
	cfg := loadDeployedConfig(t)

	fb := cfg.Refresh.IncrementalFallback["product"]
	if fb != "build_date" {
		t.Fatalf("product 的增量回退列应是 build_date,实际 %q —— 缺了它,新建商品"+
			"(实测 80%% 是 modify_date NULL)在建成当天就查不到", fb)
	}

	// 顺带确认两件事:拼写没写错(过不了白名单就是错的列名),
	// 以及 COALESCE 的**参数顺序**正确(写反了 SQL 照样能跑,只是语义反了)。
	expr, err := source.IncrementalCols{
		Primary:  cfg.Refresh.Incremental["product"],
		Fallback: fb,
	}.Expression()
	if err != nil {
		t.Fatalf("回退列没通过标识符校验(可能是拼错了列名): %v", err)
	}
	if expr != "COALESCE([modify_date], [build_date])" {
		t.Errorf("增量表达式应是 COALESCE([modify_date], [build_date]),实际 %q —— "+
			"顺序反了会让 COALESCE 优先取 build_date", expr)
	}
}
