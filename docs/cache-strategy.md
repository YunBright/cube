# 缓存策略(P1-7 选 A)

## 两层缓存

```
BI 请求
   │
   ▼
┌─────────────────┐
│  cube-gateway   │
│                 │
│  L1 缓存        │ ← sync.Map(进程内)
│                 │   key = md5(model+query+principal+tenant+freshness)
│  命中直接返回 ✓  │   TTL = 60s(可配)
└────────┬────────┘
         │ miss
         ▼ dapr invocation
┌─────────────────┐
│  dapr cube app  │
│                 │
│  L2 缓存        │ ← duckdb 文件 + 内存表
│                 │   key = 同 L1
│  miss → DuckDB  │
└─────────────────┘
```

## P1-7 选 A:L1 命中直接返回

**完全跳过 dapr invocation**,最快最省资源。

代价:多 gateway 实例时缓存不共享(MVP 接受,P2 用 dapr state store 做分布式 L1)。

## Key 设计

```
md5(
  model                  // "supplier"
  + cube_query_json      // 完整 query body
  + principal_id         // 避免跨用户泄漏
  + tenant_id            // 多租户隔离
  + freshness_tag        // "v1" 或 schema hash,改 schema 后失效
)
```

实现见 `pkg/cache/cache.go`:

```go
type KeyBuilder struct{}

func (k *KeyBuilder) Build(model string, query []byte, principal, tenant, freshness string) string {
    h := md5.New()
    h.Write([]byte(model))
    h.Write([]byte{'\\x00'})
    h.Write(query)
    h.Write([]byte{'\\x00'})
    h.Write([]byte(principal))
    h.Write([]byte{'\\x00'})
    h.Write([]byte(tenant))
    h.Write([]byte{'\\x00'})
    h.Write([]byte(freshness))
    return hex.EncodeToString(h.Sum(nil))
}
```

## 失效

| 触发 | 方式 |
|---|---|
| TTL 到期 | 被动 |
| cube app reload | cube-compiler → pubsub `cube.reload` → cube-gateway 失效 model 全 key |
| 预聚合刷新 | P2:pub sub 失效 L1 |

## 响应头

`X-Cube-Cache: L1-HIT` / `X-Cube-Cache: L1-MISS`,BI 工具可观察缓存效率。

## P2 升级路径

- L1 进程内 → dapr state store(Redis),多 gateway 实例共享
- stale-while-revalidate(L1 命中先返回旧值,后台异步刷新)
- L2 加 per-tenant 分片(避免大租户污染小租户缓存)