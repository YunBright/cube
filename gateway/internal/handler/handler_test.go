// handler_test 验证 cube-gateway 的三类入口:
//   - POST /register
//   - POST /v1/source/:source/load
//   - GET  /v1/sources
//
// 用 fakeDapr 注入 InvokeMethod 行为,测试每种错误路径是否能映射到正确的 apierror.Code。
package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YunBright/cube/gateway/internal/handler"
	"github.com/YunBright/cube/gateway/internal/l1cache"
	"github.com/YunBright/cube/gateway/internal/middleware"
	"github.com/YunBright/cube/gateway/internal/registry"
	"github.com/YunBright/cube/pkg/apierror"
	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"

	"github.com/gin-gonic/gin"
)

// fakeDapr 实现 daprclient.Client,每个方法都有可注入的回调。
// 通过 invokeFn 控制 InvokeMethod 的返回;state 用来验证 SaveState。
type fakeDapr struct {
	invokeFn func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error)
	invokeCount int32 // 原子,统计 invoke 次数
	state    map[string][]byte
	saveErr  error
	deleteErr error
	deleteCalled atomic.Bool // 记录 DeleteState 是否被调过
}

func (f *fakeDapr) InvokeMethod(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
	atomic.AddInt32(&f.invokeCount, 1)
	if f.invokeFn != nil {
		return f.invokeFn(ctx, target, method, data, extra)
	}
	return nil, fmt.Errorf("fakeDapr: no invokeFn set for %s/%s", target, method)
}

func (f *fakeDapr) PublishToPubSub(ctx context.Context, pubsub, topic string, data []byte) error {
	return nil
}

func (f *fakeDapr) GetState(ctx context.Context, store, key string) ([]byte, bool, error) {
	v, ok := f.state[key]
	return v, ok, nil
}

func (f *fakeDapr) SaveState(ctx context.Context, store, key string, value []byte) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.state == nil {
		f.state = map[string][]byte{}
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	f.state[key] = cp
	return nil
}

func (f *fakeDapr) DeleteState(ctx context.Context, store, key string) error {
	f.deleteCalled.Store(true)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.state != nil {
		delete(f.state, key)
	}
	return nil
}

func (f *fakeDapr) Close() error { return nil }

// apiErr 用于解码错误响应。
type apiErr struct {
	Code    apierror.Code   `json:"code"`
	Message string          `json:"message"`
	Details map[string]any  `json:"details"`
}

func decodeError(t *testing.T, body io.Reader) apiErr {
	t.Helper()
	var e apiErr
	if err := json.NewDecoder(body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e
}

func decodeOK(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("decode ok body: %v", err)
	}
	return m
}

// newTestServer 启动一个 httptest.Server,装配完整 handler + 中间件。
// 返回 server、registry(测试可操作)、fakeDapr(测试可注入 invokeFn)。
func newTestServer(t *testing.T, dapr daprclient.Client) (*httptest.Server, *registry.Registry, *fakeDapr) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	lg := log.New("test-gateway")
	reg := registry.New(dapr, "test-store", lg)
	cch := l1cache.New(60)

	fd, ok := dapr.(*fakeDapr)
	if !ok {
		t.Fatalf("expected *fakeDapr, got %T", dapr)
	}

	h := handler.New(handler.Deps{
		Logger:   lg,
		Dapr:     dapr,
		Registry: reg,
		L1Cache:  cch,
		Auth:     nil,
	})

	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.Use(middleware.ErrorRecovery(lg))
	engine.POST("/register", h.Register)
	engine.POST("/unregister", h.Unregister)
	engine.POST("/v1/source/:source/load", h.SourceLoad)
	engine.GET("/v1/sources", h.ListSources)
	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv, reg, fd
}

// register 通过 /register 注册一个有效的 cube app,返回 status code。
func register(t *testing.T, srv *httptest.Server, appID string) int {
	t.Helper()
	body := map[string]any{
		"app_id":  appID,
		"family":  "sixun",
		"version": "ysx",
		"models":  []string{"supplier", "product", "stock"},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// ---- 1. Register happy path ----

func TestRegister_Happy(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)
	if got := register(t, srv, "sixun-ysx-00"); got != http.StatusNoContent {
		t.Errorf("want 204, got %d", got)
	}
}

// ---- 2. Register bad source format ----

func TestRegister_BadSourceFormat(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)

	for _, bad := range []string{"bad-id", "sixun-ysx", "sixun-ysx-", "-ysx-00"} {
		t.Run(bad, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"app_id": bad, "family": "sixun", "version": "ysx",
				"models": []string{"supplier"},
			})
			resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("want 400, got %d", resp.StatusCode)
			}
			e := decodeError(t, resp.Body)
			if e.Code != apierror.SOURCE_FORMAT_INVALID {
				t.Errorf("want SOURCE_FORMAT_INVALID, got %s", e.Code)
			}
		})
	}
}

// ---- 3. ListSources empty ----

func TestListSources_Empty(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)

	resp, err := http.Get(srv.URL + "/v1/sources")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	m := decodeOK(t, resp.Body)
	srcs, ok := m["sources"].([]any)
	if !ok || len(srcs) != 0 {
		t.Errorf("want empty sources array, got %v", m["sources"])
	}
}

// ---- 4. ListSources —— 被动验证模式下所有已注册 source 都报 online ----
//
// 历史原因:旧实现里 LastSeen 5 分钟前 → 视为 offline。
// 现在 handler 不再前置 IsOnline 检查,/v1/sources 视图统一报 "online";
// LastSeen 字段保留为运维排查信号,但不再驱动 status。
func TestListSources_OnlineAndOffline(t *testing.T) {
	dapr := &fakeDapr{}
	srv, reg, _ := newTestServer(t, dapr)

	register(t, srv, "sixun-ysx-00")
	register(t, srv, "sixun-ysx-99")

	// 把 second 的 LastSeen 推到 5 分钟前 —— 旧实现会判为 offline,新实现不影响。
	if info, ok := reg.LookupByID("sixun-ysx-99"); ok {
		info.LastSeen = time.Now().Add(-5 * time.Minute)
	}

	resp, err := http.Get(srv.URL + "/v1/sources")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	m := decodeOK(t, resp.Body)
	srcs := m["sources"].([]any)
	if len(srcs) != 2 {
		t.Fatalf("want 2 sources, got %d", len(srcs))
	}

	statusBySrc := map[string]string{}
	for _, s := range srcs {
		e := s.(map[string]any)
		statusBySrc[e["source"].(string)] = e["status"].(string)
	}
	// 被动验证:无论 LastSeen 多旧,已注册 source 都报 online。
	if statusBySrc["sixun-ysx-00"] != "online" {
		t.Errorf("sixun-ysx-00 should be online, got %s", statusBySrc["sixun-ysx-00"])
	}
	if statusBySrc["sixun-ysx-99"] != "online" {
		t.Errorf("sixun-ysx-99 should be online (passive mode), got %s", statusBySrc["sixun-ysx-99"])
	}
}

// ---- 5. SourceLoad happy miss + hit ----

func TestSourceLoad_Happy_Miss_And_Hit(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return []byte(`{"data":[{"x":1}]}`), nil
		},
	}
	srv, _, fd := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"],"dimensions":[]}`)

	// 1st: cache miss
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("1st want 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Cube-Cache") != "L1-MISS" {
		t.Errorf("1st want L1-MISS, got %s", resp.Header.Get("X-Cube-Cache"))
	}
	resp.Body.Close()

	// 2nd: cache hit
	resp2, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("2nd want 200, got %d", resp2.StatusCode)
	}
	if resp2.Header.Get("X-Cube-Cache") != "L1-HIT" {
		t.Errorf("2nd want L1-HIT, got %s", resp2.Header.Get("X-Cube-Cache"))
	}

	if got := atomic.LoadInt32(&fd.invokeCount); got != 1 {
		t.Errorf("invoke should fire once, got %d", got)
	}
}

// ---- 6. SourceLoad source not registered ----

func TestSourceLoad_SourceNotRegistered(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)

	body := []byte(`{"measures":["supplier.count"],"dimensions":[]}`)
	resp, err := http.Post(srv.URL+"/v1/source/unknown-foo-99/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.SOURCE_NOT_REGISTERED {
		t.Errorf("want SOURCE_NOT_REGISTERED, got %s", e.Code)
	}
	if e.Details["source"] != "unknown-foo-99" {
		t.Errorf("details.source want unknown-foo-99, got %v", e.Details["source"])
	}
}

// ---- 7. SourceLoad 被动验证:stale LastSeen 不再前置拦截 ----
//
// 历史:旧实现里 LastSeen 10 分钟前 → handler 立刻返 503 SOURCE_OFFLINE。
// 现在 handler 不前置 IsOnline;已注册 source 总是尝试 dapr 真实 invoke;
// fake dapr (无 invokeFn) 会返 "no invokeFn set" 错 → 走 UPSTREAM_ERROR(502)。
func TestSourceLoad_SourceOffline_Stale(t *testing.T) {
	dapr := &fakeDapr{}
	srv, reg, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")
	if info, ok := reg.LookupByID("sixun-ysx-00"); ok {
		info.LastSeen = time.Now().Add(-10 * time.Minute) // 远超旧 stale 窗口
	}

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	// 被动验证:不再返 503 SOURCE_OFFLINE。
	// fakeDapr 默认 invokeFn == nil → InvokeMethod 返 "no invokeFn set" 错
	// → 走 UPSTREAM_ERROR 路径 → 502。
	if resp.StatusCode == http.StatusServiceUnavailable {
		e := decodeError(t, resp.Body)
		if e.Code == apierror.SOURCE_OFFLINE {
			t.Fatalf("passive mode: should NOT short-circuit to SOURCE_OFFLINE on stale LastSeen, got 503 %s", e.Code)
		}
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 UPSTREAM_ERROR (no invokeFn), got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.UPSTREAM_ERROR {
		t.Errorf("want UPSTREAM_ERROR, got %s", e.Code)
	}
}

// ---- 8. SourceLoad upstream timeout ----

func TestSourceLoad_UpstreamTimeout(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrTimeout, Cause: context.DeadlineExceeded,
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.UPSTREAM_TIMEOUT {
		t.Errorf("want UPSTREAM_TIMEOUT, got %s", e.Code)
	}
}

// ---- 9. SourceLoad upstream conn failure ----

func TestSourceLoad_UpstreamConnFailure(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrConnFailure, Cause: fmt.Errorf("connection refused"),
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.SOURCE_OFFLINE {
		t.Errorf("want SOURCE_OFFLINE, got %s", e.Code)
	}
}

// ---- 10. SourceLoad upstream 5xx ----

func TestSourceLoad_UpstreamError5xx(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrHTTPStatus, StatusCode: 503,
				Body: []byte(`{"error":"db down"}`),
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.UPSTREAM_ERROR {
		t.Errorf("want UPSTREAM_ERROR, got %s", e.Code)
	}
	if e.Details["upstream_status"] != float64(503) { // json 解码到 any 是 float64
		t.Errorf("details.upstream_status want 503, got %v", e.Details["upstream_status"])
	}
}

// ---- 11. SourceLoad upstream 4xx → UPSTREAM_ERROR with body ----

func TestSourceLoad_Upstream4xx(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrHTTPStatus, StatusCode: 422,
				Body: []byte(`{"code":"UNKNOWN_SUBCODE","message":"weird"}`),
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.UPSTREAM_ERROR {
		t.Errorf("want UPSTREAM_ERROR, got %s", e.Code)
	}
	if e.Details["upstream_body"] == nil {
		t.Errorf("details.upstream_body should be set")
	}
}

// ---- 12. SourceLoad MODEL_NOT_FOUND_IN_SOURCE ----

func TestSourceLoad_ModelNotFound(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrHTTPStatus, StatusCode: 400,
				Body: []byte(`{"code":"MODEL_NOT_FOUND","message":"model not exposed"}`),
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["unknown.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.MODEL_NOT_FOUND_IN_SOURCE {
		t.Errorf("want MODEL_NOT_FOUND_IN_SOURCE, got %s", e.Code)
	}
}

// TestSourceLoad_ModelNotFound_404Status 复现 plan B 暴露的契约 bug:
//
// cube app 按协议对未知 model 返 404 + body code=MODEL_NOT_FOUND
// (sixun-hbposv7 main.go::writeErr(c, http.StatusNotFound, subCodeModelNotFound, ...))。
// gateway 之前只看 400 status,扫到 404 直接归类为 SOURCE_OFFLINE(dapr 找不到 app),
// 把业务 404 误判为离线,导致 TestCubeGatewayLoadModelNotFound 503 而非 404。
//
// 修复:mapHTTPStatusToCode 收到 404 时先扫 body 子码,
// 命中 MODEL_NOT_FOUND → 404 MODEL_NOT_FOUND_IN_SOURCE;
// body 无 code → SOURCE_OFFLINE(dapr 真的找不到 app)。
func TestSourceLoad_ModelNotFound_404Status(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrHTTPStatus, StatusCode: 404,
				Body: []byte(`{"code":"MODEL_NOT_FOUND","message":"model not exposed by source","details":{"supported":["supplier","product"]}}`),
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["nonexistent.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.MODEL_NOT_FOUND_IN_SOURCE {
		t.Errorf("want MODEL_NOT_FOUND_IN_SOURCE (NOT SOURCE_OFFLINE), got %s", e.Code)
	}
}

// TestSourceLoad_Upstream404NoBody 真"dapr 找不到 app"路径 — body 为空 / 无 code 子码,
// 应归类为 SOURCE_OFFLINE (503)。这是 404 的兜底分支。
func TestSourceLoad_Upstream404NoBody(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrHTTPStatus, StatusCode: 404,
				Body: []byte(`ERR_DIRECT_INVOKE: app cube-sixun-ysx-00 not registered`),
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.SOURCE_OFFLINE {
		t.Errorf("want SOURCE_OFFLINE, got %s", e.Code)
	}
}

// ---- 13. SourceLoad VERSION_UNSUPPORTED ----

func TestSourceLoad_VersionUnsupported(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrHTTPStatus, StatusCode: 400,
				Body: []byte(`{"code":"VERSION_UNSUPPORTED","message":"too old"}`),
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.VERSION_UNSUPPORTED {
		t.Errorf("want VERSION_UNSUPPORTED, got %s", e.Code)
	}
}

// ---- 14. SourceLoad QUERY_PARSE_ERROR (malformed JSON) ----

func TestSourceLoad_QueryParseError(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader([]byte(`{not-json`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.QUERY_PARSE_ERROR {
		t.Errorf("want QUERY_PARSE_ERROR, got %s", e.Code)
	}
}

// ---- 15. SourceLoad QUERY_INVALID (empty model) ----

func TestSourceLoad_QueryInvalidEmptyModel(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader([]byte(`{"measures":[],"dimensions":[]}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.QUERY_INVALID {
		t.Errorf("want QUERY_INVALID, got %s", e.Code)
	}
}

// ---- 16. SourceLoad SOURCE_FORMAT_INVALID ----

func TestSourceLoad_SourceFormatInvalid(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)

	resp, err := http.Post(srv.URL+"/v1/source/bad/load",
		"application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.SOURCE_FORMAT_INVALID {
		t.Errorf("want SOURCE_FORMAT_INVALID, got %s", e.Code)
	}
}

// ---- 17. SourceLoad NO_PRINCIPAL (Auth configured, no Authorization) ----

type allowAllAuth struct{}

func (allowAllAuth) Allow(principal, source, model string) bool { return true }

func TestSourceLoad_NoPrincipal(t *testing.T) {
	dapr := &fakeDapr{}
	gin.SetMode(gin.TestMode)
	lg := log.New("test-gateway")
	reg := registry.New(dapr, "test-store", lg)
	cch := l1cache.New(60)
	h := handler.New(handler.Deps{
		Logger: lg, Dapr: dapr, Registry: reg, L1Cache: cch,
		Auth: allowAllAuth{}, // 开启 Auth
	})
	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.Use(middleware.ErrorRecovery(lg))
	engine.POST("/v1/source/:source/load", h.SourceLoad)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	register(t, srv, "sixun-ysx-00")
	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.NO_PRINCIPAL {
		t.Errorf("want NO_PRINCIPAL, got %s", e.Code)
	}
}

// ---- 18. SourceLoad FORBIDDEN (Authorizer returns false) ----

type denyAllAuth struct{}

func (denyAllAuth) Allow(principal, source, model string) bool { return false }

func TestSourceLoad_Forbidden(t *testing.T) {
	dapr := &fakeDapr{}
	gin.SetMode(gin.TestMode)
	lg := log.New("test-gateway")
	reg := registry.New(dapr, "test-store", lg)
	cch := l1cache.New(60)
	h := handler.New(handler.Deps{
		Logger: lg, Dapr: dapr, Registry: reg, L1Cache: cch,
		Auth: denyAllAuth{},
	})
	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.Use(middleware.ErrorRecovery(lg))
	engine.POST("/v1/source/:source/load", h.SourceLoad)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	register(t, srv, "sixun-ysx-00")
	body := []byte(`{"measures":["supplier.count"]}`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/source/sixun-ysx-00/load", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.FORBIDDEN {
		t.Errorf("want FORBIDDEN, got %s", e.Code)
	}
}

// ---- 19. SourceLoad real ctx timeout (fake sleeps past upstreamCallTimeout) ----

func TestSourceLoad_RealCtxTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real ctx timeout test in -short mode")
	}
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			// 阻塞到 ctx deadline 或 12s(谁先到),模拟 upstream 卡死
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(12 * time.Second):
				return []byte(`{"data":[]}`), nil
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	resp, err := http.Post(srv.URL+"/v1/source/sixun-ysx-00/load",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d", resp.StatusCode)
	}
	e := decodeError(t, resp.Body)
	if e.Code != apierror.UPSTREAM_TIMEOUT {
		t.Errorf("want UPSTREAM_TIMEOUT, got %s", e.Code)
	}
}

// ---- 20. request id round-trips ----

func TestRequestID_RoundTrip(t *testing.T) {
	dapr := &fakeDapr{
		invokeFn: func(ctx context.Context, target, method string, data []byte, extra map[string]string) ([]byte, error) {
			// 让 ctx 早返回 timeout → 走错误路径;这样才会回填 request_id 到 Details
			return nil, &daprclient.InvokeError{
				TargetAppID: target, Method: method,
				Kind: daprclient.ErrTimeout, Cause: context.DeadlineExceeded,
			}
		},
	}
	srv, _, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	body := []byte(`{"measures":["supplier.count"]}`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/source/sixun-ysx-00/load", bytes.NewReader(body))
	req.Header.Set("X-Request-Id", "my-test-id-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "my-test-id-123" {
		t.Errorf("X-Request-Id want my-test-id-123, got %s", got)
	}
	e := decodeError(t, resp.Body)
	if got, _ := e.Details["request_id"].(string); got != "my-test-id-123" {
		t.Errorf("Details.request_id want my-test-id-123, got %v", e.Details["request_id"])
	}
}

// ---- 21. register 全 3 段校验:多组合法格式都通过 ----

func TestRegister_ValidFormats(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)

	for _, appID := range []string{
		"sixun-ysx-00",
		"sixun-ysx-baiyuan1",
		"sixun-hbposv7-jiale",
		"liangyou-v2-foo",
		"a-b-c",
	} {
		t.Run(appID, func(t *testing.T) {
			if got := register(t, srv, appID); got != http.StatusNoContent {
				t.Errorf("want 204, got %d", got)
			}
		})
	}
}

// ---- 22. unregister happy path:已注册 source 被 unregister 后从 registry 消失 ----

func TestUnregister_Happy(t *testing.T) {
	dapr := &fakeDapr{}
	srv, reg, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	// 验证已注册
	if _, ok := reg.LookupByID("sixun-ysx-00"); !ok {
		t.Fatalf("setup: source should exist in registry")
	}

	// unregister
	body := []byte(`{"app_id":"sixun-ysx-00"}`)
	resp, err := http.Post(srv.URL+"/unregister", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}

	// 验证已删除
	if _, ok := reg.LookupByID("sixun-ysx-00"); ok {
		t.Errorf("source should be removed from registry after unregister")
	}
}

// ---- 23. unregister 幂等:未注册的 source 也返 204(gateway 重启 / 已被 unregister) ----

func TestUnregister_Idempotent(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)
	// 不 register 直接 unregister —— 应仍返 204

	body := []byte(`{"app_id":"sixun-ysx-99"}`)
	resp, err := http.Post(srv.URL+"/unregister", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204 (idempotent), got %d", resp.StatusCode)
	}
}

// ---- 24. unregister body 格式校验:1 段 / 2 段 / 空 → 400 ----

func TestUnregister_BadFormat(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, _ := newTestServer(t, dapr)

	cases := []struct {
		name string
		body string
	}{
		{"empty", `{"app_id":""}`},
		{"one_segment", `{"app_id":"bad"}`},
		{"two_segment", `{"app_id":"sixun-ysx"}`},
		{"missing", `{}`},
		{"malformed_json", `{not-json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/unregister", "application/json",
				bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("want 400, got %d", resp.StatusCode)
			}
			e := decodeError(t, resp.Body)
			// empty / missing 是 SOURCE_FORMAT_INVALID,malformed_json 是 QUERY_PARSE_ERROR
			if tc.name == "malformed_json" {
				if e.Code != apierror.QUERY_PARSE_ERROR {
					t.Errorf("want QUERY_PARSE_ERROR, got %s", e.Code)
				}
			} else {
				if e.Code != apierror.SOURCE_FORMAT_INVALID {
					t.Errorf("want SOURCE_FORMAT_INVALID, got %s", e.Code)
				}
			}
		})
	}
}

// ---- 25. unregister 后 state store DeleteState 被调用(持久化清理) ----

func TestUnregister_DeleteStateCalled(t *testing.T) {
	dapr := &fakeDapr{}
	srv, _, fd := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")

	resp, err := http.Post(srv.URL+"/unregister", "application/json",
		bytes.NewReader([]byte(`{"app_id":"sixun-ysx-00"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	if !fd.deleteCalled.Load() {
		t.Errorf("DeleteState should be called on unregister (state store key: registry:sixun-ysx-00)")
	}
}

// ---- 22. 避免未使用 import 警告 ----
var _ = strings.TrimSpace
