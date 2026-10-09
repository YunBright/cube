// readguard.go 只读 SQL 校验。
//
// 为什么放在运行期的 connector 上(而不是只放在勘察工具里):
// 源库是**思迅的生产库**。此前只有 cmd/sqlq 这个勘察工具做了只读校验,
// 而运行期的 Connector.QueryLive 把 SQL 直接交给 database/sql ——
// 唯一的防线是"编译器不会生成写语句"。
//
// 单层防线不够:编译器是查询语义那一层,不该同时承担"防止误伤生产数据"的职责。
// 万一将来有人加一个 debug 端点把裸 SQL 透传下去,或者编译器某个分支
// 拼错了东西,这里是最后一道闸 —— 宁可 500,也不能对生产库做写操作。
//
// 判定方式与 cmd/sqlq 一致:出现写关键字即拒。误杀的方向是"拒绝一条合法的
// SELECT",那只是查不了;漏放的方向是"改了生产库的数据",不可逆。
package source

import (
	"errors"
	"regexp"
	"strings"
)

// forbiddenRe 挡住一切写操作。
var forbiddenRe = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|alter|create|truncate|merge|exec|execute|grant|revoke|backup|restore)\b`)

// ErrNotReadOnly 是只读校验失败的错误。
var ErrNotReadOnly = errors.New("source: statement is not read-only")

// AssertReadOnly 校验 SQL 是只读的。
//
// 同时要求以 SELECT 开头 —— 只查写关键字会漏掉 `1; DROP TABLE x`
// 这种靠分号续接的构造(T-SQL 允许 GO 批处理,但单条语句里
// 编译器不会产生分号;这里作为额外一道,代价为零)。
func AssertReadOnly(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return errors.New("source: empty statement")
	}
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT") {
		return ErrNotReadOnly
	}
	// 分号只能出现在末尾(允许尾随分号),中间出现说明是多语句拼接。
	body := strings.TrimRight(trimmed, "; \t\r\n")
	if strings.Contains(body, ";") {
		return ErrNotReadOnly
	}
	if m := forbiddenRe.FindString(body); m != "" {
		return errors.New("source: statement is not read-only (forbidden keyword: " + m + ")")
	}
	return nil
}
