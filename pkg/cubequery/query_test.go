// cubequery_test 验证 cube query JSON 解析 + Model 推断。
package cubequery_test

import (
	"testing"

	"github.com/YunBright/cube/pkg/cubequery"
)

func TestParseAndModel(t *testing.T) {
	body := []byte(`{
		"measures": ["stock.total_quantity"],
		"dimensions": ["stock.branch_id"]
	}`)
	q, err := cubequery.Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := q.Model(); got != "stock" {
		t.Errorf("Model: want stock, got %s", got)
	}
	if len(q.Measures) != 1 || q.Measures[0] != "stock.total_quantity" {
		t.Errorf("Measures: %v", q.Measures)
	}
	if len(q.Dimensions) != 1 || q.Dimensions[0] != "stock.branch_id" {
		t.Errorf("Dimensions: %v", q.Dimensions)
	}
}

func TestParseFromMeasuresOnly(t *testing.T) {
	body := []byte(`{"measures":["product.count"]}`)
	q, err := cubequery.Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := q.Model(); got != "product" {
		t.Errorf("Model: want product, got %s", got)
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := cubequery.Parse([]byte(`{not json`)); err == nil {
		t.Errorf("want error for malformed JSON")
	}
	if _, err := cubequery.Parse([]byte(``)); err == nil {
		t.Errorf("want error for empty body")
	}
}

func TestParseEmptyModel(t *testing.T) {
	// 没有 measures/dimensions,Model() 应返回空
	q, _ := cubequery.Parse([]byte(`{}`))
	if got := q.Model(); got != "" {
		t.Errorf("Model: want empty, got %s", got)
	}
}