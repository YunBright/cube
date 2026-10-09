// Package meta 生成 cube app 的 /meta 响应。
//
// 为什么要有它:一个 source 上有十几个 model、每个几十个 member,
// 调用方(supertrade / BI 工具)没法靠人肉记住字段名。没有它,
// 加一个字段就得靠"改代码—部署—看报错"的循环发现名字写错了。
//
// 放在共用包而不是各 main.go 各写一份,理由同 pkg/cubequery:
// 两个 family 的 /meta 若各写一份,必然出现"ysx 的 meta 少了 segments、
// hbposv7 的多了 char_date"这种只有比对才发现的分叉。
package meta

import (
	"github.com/YunBright/cube/pkg/cubeschema"
)

// ModelInfo 是一个 model 对外暴露的元信息。
type ModelInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Storage     string          `json:"storage"`                // duck | live
	SourceTable string          `json:"source_table,omitempty"` // 仅 live:实例 config 里的源表名
	Dimensions  []DimensionInfo `json:"dimensions"`
	Measures    []MeasureInfo   `json:"measures"`
	Segments    []string        `json:"segments,omitempty"`
	Joins       []string        `json:"joins,omitempty"`
}

// DimensionInfo 是维度元信息。
type DimensionInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// CharDate 让调用方知道:这一列是 char 定长日期串,按字符串比较就对了。
	// 不暴露的话,调用方可能自己写一套 CAST,把 sargable 的过滤变成全表扫描。
	CharDate   bool                   `json:"char_date,omitempty"`
	EnumValues []cubeschema.EnumValue `json:"enum_values,omitempty"`
}

// MeasureInfo 是度量元信息。
type MeasureInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Title       string `json:"title,omitempty"`
	Format      string `json:"format,omitempty"`
	Description string `json:"description,omitempty"`
}

// ModelSource 是调用方提供的一个 model 的定位信息。
type ModelSource struct {
	Schema *cubeschema.Model
	// Live 为真表示该 model 是透传源库的(storage: live)。
	Live bool
	// SourceTable 仅 live 需要:实例 config 里的源表名。
	SourceTable string
}

// Build 把 schema 集合转成可序列化的元信息。
func Build(models map[string]ModelSource) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, src := range models {
		m := src.Schema
		if m == nil {
			continue
		}
		info := ModelInfo{
			Name:    m.Name,
			Storage: string(m.EffectiveStorage()),
			// SourceTable 是 family 私有事实,只在这里出现,不进共享 schema。
			SourceTable: src.SourceTable,
		}
		for _, d := range m.Dimensions {
			info.Dimensions = append(info.Dimensions, DimensionInfo{
				Name:        d.Name,
				Type:        string(d.Type),
				Title:       d.Title,
				Description: d.Description,
				CharDate:    d.CharDate,
				EnumValues:  d.EnumValues,
			})
		}
		for _, ms := range m.Measures {
			info.Measures = append(info.Measures, MeasureInfo{
				Name:        ms.Name,
				Type:        string(ms.Type),
				Title:       ms.Title,
				Format:      ms.Format,
				Description: ms.Description,
			})
		}
		info.Segments = m.SegmentNames()
		for _, j := range m.Joins {
			info.Joins = append(info.Joins, j.Name)
		}
		out = append(out, info)
	}
	return out
}
