// Package freshness 跟踪 duck 数据的新鲜度,并据此判定 healthz 的 status。
//
// # 为什么需要它
//
// 2026-10-09 之前,`/healthz` 只返回 models 列表和 uptime,恒为 `status: "ok"`。
// 于是下面三种完全不同的状态在监控和调用方看来**一模一样**:
//
//  1. 数据就是 30 分钟前的(正常)
//  2. 定时重拉挂了,数据其实是 3 天前的(故障)
//  3. 启动时某个 model 拉取失败,那张表其实是空的(故障)
//
// refresh 失败只留一行日志。这正是这个项目反复栽跟头的那一类:
// **状态不可见**。门店看到的是"商品查不到"或"价格是旧的",
// 而排查第一步永远是猜"是不是同步挂了",因为没有任何地方能告诉他。
//
// # 判定的口径
//
// 整体状态取**所有 model 里最差的那个**,不是最好的那个。
// 理由:product 新鲜但 supplier 挂了,调用方拿到的是混合快照,
// 说"ok"是在骗人。宁可整体报 degraded,也不要让最差的那个被平均掉。
//
// # 为什么不因为过期就返 5xx
//
// 数据旧 ≠ 服务不可用。返回 503 会让 dapr / gateway / supertrade
// 把它当成宕机,进而拒绝对这个 source 的查询 —— 而那张表其实还能查,
// 只是数字偏旧。把"旧"升级成"挂"会让一个降级问题变成一个不可用问题。
// 所以这里保持 200,用 `status` + `reason` 表达。
// 真的要"硬失败"(拒绝对不健康 app 的 invoke),用 dapr 的
// `--enable-app-health-check --app-health-check-path /healthz`,
// 那是显式的运维选择,不是默认行为。
package freshness

import (
	"sync"
	"time"
)

// modelState 是单个 model 的刷新状态。
type modelState struct {
	lastSuccess time.Time
	lastAttempt time.Time
	lastErr     string
	failures    int
}

// ModelSnapshot 是单个 model 的对外快照。
type ModelSnapshot struct {
	LastSuccessAt    *time.Time `json:"last_success_at,omitempty"`
	DataAgeSeconds   *int       `json:"data_age_seconds,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	ConsecutiveFails int        `json:"consecutive_failures"`
}

// Snapshot 是整体的对外快照,直接进 /healthz 的 `data` 字段。
type Snapshot struct {
	// IntervalSeconds 是配置的重拉间隔,调用方据此判断 age 是否合理。
	IntervalSeconds int `json:"interval_seconds"`
	// LastAttemptAt / LastSuccessAt 为 nil 表示**从未发生**。
	// 用指针而不是零值,是零值 time 序列化成 "0001-01-01..." 很难读。
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// DataAgeSeconds 是最差 model 的数据年龄。
	DataAgeSeconds      *int                     `json:"data_age_seconds,omitempty"`
	ConsecutiveFailures int                      `json:"consecutive_failures"`
	LastError           string                   `json:"last_error,omitempty"`
	Models              map[string]ModelSnapshot `json:"models,omitempty"`
	// Status 为 "ok" / "degraded"。
	Status string `json:"status"`
	// Reason 只在 degraded 时非空,直接说明为什么。
	Reason string `json:"reason,omitempty"`
}

// Tracker 记录刷新结果。**零值不可用**,用 New 创建。
//
// 并发安全:dataloader 在刷新时写,gin handler 同时读。
type Tracker struct {
	mu       sync.RWMutex
	interval time.Duration
	models   map[string]*modelState
	now      func() time.Time
}

// New 创建 Tracker。interval <= 0 表示"定时重拉已关闭",
// 此时只报"从未成功加载过",不做过期判定 —— 没刷新就不该有"数据变旧"这种说法。
func New(interval time.Duration) *Tracker {
	return &Tracker{
		interval: interval,
		models:   map[string]*modelState{},
		now:      time.Now,
	}
}

// SetClock 替换时间源,仅供测试。
func (t *Tracker) SetClock(f func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = f
}

// Record 记录一次某个 model 的物化结果。err == nil 表示成功。
func (t *Tracker) Record(model string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, ok := t.models[model]
	if !ok {
		st = &modelState{}
		t.models[model] = st
	}
	now := t.now()
	st.lastAttempt = now
	if err != nil {
		st.failures++
		st.lastErr = err.Error()
		return
	}
	st.failures = 0
	st.lastErr = ""
	st.lastSuccess = now
}

// Snapshot 返回当前快照。
func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	now := t.now()
	snap := Snapshot{
		IntervalSeconds: int(t.interval.Seconds()),
		Models:          make(map[string]ModelSnapshot, len(t.models)),
	}

	var (
		worstAge      time.Duration
		haveAge       bool
		totalFailures int
		worstErr      string
	)

	for name, st := range t.models {
		ms := ModelSnapshot{
			LastError:        st.lastErr,
			ConsecutiveFails: st.failures,
		}
		if !st.lastSuccess.IsZero() {
			s := st.lastSuccess
			ms.LastSuccessAt = &s
			age := int(now.Sub(st.lastSuccess).Seconds())
			ms.DataAgeSeconds = &age

			if !haveAge || now.Sub(st.lastSuccess) > worstAge {
				worstAge = now.Sub(st.lastSuccess)
				haveAge = true
			}
		}
		if !st.lastAttempt.IsZero() {
			a := st.lastAttempt
			snap.LastAttemptAt = &a
		}
		totalFailures += st.failures
		if st.failures > 0 && (worstErr == "" || st.lastErr != "") {
			worstErr = name + ": " + st.lastErr
		}
		snap.Models[name] = ms
	}

	// 整体 lastSuccess = 最早成功的那次(任一 model 落后,整体就落后)。
	// 而不是取最近的 —— 取最近会让"product 刚刷完"掩盖"supplier 挂了"。
	var earliest time.Time
	for _, st := range t.models {
		if st.lastSuccess.IsZero() {
			continue
		}
		if earliest.IsZero() || st.lastSuccess.Before(earliest) {
			earliest = st.lastSuccess
		}
	}
	if !earliest.IsZero() {
		e := earliest
		snap.LastSuccessAt = &e
		age := int(now.Sub(earliest).Seconds())
		snap.DataAgeSeconds = &age
	}

	snap.ConsecutiveFailures = totalFailures
	snap.LastError = worstErr

	// ---- 判定 ----
	switch {
	case len(t.models) == 0:
		snap.Status = "degraded"
		snap.Reason = "没有任何 model 完成过加载"

	case snap.LastSuccessAt == nil:
		// 有 model 记录但没有一个成功过
		snap.Status = "degraded"
		snap.Reason = "从未成功加载过数据"
		if worstErr != "" {
			snap.Reason += ";" + worstErr
		}

	case totalFailures > 0:
		snap.Status = "degraded"
		snap.Reason = "最近一次刷新有 model 失败:" + worstErr

	case t.interval > 0 && worstAge > 2*t.interval:
		// 阈值取 2 倍而不是 1 倍:一倍的间隔抖动(重启、GC、慢查询)很常见,
		// 1 倍就报 degraded 会天天误报,久而久之就没人看了 —— 那等于没有监控。
		snap.Status = "degraded"
		snap.Reason = "数据已过期:最旧 model 已 " +
			formatDuration(worstAge) + ",超过重拉间隔 " + formatDuration(t.interval) + " 的两倍"

	default:
		snap.Status = "ok"
	}

	return snap
}

func formatDuration(d time.Duration) string {
	return d.Truncate(time.Second).String()
}
