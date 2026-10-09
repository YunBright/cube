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
	"strings"
)

// Query 是 /v1/load body 的最小子集(只支持最常用字段)。
type Query struct {
	Measures       []string  `json:"measures"`
	Dimensions     []string  `json:"dimensions"`
	Filters        []Filter  `json:"filters,omitempty"`
	TimeDimensions []TimeDim `json:"timeDimensions,omitempty"`
	Limit          *int      `json:"limit,omitempty"`
	Offset         Offset    `json:"offset,omitempty"`
	Order          []Order   `json:"order,omitempty"`
	Segments       []string  `json:"segments,omitempty"`
	// Total 为 true 时返回"不受 limit/offset 影响的总行数"。
	// 没有它,前端只能靠"取满一页再翻"猜有没有下一页 —— 那是假分页。
	Total bool `json:"total,omitempty"`
	// 暂不实现:drillMembers / renewQuery / ungrouped 等高级字段
}

// TotalField 是编译器注入的"总行数"列名。
//
// 用一个带前缀的保留名而不是混进业务列:handler 读出它之后要把每一行里的
// 这一列**删掉**,否则调用方会看到每行都重复着一个 `__cube_total`,
// 还得自己判断该不该用它。
const TotalField = "__cube_total"

// Offset 是分页偏移(要跳过的初始行数,cube 契约里默认 0)。
//
// 用自定义 UnmarshalJSON 而不是 `Offset int`:契约里它是**数字**(`"offset": 50`),
// 但部分客户端会发数组形态。两种都收 —— 收到不认识的形态**必须报错**,
// 静默当 0 会让调用方拿到第一页却以为翻到了第二页。
type Offset []int

func (o *Offset) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" || raw == "null" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []int
		if err := json.Unmarshal(b, &arr); err != nil {
			return fmt.Errorf("offset: %w", err)
		}
		for _, v := range arr {
			if v < 0 {
				return errors.New("offset must be >= 0")
			}
		}
		*o = arr
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("offset must be a number or an array of numbers: %w", err)
	}
	if n < 0 {
		return errors.New("offset must be >= 0")
	}
	*o = Offset{n}
	return nil
}

// First 取第一个偏移值;未给或给空数组返回 0。
func (o Offset) First() int {
	if len(o) == 0 {
		return 0
	}
	return o[0]
}

// Filter 是 where 条件。
type Filter struct {
	Member   string `json:"member"`
	Operator string `json:"operator"` // equals / in / gt / lt / contains ...
	Values   []any  `json:"values,omitempty"`
}

// TimeDim 是时间维度(常用于按日/月聚合)。
type TimeDim struct {
	Dimension   string `json:"dimension"`
	DateRange   any    `json:"dateRange,omitempty"`   // string | []string
	Granularity string `json:"granularity,omitempty"` // day / week / month
}

// Order 是排序。
//
// 语义规则见 build.go 的 buildOrder(两条编译路径共用):
//   - ID 格式 `<model>.<ref>`,可指向 dimension 或 measure,且**必须也在本次 SELECT 里**
//   - Order 取 asc / desc(大小写不敏感,省略按 asc),其它值编译期报错
//
// 不给 order 时 limit 是任意截取 —— 列表类场景必须显式排序。
type Order struct {
	ID    string `json:"id"`
	Order string `json:"order"` // asc / desc
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
