// total.go 把编译器注入的"总行数"列还原成响应里的一个标量。
//
// 为什么要有这一步:total 是用 `COUNT(*) OVER()` 混在 SELECT 里算出来的,
// 它必须活在 SELECT 中(否则要么多打一次源库,要么套一层子查询)。
// 但它**不该出现在调用方的数据行里** —— 每行都重复一个 `__cube_total`,
// 调用方还得自己判断该不该用它,那是把实现细节漏给了使用方。
package cubequery

import (
	"encoding/json"
	"strconv"
)

// ExtractTotal 从结果行里取出总行数,并把 TotalField 从每一行删除。
//
// want=false 时原样返回 rows(不做任何遍历) —— 没请求 total 就不该有这笔开销。
//
// 空结果集返回 total=0:查询确实没有匹配行,这是"0"而不是"未知"。
// ⚠️ 这里的 0 只在 want=true 时有意义;want=false 时 total 一律是 0,
// 调用方必须靠 want 判断该不该展示它,不能拿 0 当"总共 0 条"。
func ExtractTotal(rows []map[string]any, want bool) (int, []map[string]any) {
	if !want {
		return 0, rows
	}
	total := 0
	if len(rows) > 0 {
		if v, ok := rows[0][TotalField]; ok {
			total = normalizeCount(v)
		}
	}
	// 每一行都要删 —— 驱动按行返回,只有第一行被读到过。
	for _, r := range rows {
		delete(r, TotalField)
	}
	return total, rows
}

// normalizeCount 把驱动返回的计数归一成 int。
//
// go-mssqldb 与 duckdb 驱动返回的数值类型并不一致(可能是 int64、[]byte、
// json.Number 甚至 float64),之前的 source.ScanRows 就踩过 numeric 被当成
// []byte 打印成字节码的坑。这里宁可多写几行也不能让 total 变成 "[49 48]"。
func normalizeCount(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	case []byte:
		i, err := strconv.Atoi(string(n))
		if err != nil {
			return 0
		}
		return i
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}
