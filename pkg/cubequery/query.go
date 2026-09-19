// Package cubequery 解析 + 构建 cube query(/v1/load body)。
//
// 设计要点:
//   - 输入是 cube query JSON(BI 工具发出的)
//   - 输出是 (model, dimensions, measures, filters, time_dimensions, limit)
//   - 由 gateway 解析后路由到对应 cube app
package cubequery

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Query 是 /v1/load body 的最小子集(只支持最常用字段)。
type Query struct {
	Measures       []string      `json:"measures"`
	Dimensions     []string      `json:"dimensions"`
	Filters        []Filter      `json:"filters,omitempty"`
	TimeDimensions []TimeDim     `json:"timeDimensions,omitempty"`
	Limit          *int          `json:"limit,omitempty"`
	Offset         []any         `json:"offset,omitempty"`
	Order          []Order       `json:"order,omitempty"`
	Segments       []string      `json:"segments,omitempty"`
	// 暂不实现:drillMembers / renewQuery / ungrouped 等高级字段
}

// Filter 是 where 条件。
type Filter struct {
	Member    string `json:"member"`
	Operator  string `json:"operator"` // equals / in / gt / lt / contains ...
	Values    []any  `json:"values,omitempty"`
}

// TimeDim 是时间维度(常用于按日/月聚合)。
type TimeDim struct {
	Dimension  string `json:"dimension"`
	DateRange  any    `json:"dateRange,omitempty"`    // string | []string
	Granularity string `json:"granularity,omitempty"` // day / week / month
}

// Order 是排序。
type Order struct {
	ID       string `json:"id"`
	Order    string `json:"order"` // asc / desc
}

// Parse 解析 cube query JSON。
func Parse(body []byte) (*Query, error) {
	if len(body) == 0 {
		return nil, errors.New("empty body")
	}
	var q Query
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, fmt.Errorf("parse cube query: %w", err)
	}
	return &q, nil
}

// Model 推断该 query 用的 model(从 measure / dimension 前缀推断)。
//
// cube 约定:dimension / measure 名是 "<model>.<field>"。
func (q *Query) Model() string {
	for _, m := range q.Measures {
		if i := indexDot(m); i >= 0 {
			return m[:i]
		}
	}
	for _, d := range q.Dimensions {
		if i := indexDot(d); i >= 0 {
			return d[:i]
		}
	}
	return ""
}

func indexDot(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}