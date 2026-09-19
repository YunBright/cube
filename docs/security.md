# 权限模型(P1-6 选 B)

## 分层

```
BI 工具发请求
    │ JWT / cookie
    ▼
┌──────────────────────────┐
│   cube-gateway           │
│                          │
│   粗粒度:谁能访问 model  │ ← 权限:RBAC(角色 ↔ model)
└────────────┬─────────────┘
             │ dapr invocation
             │ metadata: x-principal / x-tenant
             ▼
┌──────────────────────────┐
│   dapr cube app          │
│                          │
│   细粒度:谁能看哪些行/列 │ ← 权限:行级 + 列级
└──────────────────────────┘
```

## Principal 透传

Dapr invocation 自定义 metadata:

```
x-principal: <principal_id>
x-tenant:    <tenant_id>
x-trace-id:  <trace_id>
```

由 cube-gateway HTTP middleware 注入 ctx,经 dapr invocation 透传到 cube app,再从 invocation metadata 提取回 ctx。

实现见 `pkg/daprclient/daprclient.go`:

```go
// 中间件提取 → ctx
ctx := daprclient.WithPrincipal(r.Context(), principalID)
ctx = daprclient.WithTenant(ctx, tenantID)

// invocation 透传
dapr.InvokeMethod(ctx, targetAppID, "/query", body, nil)
// → 自动把 ctx 里的 principal / tenant 写入 metadata

// cube app 接收 → 提取
func (a *CubeApp) QueryHandler(w, r) {
    md := dapr.MetadataFromRequest(r)
    p := daprclient.PrincipalFromMetadata(md)
    t := daprclient.TenantFromMetadata(md)
}
```

## Authorizer 接口

`pkg/auth/auth.go`:

```go
type Authorizer interface {
    Allow(p *Principal, model string) bool        // gateway 用
    FilterRow(p, model, row) bool                 // cube app 行级
    FilterColumns(p, model) []string              // cube app 列级
}
```

各实例在 `internal/auth/` 实现:

```go
// semantic-layers/sixun/cmd/sixun-hbposv7/auth.go
type Auth struct{}

func (a *Auth) Allow(p *auth.Principal, model string) bool {
    if !p.HasRole("viewer") { return false }
    return true
}

func (a *Auth) FilterRow(p *auth.Principal, model string, row map[string]any) bool {
    // 租户隔离:只返回本租户的 supplier
    if row["tenant_id"] != p.Tenant { return false }
    return true
}

func (a *Auth) FilterColumns(p *auth.Principal, model string) []string {
    // 经理角色看 contact_phone,普通员工看不到
    if p.HasRole("manager") { return nil } // nil = 全部
    return []string{"id", "name", "category"} // 否则隐藏 phone
}
```

## MVP 限制

- ❌ 不接外部 IDP(OAuth / LDAP)— MVP 用 JWT 自签,key 在 dapr secrets
- ❌ 不做 column-level mask(脱敏)— P2
- ❌ 不做 ABAC / 复杂表达式(只 RBAC + 行/列)— P2