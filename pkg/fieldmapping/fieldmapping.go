// Package fieldmapping 解析 mapping.yaml,运行时执行字段映射。
//
// 设计要点(P1-5 选 A):
//   - 仅覆盖:字段名重命名、类型转换、单位标注
//   - **不做** enum_map、**不做** transform(trim/lower)
//   - 枚举值归一化留在 schema.yaml meta + BI 层
//
// mapping.yaml schema:
//
//	version: 1
//	model: supplier
//	mappings:
//	  - source: cups           # 原始字段名
//	    target: supplier_id    # 统一后字段名
//	    type: string
//	    unit: ""               # 可选:g/kg/yuan 等单位标注
//
// 调用方:每个 dapr cube app 启动时 Load(yamlPath),得到 Mapper。
package fieldmapping

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldType 是统一后的字段类型。
type FieldType string

const (
	TypeString   FieldType = "string"
	TypeInt      FieldType = "int"
	TypeFloat    FieldType = "float"
	TypeDecimal  FieldType = "decimal"
	TypeBool     FieldType = "bool"
	TypeDatetime FieldType = "datetime"
)

// FieldDef 是 mapping.yaml 里的一条映射规则。
type FieldDef struct {
	Source string    `yaml:"source"`
	Target string    `yaml:"target"`
	Type   FieldType `yaml:"type"`
	Unit   string    `yaml:"unit,omitempty"`
}

// Spec 是 mapping.yaml 的根结构。
type Spec struct {
	Version  int        `yaml:"version"`
	Model    string     `yaml:"model"`
	Mappings []FieldDef `yaml:"mappings"`
}

// Mapper 是运行时映射器,从 source row 到统一 row。
type Mapper struct {
	spec     *Spec
	targets  []string // 保持顺序的 target 列表
	bySource map[string]*FieldDef
	byTarget map[string]*FieldDef
}

// Load 解析 mapping.yaml 字节流。
func Load(yamlBytes []byte) (*Mapper, error) {
	if len(yamlBytes) == 0 {
		return nil, errors.New("fieldmapping: empty yaml")
	}
	var spec Spec
	if err := yaml.Unmarshal(yamlBytes, &spec); err != nil {
		return nil, fmt.Errorf("fieldmapping: parse yaml: %w", err)
	}
	if spec.Version != 1 {
		return nil, fmt.Errorf("fieldmapping: unsupported version %d", spec.Version)
	}
	if spec.Model == "" {
		return nil, errors.New("fieldmapping: model is empty")
	}
	m := &Mapper{
		spec:     &spec,
		bySource: map[string]*FieldDef{},
		byTarget: map[string]*FieldDef{},
	}
	for i := range spec.Mappings {
		def := &spec.Mappings[i]
		if def.Source == "" || def.Target == "" {
			return nil, fmt.Errorf("fieldmapping: mapping #%d has empty source/target", i)
		}
		if _, dup := m.bySource[def.Source]; dup {
			return nil, fmt.Errorf("fieldmapping: duplicate source %q", def.Source)
		}
		if _, dup := m.byTarget[def.Target]; dup {
			return nil, fmt.Errorf("fieldmapping: duplicate target %q", def.Target)
		}
		m.bySource[def.Source] = def
		m.byTarget[def.Target] = def
		m.targets = append(m.targets, def.Target)
	}
	return m, nil
}

// Apply 把一行原始数据转换为统一 schema。
func (m *Mapper) Apply(row map[string]any) (map[string]any, error) {
	if m == nil || row == nil {
		return row, nil
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		def, ok := m.bySource[k]
		if !ok {
			// 未在 mapping 中声明的字段:保留(留给 cube schema 决定是否暴露)
			out[k] = v
			continue
		}
		converted, err := convert(v, def.Type)
		if err != nil {
			return nil, fmt.Errorf("fieldmapping: convert field %q -> %q: %w", k, def.Target, err)
		}
		out[def.Target] = converted
	}
	return out, nil
}

// Targets 返回所有映射后的字段名(用于 cube schema 对齐)。
func (m *Mapper) Targets() []string {
	return m.targets
}

// TargetsWithType 返回所有 target 字段及其类型(用于 DuckDB 建表)。
func (m *Mapper) TargetsWithType() []FieldDef {
	out := make([]FieldDef, 0, len(m.targets))
	for _, name := range m.targets {
		if d, ok := m.byTarget[name]; ok {
			out = append(out, *d)
		}
	}
	return out
}

// SourceOf 反查 target 对应的 source 字段。
func (m *Mapper) SourceOf(target string) (string, bool) {
	def, ok := m.byTarget[target]
	if !ok {
		return "", false
	}
	return def.Source, true
}

// Spec 返回原始 spec(供调试用)。
func (m *Mapper) Spec() *Spec { return m.spec }

// Loader 一次加载多个 mapping-*.yaml,按 model 名查询。
//
// 文件命名约定:`mapping-<model>.yaml`(例:mapping-supplier.yaml)
//
// 用法:
//
//	l, err := fieldmapping.NewLoader("./mappings")
//	if err != nil { ... }
//	mapper, ok := l.Get("supplier")
//	if !ok { ... }
//	row, _ := mapper.Apply(rawRow)
type Loader struct {
	byModel map[string]*Mapper
}

// NewLoader 加载目录下所有 mapping-*.yaml。
func NewLoader(dir string) (*Loader, error) {
	l := &Loader{byModel: map[string]*Mapper{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("fieldmapping: read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		const prefix = "mapping-"
		const ext = ".yaml"
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		model := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext)
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("fieldmapping: read %s: %w", name, err)
		}
		mapper, err := Load(data)
		if err != nil {
			return nil, fmt.Errorf("fieldmapping: load %s: %w", name, err)
		}
		l.byModel[model] = mapper
	}
	return l, nil
}

// Get 按 model 名查 Mapper。
func (l *Loader) Get(model string) (*Mapper, bool) {
	if l == nil {
		return nil, false
	}
	m, ok := l.byModel[model]
	return m, ok
}

// Models 返回所有已加载的 model 名。
func (l *Loader) Models() []string {
	out := make([]string, 0, len(l.byModel))
	for k := range l.byModel {
		out = append(out, k)
	}
	return out
}

// ---- 内部 ----

func convert(v any, t FieldType) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch t {
	case TypeString:
		return fmt.Sprint(v), nil
	case TypeInt:
		switch x := v.(type) {
		case int:
			return int64(x), nil
		case int32:
			return int64(x), nil
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		case []byte:
			return strconv.ParseInt(string(x), 10, 64)
		case string:
			return strconv.ParseInt(x, 10, 64)
		}
		return nil, fmt.Errorf("cannot convert %T to int", v)
	case TypeFloat, TypeDecimal:
		switch x := v.(type) {
		case float64:
			return x, nil
		case float32:
			return float64(x), nil
		case int64:
			return float64(x), nil
		case int:
			return float64(x), nil
		case []byte:
			return strconv.ParseFloat(string(x), 64)
		case string:
			return strconv.ParseFloat(x, 64)
		}
		return nil, fmt.Errorf("cannot convert %T to float", v)
	case TypeBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			return x != 0, nil
		case string:
			return strconv.ParseBool(x)
		}
		return nil, fmt.Errorf("cannot convert %T to bool", v)
	case TypeDatetime:
		// DuckDB 直接识别 time.Time;此处原样返回,由 driver 层保证类型正确
		return v, nil
	}
	return nil, fmt.Errorf("unknown field type %q", t)
}