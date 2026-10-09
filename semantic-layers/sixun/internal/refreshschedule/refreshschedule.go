// Package refreshschedule 决定"谁来定时重拉 duck 数据",并在两种机制之间安全切换。
//
// # 两种机制
//
//	dapr-job 优先:把刷新登记到 Dapr Scheduler,触发时 sidecar POST app 的
//	                /job/cube-refresh。优点是跨进程重启存活、可 `dapr scheduler list`
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
	"io"
	"net/http"
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
	// ModeDisabled 表示 interval<=0,刻意关闭刷新。
	ModeDisabled Mode = "disabled"
	// ModeDaprJob 表示由 Dapr Scheduler 驱动。
	ModeDaprJob Mode = "dapr-job"
	// ModeTicker 表示回退到进程内 ticker。
	ModeTicker Mode = "ticker"
)

// DefaultJobName 是 cube app 的刷新 job 名。
//
// dapr 会按 `app/<app-id>/<job-name>` 给 job 命名空间,所以同一个名字在不同
// 门店实例(app-id 不同)之间不会互相覆盖。
const DefaultJobName = "cube-refresh"

// Controller 管理刷新触发,并暴露 /job/<name> 端点。
type Controller struct {
	// mode 用 atomic.Value 存 Mode。
	//
	// 为什么不能是普通字段:job handler(被 sidecar 的 HTTP 请求触发的 goroutine)
	// 与 ticker goroutine 会**并发**读写它 —— 前者要在收到触发时切回 dapr-job,
	// 后者要读它决定是否启动。普通字段在这种访问模式下是数据竞争,
	// go test -race 会报(本机无 gcc 跑不了 -race,但这不能当作不写对的借口)。
	mode atomic.Value // Mode

	jobName string

	// mu 保护 tickerCancel / scheduleErr 这组"要成对读写"的字段。
	//
	// scheduleErr 与 mode 是**成对**的:切到 dapr-job 就必须清空它
	// ("曾经登记失败过"这个事实到那时已经不成立了)。
	// 分开写就会出现 healthz 说 "mode=dapr-job 但 schedule_error 还有值" 的自相矛盾。
	mu          sync.Mutex
	tickerStop  context.CancelFunc
	scheduleErr string

	running atomic.Bool
	// inflight 是本 Controller 的在途锁。**必须挂在实例上,不能用包级变量** ——
	// 包级 mutex 在测试里跨用例共享,一个用例卡住会让另一个永远拿不到锁,
	// 而且这种耦合在单实例时看不出来。
	inflight sync.Mutex
	// skipped 统计因重叠被跳过的触发次数 —— 守卫有没有在起作用,看这个数。
	skipped atomic.Int64
	// triggered 统计真正执行过的次数。
	triggered atomic.Int64

	reload func() []string
	lg     *log.Logger
}

// Stats 是对外可观测的运行统计。
type Stats struct {
	Mode        Mode   `json:"mode"`
	JobName     string `json:"job_name,omitempty"`
	Triggered   int64  `json:"triggered"`
	Skipped     int64  `json:"skipped_overlap"`
	ScheduleErr string `json:"schedule_error,omitempty"`
}

// Setup 决定刷新由谁驱动。
//
// sidecarAddr 传空字符串时直接退 ticker(本地裸跑、没有 dapr 的场景)。
// interval <= 0 时返回 ModeDisabled,不登记任何东西。
func Setup(ctx context.Context, sidecarAddr string, interval time.Duration,
	reload func() []string, lg *log.Logger) *Controller {

	c := &Controller{
		jobName: DefaultJobName,
		reload:  reload,
		lg:      lg,
	}
	c.mode.Store(ModeDisabled)

	if interval <= 0 {
		lg.Info("refresh disabled (interval <= 0)")
		return c
	}
	if sidecarAddr == "" {
		lg.Info("no sidecar address, falling back to ticker", "every", interval.String())
		c.setMode(ModeTicker, "")
		return c
	}

	// 登记给 scheduler。**必须给短超时**:sidecar 连不上 scheduler 时
	// jobs API 会一直挂着(实测 2026-10-10),不设 deadline 这里会卡住整个启动。
	schedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client := daprclient.NewJobsClient(sidecarAddr)
	err := client.ScheduleJob(schedCtx, c.jobName, daprclient.ScheduleJobRequest{
		Schedule: "@every " + interval.String(),
		Data:     c.jobName,
	})

	if err != nil {
		c.setMode(ModeTicker, err.Error())
		lg.Info("dapr job 登记失败,回退进程内 ticker",
			"every", interval.String(), "err", err.Error())
		return c
	}

	c.setMode(ModeDaprJob, "")
	lg.Info("dapr job 已登记,刷新由 Scheduler 驱动",
		"job", c.jobName, "schedule", "@every "+interval.String(),
		"sidecar", sidecarAddr)
	return c
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

// promoteToDaprJob 在**收到 job 触发**时把驱动切回 dapr-job 并停掉 ticker。
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
// # 为什么必须在这里切,而不是继续让 ticker 跑
//
// 启动时登记失败 → 回退 ticker,但 **job 记录可能还留在 etcd 里**。
// Scheduler 一恢复,那个旧 job 就会回来投递,于是 ticker 和 job 同时刷同一个
// DuckDB —— 刷新频率翻倍。而且 healthz 会说 "mode=ticker" 而实际上 job 一直在触发,
// 那个字段开始说谎。
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
		"job", c.jobName)
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

	return Stats{
		Mode:        c.Mode(),
		JobName:     c.jobName,
		Triggered:   c.triggered.Load(),
		Skipped:     c.skipped.Load(),
		ScheduleErr: err,
	}
}

// StatsAny 是给 healthz 用的适配器。
//
// 存在原因:boot.HealthHandler 收的是 `func() any`(为了不反向依赖本包),
// 而 Go 的函数类型不变,`func() Stats` **不能**隐式当作 `func() any` 传。
// 在这里包一层,既保住 Stats() 的类型信息(测试里能用),
// 又不用让 boot 依赖本包。
func (c *Controller) StatsAny() any { return c.Stats() }

// Start 在需要时启动进程内 ticker(仅 ModeTicker)。
//
// ModeDaprJob 时这里什么都不做 —— 触发来自 sidecar 的 HTTP 回调。
//
// ticker 必须**可取消**:登记失败后回退到它,而 Scheduler 恢复后靠
// promoteToDaprJob() 停掉它。原来的 goroutine 只认传入的 ctx,
// 运行期没有任何办法停它 —— 那样 ticker 一旦兜底就永远兜底了。
func (c *Controller) Start(ctx context.Context, interval time.Duration, lg *log.Logger) {
	if c.Mode() != ModeTicker {
		return
	}
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
// **无论当前是哪种 mode 都要注册**:job 记录存在 Scheduler 的 etcd 里,跨重启存活。
// 如果这次启动时登记失败(mode=ModeTicker),旧 job 仍然会来触发,
// 没有这个端点就会持续 404,而 Scheduler 侧看到的是"投递失败"。
//
// 而且两个 mode 同时存在是**正确行为**而不是冲突:ticker 保底,dapr job 统一管理,
// 一次刷新跑不掉(在途守卫会跳过重叠的那些),最终仍然是"刷新了"。
func (c *Controller) JobHandler() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		name := ctx.Param("jobName")
		if name != c.jobName {
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
			// 收到触发 = Scheduler 已恢复(见 promoteToDaprJob 的依据)。
			//
			// 顺序:**先切模式再干活**。切换发生在本次刷新开始之前,
			// 于是 ticker 从这一刻起不会再触发,而本次 job 触发照常执行。
			// 反过来(先 reload 再切)会留一个窗口:ticker 在 reload 期间到点,
			// 明明调度器已经接管却仍被在途守卫跳过 —— 白丢一次刷新,
			// 而且日志里只留一句"跳过",没人看得出本该执行。
			c.promoteToDaprJob()
			c.triggered.Add(1)
			c.lg.Info("dapr job triggered", "job", name, "payload", string(body))
			c.reload()
			ran = true
		} else {
			c.skipped.Add(1)
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
		c.skipped.Add(1)
		c.lg.Info("刷新触发时上一次还在跑,跳过本次", "source", source)
		return
	}
	defer c.unlock()
	c.triggered.Add(1)
	c.reload()
}
