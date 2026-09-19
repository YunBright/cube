// Package cubeschema 解析 cube 风格 schema.yaml(dimensions / measures / joins)。
//
// 设计要点:
//   - schema.yaml 是 family 级(<family>-models/),所有实例共用
//   - 字段名与 mapping.yaml 的 target 对齐
//   - 不在 schema.yaml 里写数据源连接信息(那是 mapping.yaml + source/ 的事)
package cubeschema

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// DataType 是 cube 维度/度量类型。
type DataType string

const (
	TypeString  DataType = "string"
	TypeNumber  DataType = "number"
	TypeTime    DataType = "time"
	TypeBoolean DataType = "boolean"
)

// Dimension 是 cube schema 的维度定义。
type Dimension struct {
	Name        string      `yaml:"name"`
	SQL         string      `yaml:"sql"`
	Type        DataType    `yaml:"type"`
	Title       string      `yaml:"title"`
	EnumValues  []EnumValue `yaml:"enum_values,omitempty"`
	Description string      `yaml:"description,omitempty"`
}

// EnumValue 是枚举值的展示翻译(用于 BI 显示,不影响原始数据)。
type EnumValue struct {
	Value string `yaml:"value"`
	Label string `yaml:"label"`
}

// Measure 是 cube schema 的度量定义。
type Measure struct {
	Name        string   `yaml:"name"`
	SQL         string   `yaml:"sql"`
	Type        DataType `yaml:"type"`
	Title       string   `yaml:"title"`
	Format      string   `yaml:"format,omitempty"`
	Description string   `yaml:"description,omitempty"`
}

// Join 是表关联。
type Join struct {
	Name         string `yaml:"name"`
	SQL          string `yaml:"sql"`
	On           string `yaml:"on"`
	Relationship string `yaml:"relationship"`
}

// Model 是 cube schema 的根。
type Model struct {
	Name       string      `yaml:"name"`
	SQLTable   string      `yaml:"sql_table"`
	Dimensions []Dimension `yaml:"dimensions"`
	Measures   []Measure   `yaml:"measures"`
	Joins      []Join      `yaml:"joins,omitempty"`
}

// Load 解析 schema.yaml。
func Load(yamlBytes []byte) (*Model, error) {
	if len(yamlBytes) == 0 {
		return nil, errors.New("cubeschema: empty yaml")
	}
	var m Model
	if err := yaml.Unmarshal(yamlBytes, &m); err != nil {
		return nil, fmt.Errorf("cubeschema: parse yaml: %w", err)
	}
	if m.Name == "" {
		return nil, errors.New("cubeschema: model.name is empty")
	}
	if m.SQLTable == "" {
		return nil, fmt.Errorf("cubeschema: model %q: sql_table is empty", m.Name)
	}
	// 校验 dimension / measure name 唯一
	seenDim := map[string]bool{}
	for i := range m.Dimensions {
		d := &m.Dimensions[i]
		if d.Name == "" {
			return nil, fmt.Errorf("cubeschema: model %q: dimension #%d has empty name", m.Name, i)
		}
		if seenDim[d.Name] {
			return nil, fmt.Errorf("cubeschema: model %q: duplicate dimension %q", m.Name, d.Name)
		}
		seenDim[d.Name] = true
	}
	seenMeas := map[string]bool{}
	for i := range m.Measures {
		ms := &m.Measures[i]
		if ms.Name == "" {
			return nil, fmt.Errorf("cubeschema: model %q: measure #%d has empty name", m.Name, i)
		}
		if seenMeas[ms.Name] {
			return nil, fmt.Errorf("cubeschema: model %q: duplicate measure %q", m.Name, ms.Name)
		}
		seenMeas[ms.Name] = true
	}
	return &m, nil
}

// FindDimension 按名查维度。
func (m *Model) FindDimension(name string) (*Dimension, bool) {
	for i := range m.Dimensions {
		if m.Dimensions[i].Name == name {
			return &m.Dimensions[i], true
		}
	}
	return nil, false
}

// FindMeasure 按名查度量。
func (m *Model) FindMeasure(name string) (*Measure, bool) {
	for i := range m.Measures {
		if m.Measures[i].Name == name {
			return &m.Measures[i], true
		}
	}
	return nil, false
}

// DimensionNames 返回所有维度名。
func (m *Model) DimensionNames() []string {
	out := make([]string, 0, len(m.Dimensions))
	for _, d := range m.Dimensions {
		out = append(out, d.Name)
	}
	return out
}

// MeasureNames 返回所有度量名。
func (m *Model) MeasureNames() []string {
	out := make([]string, 0, len(m.Measures))
	for _, ms := range m.Measures {
		out = append(out, ms.Name)
	}
	return out
}