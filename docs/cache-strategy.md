# 缓存策略

> ⚠️ **2026-10-23:本文件改写过一轮。**
> 原来的"两层缓存"里的 **L2(DuckDB 文件 + 内存表)已经整个删除** ——
> 全部 model 都是 `storage: live`,查询时把 schema 编译成 T-SQL 直打源库,
> **不保留任何本地副本**。
> 规矩与理由见 **AGENTS.md §4.1**(禁止给 cube 加数据缓存 / 原始数据同步)。

## 现在只有一层缓存(L1,进程内)

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
│  编译成 T-SQL    │ ← 直打思迅源库(不落盘、不缓存)
│  查源库          │
└─────────────────┘
```

**L1 命中直接返回**(完全跳过 dapr invocation),最快最省资源。

代价:多 gateway 实例时缓存不共享(MVP 接受,P2 用 dapr state store 做分布式 L1)。

⚠️ **L1 的 60s TTL 是"同一份数据最多旧 60 秒"的唯一缓冲** ——
L2 没了之后,数据新鲜度 = 源库的新鲜度 + L1 的 TTL。

## 为什么没有 L2(不要把它加回来)

| | 直查源库 | 曾经走 DuckDB |
|---|---|---|
| product 按主键 | **6ms** | ~6ms |
| product 关键词全表扫 | **66ms** | 8~13ms |
| member 四表 JOIN(余额) | **73ms** | ~1ms(副本已算好) |
| member 单表 / COUNT(*) | **1ms / 24ms** | — |

⇒ 缓存换来的是**毫秒级**,付出的是:

1. **两份真相,而且错得无声**(副本过期 ⇒ POS 按旧价收银 ⇒ 没有一行报错);
2. **同步就是原始数据同步**(源库是生产库,反复全量重拉是压力,不是设计);
3. **一整套会悄悄坏掉的机制**(水位 / 校准 job / 启动全量 / 新鲜度健康段)。

⇒ 详见 AGENTS.md §4.1(规矩)、§4.1.2(实测代价表)、§4.1.3(缓存真正的代价)。

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
    h.Write([]byte{'\x00'})
    h.Write(query)
    h.Write([]byte{'\x00'})
    h.Write([]byte(principal))
    h.Write([]byte{'\x00'})
    h.Write([]byte(tenant))
    h.Write([]byte{'\x00'})
    h.Write([]byte(freshness))
    return hex.EncodeToString(h.Sum(nil))
}
```

## 失效

| 触发 | 方式 |
|---|---|
| TTL 到期 | 被动 |
| cube app reload | cube-compiler → pubsub `cube.reload` → cube-gateway 失效 model 全 key |
| ~~预聚合刷新~~ | **已作废**(没有预聚合了) |

## 响应头

`X-Cube-Cache: L1-HIT` / `X-Cube-Cache: L1-MISS`,BI 工具可观察缓存效率。

## P2 升级路径

- L1 进程内 → dapr state store(Redis),多 gateway 实例共享
- stale-while-revalidate(L1 命中先返回旧值,后台异步刷新)
- ~~L2 加 per-tenant 分片~~ → **已作废**(不再有 L2)
