package apierror_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/YunBright/cube/pkg/apierror"
)

// TestHTTPStatus_Mapping 校验每个 Code 都有合理的 HTTP 状态码。
func TestHTTPStatus_Mapping(t *testing.T) {
	cases := []struct {
		code apierror.Code
		want int
	}{
		{apierror.SOURCE_FORMAT_INVALID, http.StatusBadRequest},
		{apierror.SOURCE_NOT_REGISTERED, http.StatusNotFound},
		{apierror.SOURCE_OFFLINE, http.StatusServiceUnavailable},
		{apierror.UPSTREAM_TIMEOUT, http.StatusGatewayTimeout},
		{apierror.VERSION_UNSUPPORTED, http.StatusBadRequest},
		{apierror.MODEL_NOT_FOUND_IN_SOURCE, http.StatusNotFound},
		{apierror.QUERY_PARSE_ERROR, http.StatusBadRequest},
		{apierror.QUERY_INVALID, http.StatusBadRequest},
		{apierror.NO_PRINCIPAL, http.StatusUnauthorized},
		{apierror.FORBIDDEN, http.StatusForbidden},
		{apierror.RATE_LIMITED, http.StatusTooManyRequests},
		{apierror.INTERNAL_ERROR, http.StatusInternalServerError},
		{apierror.UPSTREAM_ERROR, http.StatusBadGateway},
		{apierror.DAPR_APP_ID_INVALID, http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := apierror.HTTPStatus(c.code); got != c.want {
			t.Errorf("HTTPStatus(%s) = %d, want %d", c.code, got, c.want)
		}
	}
}

// TestHTTPStatus_UnknownCode 未知 Code 退化到 500。
func TestHTTPStatus_UnknownCode(t *testing.T) {
	if got := apierror.HTTPStatus("NEVER_DEFINED"); got != http.StatusInternalServerError {
		t.Errorf("unknown code should fall back to 500, got %d", got)
	}
}

// TestAllCodes_CoversTable AllCodes 返回值应能反查回 codeHTTPStatus(14 个)。
//
// v2 末 plan B 增 DAPR_APP_ID_INVALID,所以是 14 个。
func TestAllCodes_CoversTable(t *testing.T) {
	codes := apierror.AllCodes()
	if len(codes) != 14 {
		t.Errorf("AllCodes returned %d codes, want 14", len(codes))
	}
}

// TestNew_BasicFields New 构造的 Error 字段正确。
func TestNew_BasicFields(t *testing.T) {
	e := apierror.New(apierror.SOURCE_NOT_REGISTERED, "missing")
	if e.Code != apierror.SOURCE_NOT_REGISTERED {
		t.Errorf("code mismatch: %s", e.Code)
	}
	if e.Message != "missing" {
		t.Errorf("message mismatch: %s", e.Message)
	}
	if e.Details != nil {
		t.Errorf("details should be nil by default")
	}
	if e.Cause != nil {
		t.Errorf("cause should be nil by default")
	}
}

// TestNewf 格式化消息。
func TestNewf(t *testing.T) {
	e := apierror.Newf(apierror.SOURCE_NOT_REGISTERED, "source not registered: %s", "sixun-ysx-99")
	if e.Message != "source not registered: sixun-ysx-99" {
		t.Errorf("formatted message wrong: %s", e.Message)
	}
}

// TestWithDetails_Merge 多次 WithDetails 会 merge,后写覆盖先写。
func TestWithDetails_Merge(t *testing.T) {
	e := apierror.New(apierror.UPSTREAM_ERROR, "x").
		WithDetails(map[string]any{"a": 1, "b": 2}).
		WithDetails(map[string]any{"b": 99, "c": 3})
	if e.Details["a"] != 1 {
		t.Errorf("a should remain 1")
	}
	if e.Details["b"] != 99 {
		t.Errorf("b should be overwritten to 99")
	}
	if e.Details["c"] != 3 {
		t.Errorf("c should be added as 3")
	}
}

// TestWithDetail 单 key 追加。
func TestWithDetail(t *testing.T) {
	e := apierror.New(apierror.QUERY_INVALID, "no model").WithDetail("source", "sixun-ysx-00")
	if e.Details["source"] != "sixun-ysx-00" {
		t.Errorf("WithDetail did not record source")
	}
}

// TestError_Format Error() 应包含 code + message。
func TestError_Format(t *testing.T) {
	e := apierror.New(apierror.SOURCE_OFFLINE, "down")
	s := e.Error()
	if !contains(s, "SOURCE_OFFLINE") || !contains(s, "down") {
		t.Errorf("Error() = %q, want both code and message", s)
	}
}

// TestError_FormatWithCause Cause 应出现在 Error() 输出末尾。
func TestError_FormatWithCause(t *testing.T) {
	cause := errors.New("connection refused")
	e := apierror.New(apierror.SOURCE_OFFLINE, "down").WithCause(cause)
	if !contains(e.Error(), "connection refused") {
		t.Errorf("Error() missing cause: %s", e.Error())
	}
}

// TestUnwrap errors.Is 能穿透到 Cause。
func TestUnwrap(t *testing.T) {
	cause := errors.New("boom")
	e := apierror.New(apierror.INTERNAL_ERROR, "wrapped").WithCause(cause)
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is should match cause")
	}
}

// TestHTTPStatus_Method Error.HTTPStatus 等于 HTTPStatus(Code)。
func TestHTTPStatus_Method(t *testing.T) {
	e := apierror.New(apierror.UPSTREAM_TIMEOUT, "x")
	if e.HTTPStatus() != http.StatusGatewayTimeout {
		t.Errorf("HTTPStatus() wrong")
	}
}

// TestImmutability_WithDetails 返回新对象,原对象不变。
func TestImmutability_WithDetails(t *testing.T) {
	orig := apierror.New(apierror.SOURCE_NOT_REGISTERED, "x")
	derived := orig.WithDetail("k", "v")
	if orig.Details != nil {
		t.Errorf("original should not be mutated")
	}
	if derived.Details["k"] != "v" {
		t.Errorf("derived should have k=v")
	}
}

// TestRequestIDFromCtx_NoValue 无 key 时返回 ("", false)。
func TestRequestIDFromCtx_NoValue(t *testing.T) {
	if rid, ok := apierror.RequestIDFromCtx(context.Background()); ok || rid != "" {
		t.Errorf("expected no request id, got (%q, %v)", rid, ok)
	}
}

// TestRequestIDFromCtx_WithValue 注入 ctx 后能读到。
func TestRequestIDFromCtx_WithValue(t *testing.T) {
	ctx := context.WithValue(context.Background(), apierror.CtxKeyRequestID, "rid-abc")
	rid, ok := apierror.RequestIDFromCtx(ctx)
	if !ok || rid != "rid-abc" {
		t.Errorf("expected rid-abc, got (%q, %v)", rid, ok)
	}
}

// TestRequestIDFromCtx_WrongType 注入非 string 值时 ok=false。
func TestRequestIDFromCtx_WrongType(t *testing.T) {
	ctx := context.WithValue(context.Background(), apierror.CtxKeyRequestID, 123)
	if _, ok := apierror.RequestIDFromCtx(ctx); ok {
		t.Errorf("expected ok=false for non-string value")
	}
}

// TestError_NilReceiver 在 nil 上调用 Error() 不 panic。
func TestError_NilReceiver(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil receiver panicked: %v", r)
		}
	}()
	var e *apierror.Error
	_ = e.Error()
}

// contains 是 strings.Contains 的内联(避免再 import)。
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 避免 unused import 警告(虽然 _ = fmt 没必要,这里就保 errors/fmt 都用上)。
var _ = fmt.Sprintf
