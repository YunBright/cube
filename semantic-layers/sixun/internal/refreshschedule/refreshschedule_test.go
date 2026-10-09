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

// fakeSidecar 记录收到的 job 登记请求。
func fakeSidecar(t *testing.T, status int, got *ScheduleProbe) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1.0-alpha1/jobs/") && r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if got != nil {
				got.Path = r.URL.Path
				got.Schedule, _ = body["schedule"].(string)
				got.Overwrite, _ = body["overwrite"].(bool)
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type ScheduleProbe struct {
	Path      string
	Schedule  string
	Overwrite bool
}

// TestSetup_SchedulesDaprJob 登记成功时必须切到 dapr-job,并用 @every 表达间隔。
func TestSetup_SchedulesDaprJob(t *testing.T) {
	var probe ScheduleProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe)

	c := Setup(context.Background(), srv.URL, 30*time.Minute, noopReload(), testLogger())

	if c.Mode() != ModeDaprJob {
		t.Fatalf("登记成功应切到 dapr-job,实际 %q", c.Mode())
	}
	if !strings.HasSuffix(probe.Path, "/cube-refresh") {
		t.Fatalf("应登记到 cube-refresh,实际 %q", probe.Path)
	}
	if probe.Schedule != "@every 30m0s" {
		t.Fatalf("schedule 应为 @every 30m0s,实际 %q", probe.Schedule)
	}
	if !probe.Overwrite {
		t.Fatal("必须带 overwrite=true,否则第二次启动会因为同名 job 已存在而登记失败")
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

	c := Setup(context.Background(), srv.URL, 30*time.Minute, noopReload(), testLogger())

	if c.Mode() != ModeTicker {
		t.Fatalf("scheduler 不可用必须回退 ticker,实际 %q", c.Mode())
	}
	st := c.Stats()
	if st.ScheduleErr == "" {
		t.Fatal("回退原因必须留在 Stats 里,否则运行期看不到为什么没走 dapr job")
	}
}

// TestSetup_DisabledWhenIntervalZero interval<=0 是"刻意关闭刷新",不能被当成故障。
func TestSetup_DisabledWhenIntervalZero(t *testing.T) {
	var probe ScheduleProbe
	srv := fakeSidecar(t, http.StatusNoContent, &probe)

	c := Setup(context.Background(), srv.URL, 0, noopReload(), testLogger())
	if c.Mode() != ModeDisabled {
		t.Fatalf("interval<=0 应为 disabled,实际 %q", c.Mode())
	}
	if probe.Path != "" {
		t.Fatal("disabled 时不应发起任何 job 登记请求")
	}
}

// TestSetup_NoSidecarAddr_NoDapr 没 dapr(本地裸跑)时回退 ticker,不报错。
func TestSetup_NoSidecarAddr_NoDapr(t *testing.T) {
	c := Setup(context.Background(), "", 30*time.Minute, noopReload(), testLogger())
	if c.Mode() != ModeTicker {
		t.Fatalf("无 sidecar 地址应回退 ticker,实际 %q", c.Mode())
	}
}

// TestJobHandler_UnknownJobIs404 不是我们的 job 必须 404,
// 返 200 等于假装收到了 —— Scheduler 侧就看不到"投递失败"。
func TestJobHandler_UnknownJobIs404(t *testing.T) {
	c := Setup(context.Background(), "", 0, noopReload(), testLogger())
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/job/:jobName", c.JobHandler())

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/job/somebody-elses-job", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("未知 job 应 404,实际 %d body=%s", w.Code, w.Body.String())
	}
}

// TestJobHandler_RunsReload 正常触发要真的执行 reload。
func TestJobHandler_RunsReload(t *testing.T) {
	var n atomic.Int64
	c := Setup(context.Background(), "", 0, func() []string { n.Add(1); return nil }, testLogger())

	w := doJob(t, c, "cube-refresh")

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

	c := Setup(context.Background(), "", 0, func() []string {
		runs.Add(1)
		entered <- struct{}{} // 通知"我进来了"
		<-release             // 卡住,制造重叠窗口
		return nil
	}, testLogger())

	// 第一次:占住锁不放
	go doJob(t, c, "cube-refresh")
	<-entered // 确认第一次正在跑

	// 第二次:必须被跳过
	w := doJob(t, c, "cube-refresh")
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
	c := Setup(context.Background(), "", 0, func() []string { n.Add(1); return nil }, testLogger())

	doJob(t, c, "cube-refresh")
	doJob(t, c, "cube-refresh")

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
	ca := Setup(context.Background(), "", 0, func() []string { a.Add(1); return nil }, testLogger())
	cb := Setup(context.Background(), "", 0, func() []string { b.Add(1); return nil }, testLogger())

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
	c := Setup(context.Background(), "", 0, func() []string {
		n.Add(1)
		time.Sleep(2 * time.Millisecond)
		return nil
	}, testLogger())

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); doJob(t, c, "cube-refresh") }()
	}
	wg.Wait()

	if n.Load() == 0 {
		t.Fatal("并发下至少应执行一次")
	}
	if got := c.Stats().Triggered; got != n.Load() {
		t.Fatalf("triggered(%d) 应与实际执行次数(%d)一致", got, n.Load())
	}
}
