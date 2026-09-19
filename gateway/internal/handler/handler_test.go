// handler_test 端到端验证 /register 和 /v1/meta 流程(P0-1)。
//
// 模拟场景:cube app 启动后 POST /register,BI 工具 GET /v1/meta 应能看到。
package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YunBright/cube/gateway/internal/handler"
	"github.com/YunBright/cube/gateway/internal/l1cache"
	"github.com/YunBright/cube/gateway/internal/registry"
	"github.com/YunBright/cube/gateway/internal/router"
	"github.com/YunBright/cube/pkg/config"
	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"

	"github.com/gin-gonic/gin"
)

// newTestServer 构造一个跑在 httptest 的 gateway(gin engine)。
func newTestServer(t *testing.T) (*httptest.Server, *registry.Registry) {
	t.Helper()

	cfg, err := config.NewDefaultLoader("")
	if err != nil {
		t.Fatal(err)
	}
	lg := log.New("test-gateway")
	dapr := daprclient.NewNoop()
	reg := registry.New(dapr, "", lg)
	rtr := router.New(reg, nil)
	cch := l1cache.New(60)

	h := handler.New(handler.Deps{
		Cfg:      cfg,
		Logger:   lg,
		Dapr:     dapr,
		Registry: reg,
		Router:   rtr,
		L1Cache:  cch,
	})

	// gin engine:*gin.Engine 已实现 http.Handler,httptest.NewServer 直接接
	engine := gin.New()
	engine.POST("/register", h.Register)
	engine.GET("/v1/meta", h.Meta)
	engine.POST("/v1/load", h.Load)
	engine.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv, reg
}

// TestRegisterAndMeta 验证 cube app 注册流程。
func TestRegisterAndMeta(t *testing.T) {
	srv, reg := newTestServer(t)

	// 1. cube app 启动注册
	registerBody := `{
		"app_id":"sixun-hbposv7",
		"family":"sixun",
		"version":"hbposv7",
		"models":["supplier","product","order"],
		"capabilities":["query","preagg","cache_l2"],
		"health_url":"/health"
	}`
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader([]byte(registerBody)))
	if err != nil {
		t.Fatalf("register request failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("register: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. /v1/meta 应返回注册的 app
	resp, err = http.Get(srv.URL + "/v1/meta")
	if err != nil {
		t.Fatalf("meta request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta: want 200, got %d", resp.StatusCode)
	}

	var meta struct {
		CubeVersion string              `json:"cube_version"`
		Apps        []*registry.AppInfo `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}

	if len(meta.Apps) != 1 {
		t.Fatalf("want 1 app, got %d", len(meta.Apps))
	}
	got := meta.Apps[0]
	if got.AppID != "sixun-hbposv7" {
		t.Errorf("app_id want sixun-hbposv7, got %s", got.AppID)
	}
	if got.Family != "sixun" {
		t.Errorf("family want sixun, got %s", got.Family)
	}
	if len(got.Models) != 3 {
		t.Errorf("models want 3 entries, got %d", len(got.Models))
	}

	// 3. 注册第二个 app(ysx),registry 应有 2 条
	ysxBody := `{
		"app_id":"sixun-ysx",
		"family":"sixun",
		"version":"ysx",
		"models":["supplier","product","order"]
	}`
	resp, err = http.Post(srv.URL+"/register", "application/json", bytes.NewReader([]byte(ysxBody)))
	if err != nil {
		t.Fatalf("ysx register: %v", err)
	}
	resp.Body.Close()

	if got := len(reg.All()); got != 2 {
		t.Errorf("registry want 2 apps, got %d", got)
	}

	// 4. /register 收到 malformed body → 400
	resp, err = http.Post(srv.URL+"/register", "application/json", bytes.NewReader([]byte(`{not json`)))
	if err != nil {
		t.Fatalf("malformed register: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestRouterPicksRegisteredApp 验证 router 优先用注册表(不是 fallback static)。
func TestRouterPicksRegisteredApp(t *testing.T) {
	srv, _ := newTestServer(t)

	// 注册一个非常规 app_id
	body := `{
		"app_id":"liangyou-v1",
		"family":"liangyou",
		"version":"v1",
		"models":["supplier"]
	}`
	resp, _ := http.Post(srv.URL+"/register", "application/json", bytes.NewReader([]byte(body)))
	resp.Body.Close()

	// /v1/load 查 supplier → 应被路由到 liangyou-v1(注册表查到),不是 static fallback
	// (static 没有,所以应该 404)
	queryBody := `{"measures":["supplier.count"],"dimensions":[]}`
	resp, err := http.Post(srv.URL+"/v1/load", "application/json", bytes.NewReader([]byte(queryBody)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer resp.Body.Close()

	// 当前实现 dapr 是 Noop,Load handler 会试图 InvokeMethod 失败 → 502
	// 但 router.Route() 成功返回了 liangyou-v1(没到 404),说明 router 逻辑 OK
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("router should have found liangyou-v1 via registry, got 404")
	}
}