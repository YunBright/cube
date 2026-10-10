package refreshschedule

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/cube/pkg/log"
)

func testLogger() *log.Logger { return log.New("test") }

func noopReload() func() []string { return func() []string { return nil } }

// primaryJob 构造主 job(增量),它决定 mode 与 ticker 兜底。
func primaryJob(reload func() []string) JobSpec {
	return JobSpec{
		Name:     DefaultJobName,
		Schedule: "@every 30m",
		Reload:   reload,
		Fallback: true,
	}
}

// fullJob 构造夜间全量校准 job。
func fullJob(reload func() []string) JobSpec {
	return JobSpec{
		Name:     DefaultFullJobName,
		Schedule: DefaultFullSchedule,
		Reload:   reload,
	}
}

// sidecarProbe 记录收到的 job 登记请求。
type sidecarProbe struct {
	mu    sync.Mutex
	calls []ScheduleProbe
}

type ScheduleProbe struct {
	Path      string
	Schedule  string
	Overwrite bool
}

func (p *sidecarProbe) add(c ScheduleProbe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, c)
}

func (p *sidecarProbe) byName(name string) (ScheduleProbe, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.calls {
		if strings.HasSuffix(c.Path, "/"+name) {
			return c, true
		}
	}
	return ScheduleProbe{}, false
}

func (p *sidecarProbe) all() []ScheduleProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ScheduleProbe(nil), p.calls...)
}

// fakeSidecar 返回一个 jobs API stub。
//
// failNames 里的 job 名返回 500,用来模拟"只有部分 job 登记失败"。
func fakeSidecar(t *testing.T, status int, probe *sidecarProbe, failNames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1.0-alpha1/jobs/") && r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sp := ScheduleProbe{}
			sp.Path = r.URL.Path
			sp.Schedule, _ = body["schedule"].(string)
			sp.Overwrite, _ = body["overwrite"].(bool)
			if probe != nil {
				probe.add(sp)
			}
			for _, n := range failNames {
				if strings.HasSuffix(r.URL.Path, "/"+n) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSetup_SchedulesDaprJob 登记成功时必须切到 dapr-job。
func TestSetup_SchedulesDaprJob(t *testing.T) {
	var probe sidecarProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe)

	c := Setup(context.Background(), srv.URL, []JobSpec{primaryJob(noopReload())}, testLogger())

	if c.Mode() != ModeDaprJob {
		t.Fatalf("登记成功应切到 dapr-job,实际 %q", c.Mode())
	}
	got, ok := probe.byName(DefaultJobName)
	if !ok {
		t.Fatalf("应登记到 %s,实际请求 %v", DefaultJobName, probe.all())
	}
	if got.Schedule != "@every 30m" {
		t.Fatalf("schedule 应原样透传,实际 %q", got.Schedule)
	}
	if !got.Overwrite {
		t.Fatal("必须带 overwrite=true,否则第二次启动会因为同名 job 已存在而登记失败")
	}
}

// TestSetup_RegistersBothJobs 增量 + 全量校准两个 job 都要登记。
func TestSetup_RegistersBothJobs(t *testing.T) {
	var probe sidecarProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe)

	c := Setup(context.Background(), srv.URL,
		[]JobSpec{primaryJob(noopReload()), fullJob(noopReload())}, testLogger())

	if c.Mode() != ModeDaprJob {
		t.Fatalf("两个都登记成功应是 dapr-job,实际 %q", c.Mode())
	}
	full, ok := probe.byName(DefaultFullJobName)
	if !ok {
		t.Fatalf("应登记全量校准 job,实际请求 %v", probe.all())
	}
	if full.Schedule != DefaultFullSchedule {
		t.Fatalf("全量 job 的 cron 应原样透传给 Dapr,实际 %q", full.Schedule)
	}
}

// TestSetup_CalibrationJobFailureKeepsDaprJobMode 锁一个关键的取舍。
//
// 校准 job 登记失败**不允许**把整个刷新退化成 ticker:
// 那等于"因为少了一层保险,把主保险也丢了"。
// mode 仍应是 dapr-job(主 job 登记成功了),而失败要在 jobs 段里看得见。
func TestSetup_CalibrationJobFailureKeepsDaprJobMode(t *testing.T) {
	var probe sidecarProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe, DefaultFullJobName)

	c := Setup(context.Background(), srv.URL,
		[]JobSpec{primaryJob(noopReload()), fullJob(noopReload())}, testLogger())

	if c.Mode() != ModeDaprJob {
		t.Fatalf("校准 job 失败不应影响主 job 的 dapr-job 模式,实际 %q", c.Mode())
	}
	var full JobStatus
	for _, j := range c.Stats().Jobs {
		if j.Name == DefaultFullJobName {
			full = j
		}
	}
	if full.Registered {
		t.Fatal("校准 job 应当是未登记状态")
	}
	if full.Err == "" {
		t.Fatal("校准 job 失败原因必须出现在 Stats 里 —— 否则没人知道增量漏的数据为什么没被校准")
	}
}

// TestSetup_FallsBackToTickerWhenSchedulerDown 是这个包存在的全部理由。
//
// scheduler 不可用时**必须**保留 ticker。实测(2026-10-10)scheduler 不可用时
// jobs API 会挂死而不是报错;若那时不兜底,结果就是数据永远不刷新且 healthz 全绿。
func TestSetup_FallsBackToTickerWhenSchedulerDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟"连不上":直接断连接
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			if conn != nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()

	c := Setup(context.Background(), srv.URL, []JobSpec{primaryJob(noopReload())}, testLogger())

	if c.Mode() != ModeTicker {
		t.Fatalf("scheduler 不可用必须回退 ticker,实际 %q", c.Mode())
	}
	if c.Stats().ScheduleErr == "" {
		t.Fatal("回退原因必须留在 Stats 里,否则运行期看不到为什么没走 dapr job")
	}
}

// TestSetup_DisabledWhenNoJobs 一个 job 都没有 = 刻意关闭刷新,不能被当成故障。
func TestSetup_DisabledWhenNoJobs(t *testing.T) {
	var probe sidecarProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe)

	c := Setup(context.Background(), srv.URL, nil, testLogger())
	if c.Mode() != ModeDisabled {
		t.Fatalf("无 job 应为 disabled,实际 %q", c.Mode())
	}
	if len(probe.all()) != 0 {
		t.Fatal("disabled 时不应发起任何 job 登记请求")
	}
}

// TestSetup_NoSidecarAddr_NoDapr 没 dapr(本地裸跑)时回退 ticker,不报错。
func TestSetup_NoSidecarAddr_NoDapr(t *testing.T) {
	c := Setup(context.Background(), "", []JobSpec{primaryJob(noopReload())}, testLogger())
	if c.Mode() != ModeTicker {
		t.Fatalf("无 sidecar 地址应回退 ticker,实际 %q", c.Mode())
	}
}

// TestSetup_CronPrimaryHasNoTickerFallback 主 job 若是 cron 形式,
// 就无法解析出"每隔多久跑一次",此时不能凭空猜一个间隔去兜底。
func TestSetup_CronPrimaryHasNoTickerFallback(t *testing.T) {
	var probe sidecarProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe)

	c := Setup(context.Background(), srv.URL, []JobSpec{{
		Name: DefaultJobName, Schedule: "0 3 * * *", Reload: noopReload(), Fallback: true,
	}}, testLogger())

	if c.interval != 0 {
		t.Fatalf("cron 形式不应解析出 ticker 间隔,实际 %v", c.interval)
	}
	c.Start(context.Background(), testLogger()) // 不应 panic 也不应启动 ticker
}

// TestJobHandler_UnknownJobIs404 不是我们的 job 必须 404,
// 返 200 等于假装收到了 —— Scheduler 侧就看不到"投递失败"。
func TestJobHandler_UnknownJobIs404(t *testing.T) {
	c := Setup(context.Background(), "", nil, testLogger())
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/job/:jobName", c.JobHandler())

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/job/somebody-elses-job", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("未知 job 应 404,实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestJobHandler_DispatchesPerJob 每个 job 必须调用它自己的 Reload。
//
// 两个 job 走同一个 Reload 就等于夜间校准退化成"再做一次增量",
// 而它的表现是"一切正常、只是漏的数据永远补不上"。
func TestJobHandler_DispatchesPerJob(t *testing.T) {
	var incr, full atomic.Int64
	c := Setup(context.Background(), "",
		[]JobSpec{
			primaryJob(func() []string { incr.Add(1); return nil }),
			fullJob(func() []string { full.Add(1); return nil }),
		}, testLogger())

	doJob(t, c, DefaultJobName)
	if incr.Load() != 1 || full.Load() != 0 {
		t.Fatalf("触发增量 job 应只跑增量:incr=%d full=%d", incr.Load(), full.Load())
	}

	doJob(t, c, DefaultFullJobName)
	if incr.Load() != 1 || full.Load() != 1 {
		t.Fatalf("触发全量 job 应只跑全量:incr=%d full=%d", incr.Load(), full.Load())
	}
}

// TestJobHandler_RunsReload 正常触发要真的执行 reload。
func TestJobHandler_RunsReload(t *testing.T) {
	var n atomic.Int64
	c := Setup(context.Background(), "",
		[]JobSpec{primaryJob(func() []string { n.Add(1); return nil })}, testLogger())

	w := doJob(t, c, DefaultJobName)

	if w.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d body=%s", w.Code, w.Body.String())
	}
	if n.Load() != 1 {
		t.Fatalf("应执行 1 次 reload,实际 %d", n.Load())
	}
	if c.Stats().Triggered != 1 {
		t.Fatalf("triggered 计数应为 1,实际 %d", c.Stats().Triggered)
	}
}

// TestJobHandler_SkipsOverlap 是 dapr jobs 语义带来的**新风险**的守卫:
// jobs 是 at-least-once 且不保证及时,一次慢刷新会和下一次触发重叠,
// 两个 goroutine 同时写同一个 DuckDB。必须跳过而不是排队。
func TestJobHandler_SkipsOverlap(t *testing.T) {
	var runs atomic.Int64
	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	c := Setup(context.Background(), "", []JobSpec{primaryJob(func() []string {
		runs.Add(1)
		entered <- struct{}{} // 通知"我进来了"
		<-release             // 卡住,制造重叠窗口
		return nil
	})}, testLogger())

	// 第一次:占住锁不放
	go doJob(t, c, DefaultJobName)
	<-entered // 确认第一次正在跑

	// 第二次:必须被跳过
	w := doJob(t, c, DefaultJobName)
	if w.Code != http.StatusOK {
		t.Fatalf("重叠时应仍返回 200(不是失败),实际 %d", w.Code)
	}
	if runs.Load() != 1 {
		t.Fatalf("重叠时不得并发执行第二次 reload,实际执行了 %d 次", runs.Load())
	}
	st := c.Stats()
	if st.Skipped != 1 {
		t.Fatalf("skipped 计数应为 1,实际 %d", st.Skipped)
	}
	if !strings.Contains(w.Body.String(), "previous refresh still in progress") {
		t.Fatalf("响应应说明为什么跳过,实际 %s", w.Body.String())
	}

	close(release)
}

// TestJobHandler_LockReleasedAfterRun 跑完要释放锁,否则第二次永远进不去。
func TestJobHandler_LockReleasedAfterRun(t *testing.T) {
	var n atomic.Int64
	c := Setup(context.Background(), "",
		[]JobSpec{primaryJob(func() []string { n.Add(1); return nil })}, testLogger())

	doJob(t, c, DefaultJobName)
	doJob(t, c, DefaultJobName)

	if n.Load() != 2 {
		t.Fatalf("顺序两次触发应都执行,实际 %d", n.Load())
	}
}

// TestControllersDoNotShareInflightLock 两个 Controller 的锁必须互相独立。
//
// 用包级 mutex 的话这条会红 —— 单实例时看不出来,测试里互相污染,
// 而且真实场景(将来同进程多实例)会直接出错。
func TestControllersDoNotShareInflightLock(t *testing.T) {
	var a, b atomic.Int64
	ca := Setup(context.Background(), "",
		[]JobSpec{primaryJob(func() []string { a.Add(1); return nil })}, testLogger())
	cb := Setup(context.Background(), "",
		[]JobSpec{primaryJob(func() []string { b.Add(1); return nil })}, testLogger())

	if !ca.tryLock() {
		t.Fatal("A 应能拿到自己的锁")
	}
	if !cb.tryLock() {
		t.Fatal("B 的锁被 A 占了 —— 两者共用了一个全局锁")
	}
	ca.unlock()
	cb.unlock()
}

func doJob(t *testing.T, c *Controller, name string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/job/:jobName", c.JobHandler())

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/job/"+name, strings.NewReader(`{"data":"cube-refresh"}`)))
	return w
}

// 并发触发压一下:守卫在真实并发下的行为。
func TestJobHandler_ConcurrentTriggers(t *testing.T) {
	var n atomic.Int64
	var wg sync.WaitGroup
	c := Setup(context.Background(), "", []JobSpec{primaryJob(func() []string {
		n.Add(1)
		time.Sleep(2 * time.Millisecond)
		return nil
	})}, testLogger())

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); doJob(t, c, DefaultJobName) }()
	}
	wg.Wait()

	if n.Load() == 0 {
		t.Fatal("并发下至少应执行一次")
	}
	if got := c.Stats().Triggered; got != n.Load() {
		t.Fatalf("triggered(%d) 应与实际执行次数(%d)一致", got, n.Load())
	}
}

// ---------------------------------------------------------------------------
// cron 字段数(Dapr 用 systemd 风格,严格 6 字段)
//
// 背景:2026-10-10 真机验证时,DefaultFullSchedule 原本写成 "0 3 * * *"(5 字段)。
// Dapr 直接拒绝:HTTP 500 "expected exactly 6 fields, found 5"。
// **最毒的地方在于它几乎不留痕**:主 job 是 @every 30m,不受影响,
// mode 照样是 dapr-job,healthz 整体健康 —— 但夜间全量校准再也不会发生,
// 增量漏掉的数据永远没有兜底,直到某天有人发现商品查不出来了。
// 所以下面三条测试守的是"这类静默降级不许再发生"。
// ---------------------------------------------------------------------------

// TestDefaultFullScheduleHasSixFields 锁住默认常量本身。
//
// 断言字段数而不是断言字符串:写成 "0 0 3 * * *" 还是 "0 3 * * * *"(后者是
// 每小时第 3 分)语义完全不同,但两者字段数都是 6,这里只负责拦"少一个字段"。
func TestDefaultFullScheduleHasSixFields(t *testing.T) {
	if n := len(strings.Fields(DefaultFullSchedule)); n != 6 {
		t.Fatalf("DefaultFullSchedule 必须是 6 字段 cron(秒 分 时 日 月 周),实际 %d 个: %q —— "+
			"Dapr 会直接拒绝(实测 HTTP 500: expected exactly 6 fields)", n, DefaultFullSchedule)
	}
	if err := validateSchedule(DefaultFullSchedule); err != nil {
		t.Fatalf("默认 schedule 自己就应该通过校验: %v", err)
	}
}

// TestDefaultFullScheduleIsThreeAM 锁住默认时刻真的是凌晨 3 点。
//
// 这是上面那条**抓不到**的那一半:6 字段全对,但顺序写错("0 3 * * * *"
// = 每小时第 3 分)同样会让"夜间校准"变成"每小时全量",把优化彻底抵消。
func TestDefaultFullScheduleIsThreeAM(t *testing.T) {
	f := strings.Fields(DefaultFullSchedule)
	if f[0] != "0" {
		t.Errorf("第 1 字段是秒,应为 0(整分触发),实际 %q", f[0])
	}
	if f[1] != "0" {
		t.Errorf("第 2 字段是分,应为 0(整点触发),实际 %q —— 非 0 会变成每小时第 N 分全量", f[1])
	}
	if f[2] != "3" {
		t.Errorf("第 3 字段是时,应为 3(夜间 3 点),实际 %q", f[2])
	}
}

// TestValidateSchedule 覆盖纯校验函数的各种输入。
func TestValidateSchedule(t *testing.T) {
	ok := []struct{ name, sched string }{
		{"六字段每日", "0 0 3 * * *"},
		{"六字段工作日", "0 15 3 * * 1-5"},
		{"周期表达式", "@every 30m"},
		{"日别", "@daily"},
		{"首尾空白", "  0 0 3 * * *  "},
	}
	for _, tc := range ok {
		if err := validateSchedule(tc.sched); err != nil {
			t.Errorf("%s(%q) 应通过校验,实际: %v", tc.name, tc.sched, err)
		}
	}

	bad := []struct{ name, sched string }{
		{"五字段(经典 cron 写法)", "0 3 * * *"},
		{"七字段", "0 0 0 3 * * *"},
		{"只有小时", "3"},
		{"空", ""},
		{"纯空白", "   "},
	}
	for _, tc := range bad {
		if err := validateSchedule(tc.sched); err == nil {
			t.Errorf("%s(%q) 应被拒绝,Dapr 会返 HTTP 500", tc.name, tc.sched)
		}
	}
}

// TestSetup_RejectsBadCronBeforeCallingSidecar 锁住"发请求前就拦"。
//
// 断言两件事:①没向 sidecar 发请求(不发无用的、注定 500 的调用);
// ②该 job 的 registered=false 且带原因。
//
// 之所以要单独测这条:即使 validateSchedule 存在,也可能哪天有人在它之后
// 又加了请求 —— 那时候这个校验就变成了永远不生效的死代码。
func TestSetup_RejectsBadCronBeforeCallingSidecar(t *testing.T) {
	var probe sidecarProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe)

	badFull := fullJob(noopReload())
	badFull.Schedule = "0 3 * * *" // 经典 5 字段写法

	c := Setup(context.Background(), srv.URL, []JobSpec{primaryJob(noopReload()), badFull}, testLogger())

	if _, ok := probe.byName(DefaultFullJobName); ok {
		t.Error("schedule 非法时不该向 sidecar 发登记请求(它只会回 500)")
	}
	if _, ok := probe.byName(DefaultJobName); !ok {
		t.Errorf("主 job 与校准 job 相互独立,校准被拦不应影响主 job,实际请求: %v", probe.all())
	}

	var found bool
	for _, js := range c.Stats().Jobs {
		if js.Name != DefaultFullJobName {
			continue
		}
		found = true
		if js.Registered {
			t.Error("schedule 非法的校准 job 不应显示为 registered")
		}
		if js.Err == "" {
			t.Error("被拦下的 job 必须带原因,否则 healthz 上只是一个没头没尾的 false")
		}
		if !strings.Contains(js.Err, "6 字段") {
			t.Errorf("原因应直指字段数问题,实际: %q", js.Err)
		}
	}
	if !found {
		t.Fatalf("healthz 的 jobs 段必须列出这个校准 job,否则它静默消失,实际: %v", c.Stats().Jobs)
	}
}
