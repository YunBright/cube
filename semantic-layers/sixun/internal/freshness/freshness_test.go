package freshness

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock 让"数据变旧"变成可确定复现的,而不是靠 sleep。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

const interval = 30 * time.Minute

var base = time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)

func newTestTracker(t *testing.T, ivl time.Duration) (*Tracker, *fakeClock) {
	t.Helper()
	c := &fakeClock{t: base}
	tr := New(ivl)
	tr.SetClock(c.now)
	return tr, c
}

// TestOK_WhenAllModelsFresh 全部刚刷过 → ok。
func TestOK_WhenAllModelsFresh(t *testing.T) {
	tr, c := newTestTracker(t, interval)
	tr.Record("product", nil)
	tr.Record("supplier", nil)
	c.add(5 * time.Minute)

	s := tr.Snapshot()
	if s.Status != "ok" {
		t.Fatalf("刚刷过 5 分钟应 ok,实际 status=%q reason=%q", s.Status, s.Reason)
	}
	if s.DataAgeSeconds == nil || *s.DataAgeSeconds != 300 {
		t.Fatalf("data_age 应为 300 秒,实际 %v", s.DataAgeSeconds)
	}
	if s.LastSuccessAt == nil {
		t.Fatal("last_success_at 不应为 nil")
	}
	if len(s.Models) != 2 {
		t.Fatalf("应逐 model 上报,实际 %d 个", len(s.Models))
	}
}

// TestDegraded_NeverSucceeded 从没成功加载过 → degraded,且必须说明原因。
//
// 这是最关键的一条:启动时某个 model 拉失败,以前 healthz 恒为 ok。
func TestDegraded_NeverSucceeded(t *testing.T) {
	tr, _ := newTestTracker(t, interval)
	tr.Record("product", errors.New("connect timeout"))
	tr.Record("supplier", nil)

	s := tr.Snapshot()
	if s.Status != "degraded" {
		t.Fatalf("只有 product 失败时整体应 degraded,实际 %q", s.Status)
	}
	if s.LastSuccessAt == nil {
		t.Fatal("supplier 成功过,last_success_at 不应为 nil")
	}
	if s.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures 应为 1,实际 %d", s.ConsecutiveFailures)
	}
	if !strings.Contains(s.Reason, "connect timeout") {
		t.Fatalf("reason 必须带上真实错误,实际 %q", s.Reason)
	}
	if !strings.Contains(s.LastError, "product") {
		t.Fatalf("last_error 应指出是哪个 model,实际 %q", s.LastError)
	}
}

// TestDegraded_WorstModelWins 整体状态取最差 model,不是最好的那个。
//
// 判据:product 刚刷完、supplier 三小时前刷的 → 必须 degraded。
// 取"最好的"会让 product 的新鲜掩盖 supplier 的陈旧,那是在骗调用方。
func TestDegraded_WorstModelWins(t *testing.T) {
	tr, c := newTestTracker(t, interval)
	tr.Record("supplier", nil)
	c.add(3 * time.Hour)
	tr.Record("product", nil)

	s := tr.Snapshot()
	if s.Status != "degraded" {
		t.Fatalf("最差 model 过期时整体必须 degraded,实际 %q", s.Status)
	}
	if !strings.Contains(s.Reason, "超过重拉间隔") {
		t.Fatalf("reason 应说明是过期,实际 %q", s.Reason)
	}
	// 整体 last_success 应是最早那次(3 小时前),不是最近的
	if s.DataAgeSeconds == nil || *s.DataAgeSeconds < 3*3600 {
		t.Fatalf("整体 data_age 应取最旧 model(约 10800 秒),实际 %v", s.DataAgeSeconds)
	}
}

// TestOK_WithinTwiceInterval 刚过 1 个间隔仍然是 ok。
//
// 锁的是"阈值取 2 倍"这个刻意选择:1 倍会因重启/GC/慢查询天天误报,
// 久而久之没人看 —— 那等于没有监控。
func TestOK_WithinTwiceInterval(t *testing.T) {
	tr, c := newTestTracker(t, interval)
	tr.Record("product", nil)
	c.add(interval + 5*time.Minute)

	if s := tr.Snapshot(); s.Status != "ok" {
		t.Fatalf("超过 1 个间隔但未到 2 倍应仍是 ok,实际 %q reason=%q", s.Status, s.Reason)
	}
}

// TestDegraded_BeyondTwiceInterval 超过 2 倍间隔 → degraded。
func TestDegraded_BeyondTwiceInterval(t *testing.T) {
	tr, c := newTestTracker(t, interval)
	tr.Record("product", nil)
	c.add(2*interval + 2*time.Minute)

	s := tr.Snapshot()
	if s.Status != "degraded" {
		t.Fatalf("超过 2 倍间隔应 degraded,实际 %q", s.Status)
	}
	if !strings.Contains(s.Reason, "超过重拉间隔") {
		t.Fatalf("reason 应说明过期,实际 %q", s.Reason)
	}
}

// TestRecovers_AfterNextSuccess 失败一次后下一次成功 → 回到 ok。
//
// 不然就是"一次失败永久 degraded"的告警疲劳。
func TestRecovers_AfterNextSuccess(t *testing.T) {
	tr, c := newTestTracker(t, interval)
	tr.Record("product", errors.New("boom"))
	c.add(time.Minute)
	if s := tr.Snapshot(); s.Status != "degraded" {
		t.Fatalf("失败时应 degraded,实际 %q", s.Status)
	}

	tr.Record("product", nil)
	s := tr.Snapshot()
	if s.Status != "ok" {
		t.Fatalf("恢复后应回到 ok,实际 %q reason=%q", s.Status, s.Reason)
	}
	if s.ConsecutiveFailures != 0 {
		t.Fatalf("成功后连续失败数应清零,实际 %d", s.ConsecutiveFailures)
	}
	if s.LastError != "" {
		t.Fatalf("成功后 last_error 应清空,实际 %q", s.LastError)
	}
}

// TestRefreshDisabled_NoStalenessJudgment interval<=0 = 刻意关闭重拉。
//
// 这时只报"从未成功加载过",**不**因为时间久而报过期 ——
// 没有刷新就没有"变旧"这个概念,否则关掉重拉会立刻永久 degraded。
func TestRefreshDisabled_NoStalenessJudgment(t *testing.T) {
	tr, c := newTestTracker(t, 0)
	tr.Record("product", nil)
	c.add(1000 * time.Hour)

	s := tr.Snapshot()
	if s.Status != "ok" {
		t.Fatalf("关闭重拉后不应因时间久而 degraded,实际 %q reason=%q", s.Status, s.Reason)
	}
	if s.IntervalSeconds != 0 {
		t.Fatalf("interval_seconds 应为 0,实际 %d", s.IntervalSeconds)
	}
}

// TestDegraded_NoModels 一个 model 都没记录 → degraded。
func TestDegraded_NoModels(t *testing.T) {
	tr, _ := newTestTracker(t, interval)
	s := tr.Snapshot()
	if s.Status != "degraded" {
		t.Fatalf("没有任何记录时应 degraded,实际 %q", s.Status)
	}
}

// TestConcurrentRecordAndSnapshot 刷新线程写、gin handler 读,必须并发安全。
//
// 这不是"顺手加个锁":dataloader 在 goroutine 里 Record,
// 同时 /healthz 可能在另一个 goroutine 里 Snapshot。
// 用 -race 才抓得到,这里只是确保锁真的在。
func TestConcurrentRecordAndSnapshot(t *testing.T) {
	tr, _ := newTestTracker(t, interval)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			tr.Record("product", nil)
			tr.Record("supplier", errors.New("x"))
		}
	}()
	for i := 0; i < 500; i++ {
		_ = tr.Snapshot()
	}
	<-done
}
