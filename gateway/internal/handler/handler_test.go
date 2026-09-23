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

// ---- 4. ListSources online + offline ----

func TestListSources_OnlineAndOffline(t *testing.T) {
	dapr := &fakeDapr{}
	srv, reg, _ := newTestServer(t, dapr)

	register(t, srv, "sixun-ysx-00")
	register(t, srv, "sixun-ysx-99")

	// 手动把 second 的 LastSeen 推到 5 分钟前 → 视为 offline
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
	if statusBySrc["sixun-ysx-00"] != "online" {
		t.Errorf("sixun-ysx-00 should be online, got %s", statusBySrc["sixun-ysx-00"])
	}
	if statusBySrc["sixun-ysx-99"] != "offline" {
		t.Errorf("sixun-ysx-99 should be offline, got %s", statusBySrc["sixun-ysx-99"])
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

// ---- 7. SourceLoad source offline (stale LastSeen) ----

func TestSourceLoad_SourceOffline_Stale(t *testing.T) {
	dapr := &fakeDapr{}
	srv, reg, _ := newTestServer(t, dapr)
	register(t, srv, "sixun-ysx-00")
	if info, ok := reg.LookupByID("sixun-ysx-00"); ok {
		info.LastSeen = time.Now().Add(-10 * time.Minute) // 远超 stale
	}

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

// ---- 22. 避免未使用 import 警告 ----
var _ = strings.TrimSpace
