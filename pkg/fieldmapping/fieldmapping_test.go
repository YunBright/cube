// fieldmapping_test 验证 yaml 解析 + 字段映射 + 类型转换。
package fieldmapping_test

import (
	"testing"

	"github.com/YunBright/cube/pkg/fieldmapping"
)

const supplierMapping = `
version: 1
model: supplier
mappings:
  - source: cups
    target: id
    type: string
  - source: cups_name
    target: name
    type: string
  - source: class_id
    target: category
    type: string
  - source: weight_gross
    target: weight_gross_g
    type: int
    unit: g
  - source: reg_time
    target: registered_at
    type: datetime
`

func TestLoadAndApply(t *testing.T) {
	m, err := fieldmapping.Load([]byte(supplierMapping))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Spec().Model != "supplier" {
		t.Errorf("model want supplier, got %s", m.Spec().Model)
	}

	// 应用映射:思迅7pro 原始字段 → 统一字段
	raw := map[string]any{
		"cups":         "SUP001",
		"cups_name":    "可口可乐",
		"class_id":     "1",
		"weight_gross": "1500",
		"reg_time":     "2026-09-17 00:00:00",
	}
	out, err := m.Apply(raw)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if out["id"] != "SUP001" {
		t.Errorf("id want SUP001, got %v", out["id"])
	}
	if out["name"] != "可口可乐" {
		t.Errorf("name want 可口可乐, got %v", out["name"])
	}
	if out["category"] != "1" {
		t.Errorf("category want 1, got %v", out["category"])
	}
	// weight_gross 是 string "1500" → 应转 int64
	if w, ok := out["weight_gross_g"].(int64); !ok || w != 1500 {
		t.Errorf("weight_gross_g want int64(1500), got %T(%v)", out["weight_gross_g"], out["weight_gross_g"])
	}

	// 验证 SourceOf 反查
	if src, ok := m.SourceOf("weight_gross_g"); !ok || src != "weight_gross" {
		t.Errorf("SourceOf want weight_gross, got %s (ok=%v)", src, ok)
	}
}

func TestDuplicateTarget(t *testing.T) {
	bad := `
version: 1
model: supplier
mappings:
  - source: a
    target: id
    type: string
  - source: b
    target: id
    type: string
`
	if _, err := fieldmapping.Load([]byte(bad)); err == nil {
		t.Errorf("want error for duplicate target, got nil")
	}
}

func TestPassThroughUnknownFields(t *testing.T) {
	m, err := fieldmapping.Load([]byte(supplierMapping))
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"cups": "X", "extra_field": "kept"}
	out, err := m.Apply(row)
	if err != nil {
		t.Fatal(err)
	}
	if out["extra_field"] != "kept" {
		t.Errorf("unknown field should pass through, got %v", out["extra_field"])
	}
}