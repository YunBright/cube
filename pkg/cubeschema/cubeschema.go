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
	"regexp"
	"strings"

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
	// CharDate 表示这一列是 **char 定长日期字符串**(思迅的 t_rm_daysum.oper_date
	// 实测就是 char(10) 'YYYY-MM-DD',不是 datetime)。
	//
	// 它不是装饰性标注,直接决定 SQL 形态与性能:
	//   - true  → dateRange 过滤对**原列**做字符串比较。ISO 日期的字典序等于
	//     时间序,语义正确,而且 sargable —— SQL Server 能用索引。
	//   - false → 必须 CAST 成日期再比较,正确但全表扫描。
	//
	// 思迅的日期列大量是 char 定长(且 hbposv7 还普遍补尾部空格),
	// 不标这个字段会让每次日期过滤都退化成全表扫描。
	CharDate bool `yaml:"char_date,omitempty"`
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

// Segment 是预定义的过滤片段:schema 里写好的一段 SQL 谓词,调用方只写名字。
//
// 它的价值在"判定条件要复用":企业微信审批里那套"异常单据"的判据会被
// 机器人、BI 报表、回归脚本反复用。写在 schema 里才有一份真值。
type Segment struct {
	Name        string `yaml:"name"`
	SQL         string `yaml:"sql"`
	Title       string `yaml:"title,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// Join 是表关联。
//
// Relationship 用 cube 的语义,方向是**从当前 model 出发**:
//   - many_to_one:当前 model 是"多"的一侧,join 进来的是"一"(查档案,行数不变)
//   - one_to_many:当前 model 是"一"的一侧,join 进来的是"多"(**行数会被放大**)
//   - one_to_one :一对一
//
// ⚠️ one_to_many 是扇出的来源:join 进来 N 行,当前 model 的度量就变成 N 倍,
// 而 SQL 完全合法、查询完全成功、金额凭空翻倍。编译器据此做静态判定
// (见 pkg/cubequery/join.go),不是靠人记住。
type Join struct {
	Name         string `yaml:"name"`
	SQL          string `yaml:"sql"`
	On           string `yaml:"on"`
	Relationship string `yaml:"relationship"`
}

// validRelationships 是允许的关联方向。
var validRelationships = map[string]bool{
	"many_to_one": true,
	"one_to_many": true,
	"one_to_one":  true,
}

// JoinNames 返回所有 join 名。
func (m *Model) JoinNames() []string {
	out := make([]string, 0, len(m.Joins))
	for _, j := range m.Joins {
		out = append(out, j.Name)
	}
	return out
}

// hasSubqueryRE 粗判 SQL 里有没有嵌套 SELECT。只用来在 schema 加载时拦下
// "segment 里藏子查询",不做完整的 SQL 解析 —— 误判方向是"多拦一个",
// 而多拦的代价(作者改成 join)远小于漏拦(列名盲替换改错别人的表)。
var hasSubqueryRE = regexp.MustCompile(`(?i)\bselect\b`)

// Storage 决定一个 model 的数据落在哪里。
//
// 架构约束(2026-10-09 确认):**cube 不做任何原始数据同步**。
// DuckDB 只承载聚合结果与常用枚举/档案类数据;明细与实时数据查询时透传源库。
type Storage string

const (
	// StorageDuck 物化进 DuckDB。用于枚举/档案类(变化少、体量小、被高频引用)。
	StorageDuck Storage = "duck"
	// StorageLive 不落盘,查询时实时透传源库。
	// 用于明细表与实时变化的数据(结算单、采购单、销售流水、当前库存)。
	StorageLive Storage = "live"
)

// Model 是 cube schema 的根。
type Model struct {
	Name     string  `yaml:"name"`
	SQLTable string  `yaml:"sql_table"`
	Storage  Storage `yaml:"storage,omitempty"`
	// SourceTable 是 live model 的**源库表名**。
	//
	// schema.sql_table 对 live model 无意义(它指的是 DuckDB 侧表名),
	// 透传时真正的 FROM 来自实例 config.yaml 的 table_* 配置 ——
	// 表名是 family 私有事实,不能写进共享 schema。
	Dimensions []Dimension `yaml:"dimensions"`
	Measures   []Measure   `yaml:"measures"`
	Segments   []Segment   `yaml:"segments,omitempty"`
	Joins      []Join      `yaml:"joins,omitempty"`
}

// EffectiveStorage 返回实际存储策略,未声明时按 duck 处理(保持既有 model 行为不变)。
func (m *Model) EffectiveStorage() Storage {
	if m.Storage == "" {
		return StorageDuck
	}
	return m.Storage
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
	switch m.EffectiveStorage() {
	case StorageDuck, StorageLive:
	default:
		return nil, fmt.Errorf("cubeschema: model %q: unknown storage %q (want duck|live)", m.Name, m.Storage)
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
	// segment 校验:名字唯一 + SQL 非空 + **不得含占位符**。
	//
	// 最后一条是必须的:segment 的 SQL 是原样内联进 WHERE 的,不携带参数。
	// 里面写一个 `?` 会变成一个无人绑定的占位符 —— 要么驱动报错,
	// 要么更糟:把别的 filter 的参数绑上去,查询结果错得无声无息。
	// 这属于"在 schema 加载时就拦住"比"运行时才发现"便宜得多的一类。
	seenSeg := map[string]bool{}
	for i := range m.Segments {
		sg := &m.Segments[i]
		if sg.Name == "" {
			return nil, fmt.Errorf("cubeschema: model %q: segment #%d has empty name", m.Name, i)
		}
		if seenSeg[sg.Name] {
			return nil, fmt.Errorf("cubeschema: model %q: duplicate segment %q", m.Name, sg.Name)
		}
		if strings.TrimSpace(sg.SQL) == "" {
			return nil, fmt.Errorf("cubeschema: model %q: segment %q has empty sql", m.Name, sg.Name)
		}
		if strings.Contains(sg.SQL, "?") {
			return nil, fmt.Errorf("cubeschema: model %q: segment %q 的 sql 不得含 `?`"+
				"(segment 不携带参数,占位符会变成无人绑定的位置)", m.Name, sg.Name)
		}
		// 也不得含子查询。理由在 fieldmapping.Rewrite:它对整段 SQL 做**标识符**
		// 级别的盲替换,子查询里属于另一张表的同名列也会被一起改掉 ——
		// 碰巧对上就是对的,对不上就是错的,而且不报错。
		// segment 应当只是"本表上的一个谓词";真要跨表,那是 join 的事,
		// 应当显式声明关联,而不是藏在 segment 里偷偷扩表。
		if hasSubqueryRE.MatchString(sg.SQL) {
			return nil, fmt.Errorf("cubeschema: model %q: segment %q 的 sql 不得含子查询"+
				"(跨表请显式声明 join;列名重写是标识符级的盲替换,子查询会误伤)",
				m.Name, sg.Name)
		}
		seenSeg[sg.Name] = true
	}
	// join 校验:名字唯一 + 关联方向在白名单内。
	//
	// 方向写错(比如把 one_to_many 写成 many_to_one)不会让 SQL 报错,
	// 只会让扇出被静默当成查档案 —— 所以在加载时就拒。
	seenJoin := map[string]bool{}
	for i := range m.Joins {
		j := &m.Joins[i]
		if j.Name == "" {
			return nil, fmt.Errorf("cubeschema: model %q: join #%d has empty name", m.Name, i)
		}
		if seenJoin[j.Name] {
			return nil, fmt.Errorf("cubeschema: model %q: duplicate join %q", m.Name, j.Name)
		}
		if strings.TrimSpace(j.On) == "" {
			return nil, fmt.Errorf("cubeschema: model %q: join %q has empty on", m.Name, j.Name)
		}
		if !validRelationships[j.Relationship] {
			return nil, fmt.Errorf("cubeschema: model %q: join %q relationship %q 非法"+
				"(want many_to_one|one_to_many|one_to_one)", m.Name, j.Name, j.Relationship)
		}
		seenJoin[j.Name] = true
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

// FindSegment 按名查预定义过滤片段。
func (m *Model) FindSegment(name string) (*Segment, bool) {
	for i := range m.Segments {
		if m.Segments[i].Name == name {
			return &m.Segments[i], true
		}
	}
	return nil, false
}

// SegmentNames 返回所有 segment 名。
func (m *Model) SegmentNames() []string {
	out := make([]string, 0, len(m.Segments))
	for _, s := range m.Segments {
		out = append(out, s.Name)
	}
	return out
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
