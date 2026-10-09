package boot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/freshness"
)

// callHealth 通过真实 gin engine 发一次请求 —— 不能直接构造 gin.Context
// (httptest.ResponseRecorder 不实现 gin.ResponseWriter)。
func callHealth(t *testing.T, h gin.HandlerFunc) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/healthz", h)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func getHealth(t *testing.T, h gin.HandlerFunc) map[string]any {
	t.Helper()
	code, out := callHealth(t, h)
	if code != 200 {
		t.Fatalf("healthz 应 200,实际 %d", code)
	}
	return out
}

// TestHealthHandler_RefreshIsEvaluatedPerRequest 锁的是 2026-10-10 踩到的那个坑。
//
// 把 `sched.Stats()` 直接当值传进来时,它在 handler 构造那一刻就求值了,
// 之后 healthz 里的 triggered / skipped / data_age 永久冻结在启动值 ——
// 接口 200、内容看着正常,只是再也不反映运行期。
// 日志里明明有 "dapr job triggered",healthz 却显示 triggered:0。
//
// 判据不是"格式对不对",而是"第二次请求能不能看到变化"。
func TestHealthHandler_RefreshIsEvaluatedPerRequest(t *testing.T) {
	n := 0
	h := (&Config{AppID: "sixun-ysx-00"}).HealthHandler(
		[]string{"product"},
		nil,
		func() any {
			n++
			return map[string]any{"triggered": n}
		},
	)

	first := getHealth(t, h)["refresh"].(map[string]any)
	if first["triggered"].(float64) != 1 {
		t.Fatalf("第一次请求 triggered 应为 1,实际 %v", first["triggered"])
	}

	second := getHealth(t, h)["refresh"].(map[string]any)
	if second["triggered"].(float64) != 2 {
		t.Fatalf("第二次请求必须看到新值(triggered=2),实际 %v —— "+
			"说明 refresh 被当成值求值了一次,计数器已冻结", second["triggered"])
	}
}

// TestHealthHandler_NilRefreshOmitsSegment 没传 provider 时不能输出 refresh 段。
func TestHealthHandler_NilRefreshOmitsSegment(t *testing.T) {
	h := (&Config{AppID: "x"}).HealthHandler([]string{"product"}, nil, nil)
	body := getHealth(t, h)
	if _, ok := body["refresh"]; ok {
		t.Fatal("nil provider 时不应有 refresh 段")
	}
	if body["status"] != "ok" {
		t.Fatalf("无 tracker 时 status 应为 ok,实际 %v", body["status"])
	}
}

// TestHealthHandler_DegradedStillReturns200 数据旧 ≠ 服务不可用。
//
// 返 503 会让 dapr / gateway 把它当宕机从而拒绝查询,
// 把一个降级问题升级成不可用问题 —— 这是刻意不做的。
func TestHealthHandler_DegradedStillReturns200(t *testing.T) {
	tr := freshness.New(30 * 60 * 1e9) // 30m
	tr.Record("product", errFailed)

	h := (&Config{AppID: "x"}).HealthHandler([]string{"product"}, tr, nil)
	code, out := callHealth(t, h)

	if code != http.StatusOK {
		t.Fatalf("degraded 时仍应 200,实际 %d", code)
	}
	if out["status"] != "degraded" {
		t.Fatalf("有 model 加载失败时 status 应 degraded,实际 %v", out["status"])
	}
	if !strings.Contains(toStr(out["reason"]), "boom") {
		t.Fatalf("reason 应包含真实错误,实际 %v", out["reason"])
	}
}

var errFailed = errBoom{}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func toStr(v any) string {
	s, _ := v.(string)
	return s
}
