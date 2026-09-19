# Dapr App 协议

## 1. 注册协议(P0-1)

dapr cube app 启动后调 cube-gateway:

```
POST cube-gateway/register
Content-Type: application/json

{
  "app_id": "sixun-hbposv7",      // dapr app id
  "family": "sixun",              // 数据源家族
  "version": "hbposv7",           // 版本
  "models": ["supplier", "product", "order"],
  "capabilities": ["query", "preagg", "cache_l2"],
  "health_url": "/health",
  "registered_at": "2026-09-17T00:00:00Z"
}

204 No Content
```

cube-gateway 写入 in-memory + dapr state store `cube-statestore`。

## 2. 查询协议(P1-8)

cube-gateway 路由后调 dapr cube app:

```
POST <target_app_id>/query
Content-Type: application/json
x-principal: <principal_id>     // P1-6:metadata 透传身份
x-tenant: <tenant_id>
x-trace-id: <trace_id>

<cube query JSON>

{
  "data": [...],
  "annotation": {...}
}
```

## 3. /v1/load(P1-8 选 B,不含 /v1/sql)

cube 兼容查询请求体:

```json
{
  "measures": ["supplier.count"],
  "dimensions": ["supplier.category"],
  "filters": [
    { "member": "supplier.category", "operator": "equals", "values": ["食品"] }
  ],
  "timeDimensions": [
    { "dimension": "supplier.registered_at", "granularity": "month" }
  ],
  "limit": 100
}
```

## 4. /v1/meta(P1-8)

```json
{
  "cube_version": "1.0",
  "apps": [
    { "app_id": "sixun-hbposv7", "family": "sixun", "models": [...], ... },
    { "app_id": "sixun-ysx", "family": "sixun", "models": [...], ... }
  ]
}
```

## 5. cube-compiler reload 协议

cube-compiler 编译完一个 cube app 后,通过 pubsub 通知 cube-gateway:

```
Topic: cube.reload
Payload: { "app_id": "sixun-hbposv7", "action": "reload" }
```

cube-gateway 收到后调本地 registry.Reload(appID),下次该 app 注册时会刷新元信息。

## Dapr building blocks 利用

| Building block | 用途 |
|---|---|
| Service Invocation | gateway ↔ cube app / cube app ↔ cube app |
| State Store (`cube-statestore`) | 注册表元信息 |
| PubSub (`cube-pubsub`) | reload 通知 |
| Secrets (`cube-secrets`) | 数据源 DSN |
| Distributed Lock | (P2)预聚合并发构建互斥 |