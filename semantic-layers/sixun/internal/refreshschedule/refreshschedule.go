// Package refreshschedule 决定"谁来定时重拉 duck 数据",并在两种机制之间安全切换。
//
// # 两种机制
//
//	dapr-job 优先:把刷新登记到 Dapr Scheduler,触发时 sidecar POST app 的
//	                /job/<name>。优点是跨进程重启存活、可 `dapr scheduler list`
//	                统一查看、多个门店实例集中管控。
//	ticker 回退:进程内 time.Ticker。scheduler 不可用时的保底。
//
// # 为什么必须有回退,而不是"登记失败就报错退出"
//
// Dapr Jobs 是 alpha 能力,且它的可用性取决于集群里 Scheduler 服务与 sidecar 的连通性。
// 2026-10-10 实测:sidecar 被指向 `localhost:50006` 时,Jobs API 调用**直接挂死**
// (不是 404,是超时),表现为「登记没成功但也没报错」。
// 如果那时候已经切到纯 dapr job,结果就是 **数据永远不刷新、healthz 全绿** ——
// 这个项目栽过太多次这种"静默停摆"。
// 所以:登记成功才切 job,失败就保留 ticker,并把 mode 写进 healthz 让人看得见。
//
// # 两个 job:增量为主 + 夜间全量校准
//
//	主 job(cube-refresh)      @every 30m         只拉水位之后的变更行
//	校准 job(cube-refresh-full) 0 0 3 * * *       夜间 3 点全量重建
//
// 增量天然会漏(写库不更新 modify_date、新建商品 modify_date 为 NULL ——
// 实测 ysx 近 7 天新建商品 80% 是 NULL),所以必须有周期性全量兜底。
//
// 只有**主 job**决定 mode 与 ticker:校准 job 登记失败不会让数据停止刷新,
// 它只是丢了"定期校准"这一层保险,这件事在 healthz 的 jobs 段可见。
// 把两者混成一个 mode 会让"全量校准没登记上"变成"整个刷新退化成 ticker",
// 那是用一个保险的缺失换另一个更大的缺失。
//
// # 为什么需要 in-flight 守卫
//
// ticker 的语义是"上一次跑完才等下一次",天然串行。
// Dapr jobs 的语义是 **at-least-once 且不保证及时**:如果一次刷新跑了 40 分钟而间隔是 30 分钟,
// 下一次触发会**与它重叠**,两个 goroutine 同时往同一个 DuckDB 写。
// 守卫把重叠变成"跳过本次",并在日志和 healthz 里留痕。
//
// 判据:宁可少刷新一次(下个周期会补上),也不要并发写。
package refreshschedule

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"
)

// Mode 是当前的刷新驱动方式。
type Mode string

const (
	// ModeDisabled 表示 interval<=0 或没有可兜底的 job,刻意关闭刷新。
	ModeDisabled Mode = "disabled"
	// ModeDaprJob 表示主 job 已由 Dapr Scheduler 驱动。
	ModeDaprJob Mode = "dapr-job"
	// ModeTicker 表示主 job 登记失败,回退到进程内 ticker。
	ModeTicker Mode = "ticker"
)

// DefaultJobName 是增量刷新 job 名。
//
// dapr 会按 `app/<app-id>/<job-name>` 给 job 命名空间,所以同一个名字在不同
// 门店实例(app-id 不同)之间不会互相覆盖。
const DefaultJobName = "cube-refresh"

// DefaultFullJobName 是夜间全量校准 job 名。
const DefaultFullJobName = "cube-refresh-full"

// DefaultFullSchedule 是默认的全量校准时刻:每天 03:00:00。
//
// ⚠️ **必须是 6 字段 cron(秒 分 时 日 月 周)**。Dapr 用 systemd 风格解析器,
// 实测(2026-10-10 在 gyy 上对 sidecar 登记验证)字段数不对会被直接拒绝:
//
//	schedule="0 3 * * *"    → HTTP 500
//	  "job failed validation: expected exactly 6 fields, found 5"
//	schedule="0 0 3 * * *"  → HTTP 204
//
// 这个拒绝是**静默的**:主 job(@every 30m)不受影响,mode 照样是 dapr-job,
// healthz 整体健康 —— 但夜间全量校准从此再也不会发生,增量漏掉的数据
// 永远没有兜底。所以除了写对常量,还有 validateSchedule 在登记前拦一道。
//
// 为什么不用 "@every 24h":那是"从登记那一刻起每 24 小时",进程重启就漂移,
// 永远对不上"夜间 3 点"这个语义。
const DefaultFullSchedule = "0 0 3 * * *"

// cronFields 是 Dapr 要求的 cron 字段数(systemd 风格:秒 分 时 日 月 周)。
const cronFields = 6

// validateSchedule 在**发请求之前**校验 schedule,让配错当场变成一条明确的
// 错误日志,而不是一个要人去翻 sidecar 才知道的 500。
//
// 只检查 cron 的字段数 —— 字段语义(range / step / 名字)交给 Dapr 判,
// 我们不重复实现一个半个 cron 解析器。以 "@" 开头的是周期表达式
// (@every 30m / @daily),字段数不适用,直接放行。
func validateSchedule(schedule string) error {
	s := strings.TrimSpace(schedule)
	if s == "" {
		return errors.New("schedule 为空")
	}
	if strings.HasPrefix(s, "@") {
		return nil
	}
	if n := len(strings.Fields(s)); n != cronFields {
		return fmt.Errorf("cron 表达式必须是 %d 字段(秒 分 时 日 月 周),实际 %d 个: %q —— "+
			"Dapr 会直接拒绝(实测 HTTP 500: expected exactly 6 fields)", cronFields, n, s)
	}
	return nil
}

// ValidateSchedule 是 validateSchedule 的导出形式。
//
// 给 cmd 包在装配阶段校验配置用 —— 让「配置里的 cron 字段数」在**测试里**
// 就能断言,而不必等到进程起来、请求发出去才从 sidecar 的 500 里发现。
func ValidateSchedule(schedule string) error { return validateSchedule(schedule) }

// JobSpec 是一个要登记到 Scheduler 的刷新 job。
type JobSpec struct {
	// Name 是 job 名,也会成为 app 侧端点的最后一段(/job/<name>)。
	Name string
	// Schedule 交给 Dapr 原样解析:支持 "@every 30m" 这类周期表达式,
	// 也支持 6 字段 cron("0 0 3 * * *")。登记前会被 validateSchedule 拦一道。
	Schedule string
	// Reload 是被触发时执行的动作(全量或增量由调用方闭包决定)。
	Reload func() []string
	// Fallback 标记"这是主 job":只有主 job 登记失败才回退 ticker,
	// 也只有主 job 的触发能判定 Scheduler 已恢复。
	Fallback bool
}

// JobStatus 是单个 job 的对外可观测状态。
//
// 单独暴露每一个 job,是因为它们会**独立失败**:校准 job 没登记上不会影响
// 数据刷新,但"没有定期校准"这件事必须看得见 —— 否则增量漏的数据会一直漏,
// 而没人知道为什么。
type JobStatus struct {
	Name       string `json:"name"`
	Schedule   string `json:"schedule"`
	Registered bool   `json:"registered"`
	Triggered  int64  `json:"triggered"`
	Skipped    int64  `json:"skipped_overlap"`
	Err        string `json:"error,omitempty"`
}

// jobRuntime 是一个 job 的运行时状态。
type jobRuntime struct {
	spec      JobSpec
	failed    bool
	err       string
	triggered atomic.Int64
	skipped   atomic.Int64
}

// Controller 管理刷新触发,并暴露 /job/<name> 端点。
type Controller struct {
	// mode 用 atomic.Value 存 Mode。
	//
	// 为什么不能是普通字段:job handler(被 sidecar 的 HTTP 请求触发的 goroutine)
	// 与 ticker goroutine 会**并发**读写它 —— 前者要在收到触发时切回 dapr-job,
	// 后者要读它决定是否启动。普通字段在这种访问模式下是数据竞争。
	mode atomic.Value // Mode

	// mu 保护 tickerStop / scheduleErr 这组"要成对读写"的字段。
	mu          sync.Mutex
	tickerStop  context.CancelFunc
	scheduleErr string

	// inflight 是本 Controller 的在途锁。**必须挂在实例上,不能用包级变量** ——
	// 包级 mutex 在测试里跨用例共享,一个用例卡住会让另一个永远拿不到锁,
	// 而且这种耦合在单实例时看不出来。
	inflight sync.Mutex

	jobs     map[string]*jobRuntime
	primary  string // 主 job 名
	interval time.Duration

	reload func() []string // 主 job 的动作,ticker 用
	lg     *log.Logger
}

// Stats 是对外可观测的运行统计。
type Stats struct {
	Mode        Mode        `json:"mode"`
	JobName     string      `json:"job_name,omitempty"`
	Triggered   int64       `json:"triggered"`
	Skipped     int64       `json:"skipped_overlap"`
	ScheduleErr string      `json:"schedule_error,omitempty"`
	Jobs        []JobStatus `json:"jobs,omitempty"`
}

// Setup 决定刷新由谁驱动。
//
// sidecarAddr 传空字符串时直接退 ticker(本地裸跑、没有 dapr 的场景)。
// jobs 为空时返回 ModeDisabled,不登记任何东西。
func Setup(ctx context.Context, sidecarAddr string, jobs []JobSpec, lg *log.Logger) *Controller {
	c := &Controller{
		jobs: map[string]*jobRuntime{},
		lg:   lg,
	}
	c.mode.Store(ModeDisabled)

	if len(jobs) == 0 {
		lg.Info("refresh disabled (no jobs configured)")
		return c
	}

	var primary *jobRuntime
	for _, s := range jobs {
		if strings.TrimSpace(s.Name) == "" {
			lg.Info("跳过没有名字的 job")
			continue
		}
		if _, dup := c.jobs[s.Name]; dup {
			lg.Info("重复的 job 名,后者忽略", "job", s.Name)
			continue
		}
		rt := &jobRuntime{spec: s}
		c.jobs[s.Name] = rt
		if s.Fallback {
			if primary != nil {
				lg.Info("只能有一个主 job,后续的忽略", "job", s.Name)
				continue
			}
			primary = rt
		}
	}
	if len(c.jobs) == 0 {
		lg.Info("refresh disabled (所有 job 都没有可用名字)")
		return c
	}
	if primary == nil {
		// 没有主 job = 没有 ticker 兜底的对象。此时若登记成功仍可工作,
		// 但一旦 Scheduler 不可用就没有任何刷新,必须让这件事显式可见。
		lg.Info("没有任何 job 标记为 Fallback,Scheduler 不可用时不会回退 ticker",
			"jobs", len(c.jobs))
	}

	// ticker 间隔从主 job 的 schedule 里解析。解析不出来(比如主 job 用的是
	// cron 而非 @every)就没有进程内兜底 —— 这时 mode 会停在 disabled,
	// 而不是用一个猜出来的间隔去刷。
	c.interval = parseEvery(func() string {
		if primary != nil {
			return primary.spec.Schedule
		}
		return ""
	}())
	c.primary = ""
	if primary != nil {
		c.primary = primary.spec.Name
		c.reload = primary.spec.Reload
	}

	if sidecarAddr == "" {
		if primary != nil {
			c.setMode(ModeTicker, "无 sidecar 地址")
		}
		lg.Info("no sidecar address, falling back to ticker",
			"every", c.interval.String())
		return c
	}

	// 登记给 scheduler。**必须给短超时**:sidecar 连不上 scheduler 时
	// jobs API 会一直挂着(实测 2026-10-10),不设 deadline 这里会卡住整个启动。
	schedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client := daprclient.NewJobsClient(sidecarAddr)
	registered := 0
	for _, rt := range c.jobs {
		// 先本地校验再发请求:字段数不对时 Dapr 只会回一个 500,
		// 而校准 job 登记失败**不影响 mode**(主 job 仍然 dapr-job),
		// 于是"夜间没有全量校准了"这件事会被 healthz 整体健康掩盖。
		// 在这里拦一道,至少日志直指原因,不用人去翻 sidecar。
		if err := validateSchedule(rt.spec.Schedule); err != nil {
			rt.failed = true
			rt.err = err.Error()
			lg.Info("dapr job schedule 非法,跳过登记",
				"job", rt.spec.Name, "schedule", rt.spec.Schedule, "err", err.Error())
			continue
		}
		err := client.ScheduleJob(schedCtx, rt.spec.Name, daprclient.ScheduleJobRequest{
			Schedule: rt.spec.Schedule,
			Data:     rt.spec.Name,
		})
		if err != nil {
			rt.failed = true
			rt.err = err.Error()
			lg.Info("dapr job 登记失败",
				"job", rt.spec.Name, "schedule", rt.spec.Schedule, "err", err.Error())
			continue
		}
		registered++
		lg.Info("dapr job 已登记,刷新由 Scheduler 驱动",
			"job", rt.spec.Name, "schedule", rt.spec.Schedule, "sidecar", sidecarAddr)
	}

	// mode 只看主 job:校准 job 登记失败不该让整个刷新退化成 ticker。
	switch {
	case primary == nil:
		if registered > 0 {
			c.setMode(ModeDaprJob, "")
		}
	case primary.failed:
		c.setMode(ModeTicker, primary.err)
	default:
		c.setMode(ModeDaprJob, "")
	}
	return c
}

// parseEvery 从 "@every 30m" 解析出间隔;不是该形式时返回 0。
func parseEvery(schedule string) time.Duration {
	s := strings.TrimSpace(schedule)
	if !strings.HasPrefix(s, "@every ") {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(s, "@every")))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// setMode 原子地切换驱动方式,并同步 scheduleErr。
//
// 两者**必须成对写**:mode=ModeDaprJob 意味着"调度器已经接管",
// 此时 scheduleErr 留着"上次登记失败:xxx"就是自相矛盾 ——
// healthz 会同时说"现在是 job 模式"和"登记失败过",看的人只能自己猜哪个是真的。
func (c *Controller) setMode(m Mode, scheduleErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scheduleErr = scheduleErr
	c.mode.Store(m)
}

// promoteToDaprJob 在**收到主 job 的触发**时把驱动切回 dapr-job 并停掉 ticker。
//
// # 为什么 job 触发就是"scheduler 已恢复"的权威信号
//
// Dapr 官方文档(jobs-overview / scheduler concepts)确认了三件事:
//  1. job 定义持久化在 Scheduler 的内嵌 etcd 里,跨重启存活;
//  2. 触发时找不到可用 sidecar,job 会进 staging queue,等 sidecar 可用后自动补投;
//  3. 触发最终由 sidecar POST 到 app 的 /job/<name>。
//
// 所以"app 收到了自己的 job"同时证明了:Scheduler 活着、etcd 数据完好、sidecar 在线。
// 这比轮询 `GET /jobs/<name>` 更强 —— 轮询只能**猜**"应该恢复了",
// 而 job 触发是**确实**恢复了。用真实信号替代轮询,顺带省掉一整套重试状态机。
//
// ⚠️ 只认**主** job 的触发:校准 job 的触发不能作为依据,因为它可能压根没登记上。
// 把没登记的 job 的触发当恢复信号,会让主 job 的 ticker 在校准 job 恢复前就停掉。
//
// # 幂等与顺序
//
// mode 已是 dapr-job 时直接返回,热路径零额外开销。
// 停 ticker 与置 mode 在同一把锁里,中间不会有新的 ticker 触发插进来。
func (c *Controller) promoteToDaprJob() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if m, _ := c.mode.Load().(Mode); m != ModeTicker {
		return
	}
	c.scheduleErr = ""
	c.mode.Store(ModeDaprJob)
	if c.tickerStop != nil {
		c.tickerStop()
		c.tickerStop = nil
	}
	c.lg.Info("收到 dapr job 触发,判定 Scheduler 已恢复,切回 dapr-job 并停止 ticker",
		"job", c.primary)
}

// Mode 返回当前驱动方式。
func (c *Controller) Mode() Mode {
	m, _ := c.mode.Load().(Mode)
	return m
}

// Stats 返回可观测统计。
func (c *Controller) Stats() Stats {
	c.mu.Lock()
	err := c.scheduleErr
	c.mu.Unlock()

	st := Stats{
		Mode:        c.Mode(),
		JobName:     c.primary,
		ScheduleErr: err,
		Jobs:        make([]JobStatus, 0, len(c.jobs)),
	}
	var triggered, skipped int64
	for _, rt := range c.jobs {
		t, s := rt.triggered.Load(), rt.skipped.Load()
		triggered += t
		skipped += s
		st.Jobs = append(st.Jobs, JobStatus{
			Name:       rt.spec.Name,
			Schedule:   rt.spec.Schedule,
			Registered: !rt.failed,
			Triggered:  t,
			Skipped:    s,
			Err:        rt.err,
		})
	}
	st.Triggered = triggered
	st.Skipped = skipped
	return st
}

// StatsAny 是给 healthz 用的适配器。
//
// 存在原因:boot.HealthHandler 收的是 `func() any`(为了不反向依赖本包),
// 而 Go 的函数类型不变,`func() Stats` **不能**隐式当作 `func() any` 传。
// 在这里包一层,既保住 Stats() 的类型信息(测试里能用),
// 又不用让 boot 依赖本包。
func (c *Controller) StatsAny() any { return c.Stats() }

// Start 在需要时启动进程内 ticker(仅 ModeTicker 且能解析出间隔)。
//
// ModeDaprJob 时这里什么都不做 —— 触发来自 sidecar 的 HTTP 回调。
//
// ticker 必须**可取消**:登记失败后回退到它,而 Scheduler 恢复后靠
// promoteToDaprJob() 停掉它。原来的 goroutine 只认传入的 ctx,
// 运行期没有任何办法停它 —— 那样 ticker 一旦兜底就永远兜底了。
func (c *Controller) Start(ctx context.Context, lg *log.Logger) {
	if c.Mode() != ModeTicker || c.interval <= 0 {
		if c.Mode() == ModeTicker {
			lg.Info("主 job 登记表里不是 @every 形式(或间隔非法),没有进程内兜底",
				"interval", c.interval.String())
		}
		return
	}
	interval := c.interval
	lg.Info("refresh ticker started", "every", interval.String())

	tickCtx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-t.C:
				c.runOnce("ticker")
			}
		}
	}()

	c.mu.Lock()
	// 必须在锁内重新确认 mode:goroutine 启动到加锁之间存在窗口,
	// 期间 promoteToDaprJob 可能已经把模式切走。
	// 那种情况下若还把 cancel 存成 tickerStop,promote 已经执行完了不会再调它,
	// ticker 就永远停不下来 —— 一个只在极窄竞态里出现、且**症状是"停不掉"**
	// 的 bug,比直接崩溃难查得多。
	if c.Mode() != ModeTicker {
		c.mu.Unlock()
		cancel()
		return
	}
	c.tickerStop = cancel
	c.mu.Unlock()
}

// JobHandler 返回 POST /job/:jobName 的 handler。
//
// **无论当前是哪种 mode,所有 job 的端点都要注册**:job 记录存在 Scheduler 的
// etcd 里,跨重启存活。如果这次启动时登记失败(mode=ModeTicker),旧 job 仍然
// 会来触发,没有这个端点就会持续 404,而 Scheduler 侧看到的是"投递失败"。
//
// 而且多 mode 并存是**正确行为**而不是冲突:ticker 保底,dapr job 统一管理,
// 一次刷新跑不掉(在途守卫会跳过重叠的那些),最终仍然是"刷新了"。
func (c *Controller) JobHandler() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		name := ctx.Param("jobName")
		rt, ok := c.jobs[name]
		if !ok {
			// 不是我们的 job。返 404 让 Scheduler 记一笔,不要 200 假装成功。
			ctx.JSON(http.StatusNotFound, gin.H{
				"code":    "JOB_UNKNOWN",
				"message": "unknown job " + name,
			})
			return
		}

		body, _ := io.ReadAll(io.LimitReader(ctx.Request.Body, 4096))
		var ran bool
		if c.tryLock() {
			defer c.unlock()
			// 收到主 job 的触发 = Scheduler 已恢复(见 promoteToDaprJob 的依据)。
			//
			// 顺序:**先切模式再干活**。切换发生在本次刷新开始之前,
			// 于是 ticker 从这一刻起不会再触发,而本次 job 触发照常执行。
			// 反过来(先 reload 再切)会留一个窗口:ticker 在 reload 期间到点,
			// 明明调度器已经接管却仍被在途守卫跳过 —— 白丢一次刷新,
			// 而且日志里只留一句"跳过",没人看得出本该执行。
			if name == c.primary {
				c.promoteToDaprJob()
			}
			rt.triggered.Add(1)
			c.lg.Info("dapr job triggered", "job", name, "payload", string(body))
			rt.spec.Reload()
			ran = true
		} else {
			rt.skipped.Add(1)
			c.lg.Info("dapr job 触发时上一次刷新还在跑,跳过本次", "job", name)
		}

		// 一律 200:重叠跳过不是失败,返回错误会让 Scheduler 重试,
		// 而下一个周期本来就会补上 —— 重试只会加剧堆积。
		ctx.JSON(http.StatusOK, gin.H{
			"job":    name,
			"ran":    ran,
			"reason": reasonOf(ran),
		})
	}
}

func reasonOf(ran bool) string {
	if ran {
		return "executed"
	}
	return "previous refresh still in progress"
}

func (c *Controller) tryLock() bool {
	return c.inflight.TryLock()
}

func (c *Controller) unlock() {
	c.inflight.Unlock()
}

func (c *Controller) runOnce(source string) {
	if !c.tryLock() {
		c.lg.Info("刷新触发时上一次还在跑,跳过本次", "source", source)
		return
	}
	defer c.unlock()
	if c.reload != nil {
		c.reload()
	}
}
