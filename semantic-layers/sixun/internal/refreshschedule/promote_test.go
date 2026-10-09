package refreshschedule

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/cube/pkg/log"
)

// ---- job 触发 = "Scheduler 已恢复"的权威信号 ----
//
// Dapr 官方文档确认的三件事(jobs-overview / scheduler concepts):
//   1. job 定义持久化在 Scheduler 的内嵌 etcd,跨重启存活;
//   2. 触发时找不到可用 sidecar → 进 staging queue,等 sidecar 可用后自动补投;
//   3. 触发最终由 sidecar POST 到 app 的 /job/<name>。
//
// 所以 app 收到自己的 job,同时证明 Scheduler 活着、etcd 数据完好、sidecar 在线。
// 因此**收到触发就该切回 dapr-job 并停 ticker**,而不是靠轮询去猜它恢复了。
//
// 不切的后果不是"多一个保险",而是:job 记录可能还留在 etcd 里,
// Scheduler 一恢复它就回来投递 → ticker 与 job 同时刷同一个 DuckDB,
// 刷新频率翻倍,而且 healthz 会说 "mode=ticker" 而 job 其实一直在触发 ——
// 那个字段开始说谎。

func newTickerController(t *testing.T, interval time.Duration, calls *atomic.Int64) *Controller {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// sidecarAddr 传空 = 模拟"登记失败/没有 sidecar"这条路径,直接进 ticker。
	c := Setup(context.Background(), "", interval,
		func() []string {
			calls.Add(1)
			return nil
		}, log.New("test"))
	return c
}

func postJob(c *Controller, name string) {
	engine := gin.New()
	engine.POST("/job/:jobName", c.JobHandler())
	srv := httptest.NewServer(engine)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/job/"+name, strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	resp.Body.Close()
}

// TestTickerModePromotesToDaprJobOnTrigger 是本次改动的核心回归锁。
//
// 反例(2026-10-10 实际行为):启动时登记失败 → 永久停在 ticker,
// Scheduler 恢复后也不会切回去,于是旧 job 回来投递 → 双刷 + healthz 说谎。
func TestTickerModePromotesToDaprJobOnTrigger(t *testing.T) {
	var calls atomic.Int64
	c := newTickerController(t, 50*time.Millisecond, &calls)
	c.Start(context.Background(), 50*time.Millisecond, log.New("test"))

	if got := c.Mode(); got != ModeTicker {
		t.Fatalf("起始 mode = %q, want %q", got, ModeTicker)
	}

	postJob(c, DefaultJobName)

	if got := c.Mode(); got != ModeDaprJob {
		t.Errorf("收到 job 触发后 mode = %q, want %q —— 收到触发即证明 Scheduler 已恢复", got, ModeDaprJob)
	}
	if calls.Load() == 0 {
		t.Error("job 触发本身应该执行一次 reload")
	}
}

// TestScheduleErrClearedOnPromote 锁"mode 与 scheduleErr 必须成对改"。
//
// 只清 mode 不清 err 的话,healthz 会同时显示
// mode=dapr-job 和 schedule_error=<上次的失败原因>,两者自相矛盾,
// 看的人只能自己猜哪个是真的。
func TestScheduleErrClearedOnPromote(t *testing.T) {
	var calls atomic.Int64
	c := newTickerController(t, time.Hour, &calls)

	// 手工塞一个"登记失败"的状态,模拟 Setup 里 ScheduleJob 返回 error 的路径。
	c.setMode(ModeTicker, "dial tcp 127.0.0.1:50006: connect: connection refused")
	if got := c.Stats().ScheduleErr; got == "" {
		t.Fatal("前置条件失败:scheduleErr 应当有值")
	}

	postJob(c, DefaultJobName)

	st := c.Stats()
	if st.Mode != ModeDaprJob {
		t.Errorf("mode = %q, want %q", st.Mode, ModeDaprJob)
	}
	if st.ScheduleErr != "" {
		t.Errorf("切到 dapr-job 后 scheduleErr 应清空,实际仍是 %q", st.ScheduleErr)
	}
}

// TestTickerStopsAfterPromote 锁"停止 ticker"这个动作本身。
//
// 只断言 mode 变了不够 —— mode 是字符串,ticker 那个 goroutine 可能还活着。
// 判据留了 1 次余量:cancel() 是同步关 channel,但 select 在 Done 与已就绪的
// t.C 同时 ready 时会随机挑一个,所以最多泄漏一次执行。真正的缺陷(ticker
// 没停)会让计数持续增长,判据完全拦得住。
func TestTickerStopsAfterPromote(t *testing.T) {
	const interval = 50 * time.Millisecond
	var calls atomic.Int64
	c := newTickerController(t, interval, &calls)
	c.Start(context.Background(), interval, log.New("test"))

	// 先确认 ticker 真的在跑(否则"停了"是因为从没启动过,测试就没意义了)。
	time.Sleep(6 * interval)
	if calls.Load() == 0 {
		t.Fatal("前置条件失败:ticker 应当已经触发过 reload")
	}

	postJob(c, DefaultJobName)
	after := calls.Load()

	// 5 个周期内不应再有增长(ticker 没停的话这里会涨约 5 次)。
	time.Sleep(5 * interval)
	grew := calls.Load() - after

	if grew > 1 {
		t.Errorf("切到 dapr-job 后 ticker 仍在刷新:%d 个周期内多出 %d 次(上限 1,余量给 select 竞态)",
			5, grew)
	}
}

// TestPromoteIsIdempotent 锁热路径:已经是 dapr-job 时收到触发不做任何切换。
//
// 这条不是可有可无的 —— job 每 30 分钟来一次,如果每次都重置 tickerStop /
// 打日志,长时间运行会积累误导性的日志,并且掩盖"谁真正触发了切换"。
func TestPromoteIsIdempotent(t *testing.T) {
	var calls atomic.Int64
	c := newTickerController(t, time.Hour, &calls)
	c.setMode(ModeDaprJob, "")

	postJob(c, DefaultJobName)
	postJob(c, DefaultJobName)

	if got := c.Mode(); got != ModeDaprJob {
		t.Errorf("mode = %q, want %q", got, ModeDaprJob)
	}
	if got := c.Stats().Triggered; got != 2 {
		t.Errorf("triggered = %d, want 2(两次触发都该执行)", got)
	}
}

// TestPromoteStopsUnknownTickerGuard 验证顺序:先切模式再干活。
//
// 如果顺序反过来(reload 完才切),ticker 在 reload 期间到点会被在途守卫跳过,
// 白丢一次刷新且日志只留"跳过"。本测试不构造那个时序,但锁住一个可观察的副产物:
// promote 发生在 reload **之前**,所以 JobHandler 返回时 mode 已是 dapr-job。
func TestPromoteHappensBeforeReload(t *testing.T) {
	var calls atomic.Int64
	var modeDuringReload Mode
	c := newTickerController(t, time.Hour, &calls)

	// 换成能观察 mode 的 reload:包一层记录"reload 执行瞬间的 mode"。
	c.reload = func() []string {
		calls.Add(1)
		modeDuringReload = c.Mode()
		return nil
	}
	c.setMode(ModeTicker, "boom")

	postJob(c, DefaultJobName)

	if modeDuringReload != ModeDaprJob {
		t.Errorf("reload 执行时的 mode = %q, want %q —— 切换必须发生在干活之前,"+
			"否则 ticker 会在 reload 期间触发并被守卫白跳过", modeDuringReload, ModeDaprJob)
	}
}

// TestStartDoesNothingWhenNotTicker 守住 disabled 模式不会被 ticker 意外复活。
func TestStartDoesNothingWhenNotTicker(t *testing.T) {
	var calls atomic.Int64
	c := newTickerController(t, 20*time.Millisecond, &calls)
	c.setMode(ModeDaprJob, "")
	c.Start(context.Background(), 20*time.Millisecond, log.New("test"))

	time.Sleep(120 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("dapr-job 模式下不应启动 ticker,但触发了 %d 次", got)
	}
}
