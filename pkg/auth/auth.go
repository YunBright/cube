// Package auth 提供 principal 模型与权限接口。
//
// 设计要点(P1-6):
//   - Principal 经 Dapr invocation metadata 透传,字段固定:x-principal / x-tenant
//   - gateway 判粗粒度(谁能访问 model)
//   - cube app 判细粒度(谁能看哪些行/列),由各 app 实现
package auth

// Principal 代表调用方身份。
type Principal struct {
	ID    string   // 用户/服务 ID
	Tenant string   // 租户(多租户隔离)
	Roles  []string // 角色(RBAC)
	Scopes []string // scope(BI 工具授权)
}

// HasRole 判断是否有指定角色。
func (p *Principal) HasRole(r string) bool {
	for _, x := range p.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// HasScope 判断是否有指定 scope。
func (p *Principal) HasScope(s string) bool {
	for _, x := range p.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

// Authorizer 是权限判断接口。
//
// Implementations:
//   - gateway:ModelAuthorizer(谁能访问 model)
//   - cube app:RowColumnAuthorizer(行/列过滤)
type Authorizer interface {
	// Allow 返回是否允许 principal 执行 query。
	Allow(p *Principal, model string) bool
	// FilterRow 返回该行 principal 是否可见(行级权限)。
	// 返回 false 表示这行不应出现在结果里。
	FilterRow(p *Principal, model string, row map[string]any) bool
	// FilterColumns 返回 principal 可见的列名(列级权限)。
	// 返回 nil 表示全部可见。
	FilterColumns(p *Principal, model string) []string
}

// TODO: 各 dapr cube app 在 internal/auth/ 实现 ModelAuthorizer + RowColumnAuthorizer。