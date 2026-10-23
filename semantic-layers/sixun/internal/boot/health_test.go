package boot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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

// TestHealthHandler_ReportsModels 锁"healthz 报出注册了的 model"。
func TestHealthHandler_ReportsModels(t *testing.T) {
	body := getHealth(t, (&Config{AppID: "sixun-ysx-00"}).HealthHandler([]string{"product", "member"}))
	if body["source"] != "sixun-ysx-00" {
		t.Errorf("source 应是 app id,实际 %v", body["source"])
	}
	models, ok := body["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("models 应列出 2 个 model,实际 %v", body["models"])
	}
	if body["status"] != "ok" {
		t.Errorf("status 应为 ok,实际 %v", body["status"])
	}
}

// TestHealthHandler_NoCopySegments 锁"healthz 里**没有**副本相关的那两段"。
//
// ⚠️ 2026-10-23 去掉本地副本:以前 healthz 里有
//   - data    = 本地副本的物化状态与年龄
//   - refresh = 刷新 job 的统计
// 现在**一个都不落本地副本** ⇒ 没有副本、没有物化、没有刷新 job
// ⇒ 那两段永远是空,而且**留着它们是假信号**:
// 一个恒为 "ok" 的健康检查会让人以为"有人在管数据的新鲜度",
// 而实际上数据新鲜度 = 源库的新鲜度。
//
// ⇒ 这条测试是**防回退**的:谁再加回副本/刷新机制,这里就会红。
func TestHealthHandler_NoCopySegments(t *testing.T) {
	body := getHealth(t, (&Config{AppID: "x"}).HealthHandler([]string{"product"}))
	for _, seg := range []string{"data", "refresh", "jobs"} {
		if _, ok := body[seg]; ok {
			t.Errorf("本实例不持有数据副本,healthz 不该再有 %q 段(实际 %v)", seg, body[seg])
		}
	}
}
