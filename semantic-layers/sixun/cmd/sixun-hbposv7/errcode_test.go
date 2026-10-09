package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/log"
	models "github.com/YunBright/cube/sixun-models"
)

// ---- 4xx 子码分层:"东西不存在"有两类,不能共用一个码 ----
//
// 2026-10-10 生产现场:查询里把 settlement.sheet_no 写错(真实字段是 id),
// cube app 回了 404 + code=MODEL_NOT_FOUND,gateway 扫到该子码后翻成
// "model not exposed by source: settlement"。于是排查方向整个跑偏 ——
// 去查 model 有没有暴露 / 有没有注册,而真正要改的只是查询里的字段名。
// 现场是"查询写错"被报成了"契约/部署问题",两者重试策略正好相反。
//
// 正确分层:
//   - model 不存在      → 404 MODEL_NOT_FOUND(契约/部署问题,运维介入)
//   - dimension/measure → 400 QUERY_INVALID(model 是好的,调用方查询写错)
//   - 非 BuildError     → 500 INTERNAL_ERROR(我们的 bug,不是任何业务结论)
//
// 本文件与 sixun-ysx/errcode_test.go 内容一致 —— 两个 family 必须同时满足,
// 只修一边就等于又制造了一次"同一份契约、两套实现"的分叉(见 pkg/cubequery
// 顶部注释:hbposv7 那份残缺实现曾静默丢弃 filters,单测却全绿)。

type errEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// postQuery 打一次 /query 并解出错误信封。
//
// 只测错误路径,所以 db / conn 都可以传 nil:错误发生在 SQL 拼装阶段,
// 走不到 DuckDB 与源库。传 nil 也顺带证明了"错误分类不依赖任何 IO 成功"。
func postQuery(t *testing.T, model string, body string) (int, errEnvelope) {
	t.Helper()

	raw, err := models.ReadSchema(model)
	if err != nil {
		t.Fatalf("models.ReadSchema(%s): %v", model, err)
	}
	sch, err := cubeschema.Load(raw)
	if err != nil {
		t.Fatalf("cubeschema.Load(%s): %v", model, err)
	}

	schemas := map[string]*schemaMeta{
		model: {Schema: sch},
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/query", queryHandler(nil, nil, schemas, []string{model}, "sixun-hbposv7-test", log.New("test")))

	srv := httptest.NewServer(engine)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/query", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post /query: %v", err)
	}
	defer resp.Body.Close()

	var env errEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return resp.StatusCode, env
}

// TestQueryHandler_UnknownModelIs404 锁第一档:model 本身不在该 source 里。
//
// 语义是"契约/部署问题" —— 调用方改查询没用,得改 schema 或注册,
// 所以是 404 + MODEL_NOT_FOUND,gateway 才不会把它当 SOURCE_OFFLINE。
func TestQueryHandler_UnknownModelIs404(t *testing.T) {
	code, env := postQuery(t, "product",
		`{"measures":["no_such_model.count"],"dimensions":["no_such_model.id"]}`)

	if code != http.StatusNotFound {
		t.Fatalf("unknown model: status = %d, want 404", code)
	}
	if env.Code != subCodeModelNotFound {
		t.Errorf("unknown model: code = %q, want %q", env.Code, subCodeModelNotFound)
	}
}

// TestQueryHandler_UnknownDimensionIs400 是本次修复的核心回归锁。
//
// 反例(2026-10-10 实际行为):同一个错,只要字段名写错就返回
// 404 MODEL_NOT_FOUND → gateway 报 "model not exposed by source",
// 把"改查询"说成"model 没暴露"。若这里将来又变回 404,测试立刻红。
func TestQueryHandler_UnknownDimensionIs400(t *testing.T) {
	code, env := postQuery(t, "product",
		`{"measures":["product.count"],"dimensions":["product.__no_such_dim__"]}`)

	if code != http.StatusBadRequest {
		t.Fatalf("unknown dimension: status = %d, want 400 (got code=%q msg=%q)",
			code, env.Code, env.Message)
	}
	if env.Code != subCodeQueryInvalid {
		t.Errorf("unknown dimension: code = %q, want %q", env.Code, subCodeQueryInvalid)
	}
	// 调用方要靠 ref 才能改对查询,不能只说"字段不存在"。
	if env.Details["ref"] != "__no_such_dim__" {
		t.Errorf("unknown dimension: details.ref = %v, want %q", env.Details["ref"], "__no_such_dim__")
	}
	if env.Details["model"] != "product" {
		t.Errorf("unknown dimension: details.model = %v, want %q", env.Details["model"], "product")
	}
}

// TestQueryHandler_UnknownMeasureIs400 与 dimension 同档。
//
// measure 单独测是有意义的:measure 缺失不影响维度循环,走的是另一处
// FindMeasure 失败分支 —— 曾经同样兜底成 404。
func TestQueryHandler_UnknownMeasureIs400(t *testing.T) {
	code, env := postQuery(t, "product",
		`{"measures":["product.__no_such_measure__"],"dimensions":["product.id"]}`)

	if code != http.StatusBadRequest {
		t.Fatalf("unknown measure: status = %d, want 400 (got code=%q msg=%q)",
			code, env.Code, env.Message)
	}
	if env.Code != subCodeQueryInvalid {
		t.Errorf("unknown measure: code = %q, want %q", env.Code, subCodeQueryInvalid)
	}
	if env.Details["ref"] != "__no_such_measure__" {
		t.Errorf("unknown measure: details.ref = %v, want %q", env.Details["ref"], "__no_such_measure__")
	}
}

// TestQueryHandler_NoMeasureIs400 守住"空 measures"这一档。
//
// 它过去同样是 404(维度对但 measures 空,Build 返 Kind=measure),
// 一并钉成 400,免得只修了字段名那一半。
func TestQueryHandler_NoMeasureIs400(t *testing.T) {
	code, env := postQuery(t, "product",
		`{"dimensions":["product.id"]}`)

	if code != http.StatusBadRequest {
		t.Fatalf("no measure: status = %d, want 400 (got code=%q)", code, env.Code)
	}
	if env.Code != subCodeQueryInvalid {
		t.Errorf("no measure: code = %q, want %q", env.Code, subCodeQueryInvalid)
	}
}
