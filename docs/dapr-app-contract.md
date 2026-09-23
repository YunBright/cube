# Dapr App 协议(v2)

> 替换旧版的 `/v1/load` + `/v1/meta` 路由与无错误分类。
> 新版按 **source → app_id (1:1)** 直接转发,统一机器可读错误信封。

## 1. Source 寻址模型

每个 dapr cube app 进程有一个 **source id**(同时是 dapr app-id),格式:

```
<family>-<version>-<instance>
   ^       ^         ^
   |       |         └─ 门店 / 租户别名(必须 ≥ 1 段)
   |       └─ 版本短码(hbposv7 / ysx / ...)
   └─ 家族名(sixun / liangyou / ...)

正则: ^[\w-]+-[\w-]+-[\w-]+$
```

示例:
- `sixun-ysx-00`        ─ 思迅云商x 第 0 号实例
- `sixun-ysx-baiyuan1`  ─ 思迅云商x 百源 1 号店实例
- `sixun-hbposv7-jiale` ─ 思迅7pro 家乐门店实例

> gateway **只校验 3 段正则**,**不解析** family / version。
> 字段冗余写入 /register body,便于外部 inspect(boot 包从 `CUBE_APP_ID` 自动派生)。

## 2. 注册协议(POST /register)

dapr cube app 启动后调 cube-gateway:

```
POST cube-gateway/register
Content-Type: application/json

{
  "app_id": "sixun-hbposv7-jiale",    // == source
  "family": "sixun",                  // 从 app_id 拆分得到(冗余)
  "version": "hbposv7",               // 从 app_id 拆分得到(冗余)
  "source": "sixun-hbposv7-jiale",    // == app_id
  "models": ["supplier", "product", "category", "sale_detail", "stock"],
  "capabilities": ["query", "preagg", "cache_l2"],
  "health_url": "/healthz"
}

204 No Content      // 注册成功
400 Bad Request     // 错误信封 SOURCE_FORMAT_INVALID(app_id 不匹配正则)
```

gateway 写入 in-memory registry + dapr state store `cube-statestore`,key=`registry:<app_id>`。
**LastSeen 在每次成功 /v1/source/{source}/load 时刷新**;超过 90s 未见 → 标记 offline。

## 3. 查询协议(POST /v1/source/{source}/load)

### 客户端调用

```
POST cube-gateway/v1/source/sixun-ysx-00/load
Content-Type: application/json
Authorization: Bearer <principal>       // NO_PRINCIPAL 检查
X-Request-Id: <optional>               // 不传则 gateway 自动生成

<cube query JSON>
```

`<cube query JSON>` 同 v1 cube.js 兼容查询:

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

### 成功响应

```
HTTP/1.1 200 OK
Content-Type: application/json
X-Cube-Cache: L1-MISS | L1-HIT
X-Request-Id: <echoed>

{
  "data": [...],
  "annotation": {...}
}
```

### gateway → cube app(internal dapr invocation)

```
POST <source>/query
Content-Type: application/json
dapr-app-id: <source>
x-principal: <principal_id>           // 从 Authorization 解出
x-tenant: <tenant_id>
x-request-id: <request_id>           // 端到端传递

<cube query JSON>

{ "data": [...], ... }
```

超时:`context.WithTimeout(10s)` 在 handler 层控制;dapr sidecar 与后端 cube app 复用该 ctx。

## 4. 元信息接口(GET /v1/sources)

```json
{
  "sources": [
    {
      "source":     "sixun-ysx-00",
      "family":     "sixun",
      "version":    "ysx",
      "models":     ["supplier", "product", "category", "sale_detail", "stock"],
      "capabilities": ["query", "preagg", "cache_l2"],
      "status":     "online",              // online | offline
      "last_seen":  "2026-09-23T10:30:00Z"
    },
    {
      "source":     "sixun-hbposv7-jiale",
      "family":     "sixun",
      "version":    "hbposv7",
      "models":     [...],
      "capabilities": [...],
      "status":     "offline",
      "last_seen":  "2026-09-23T09:50:00Z"
    }
  ]
}
```

## 5. 错误信封(所有非 2xx 响应)

```json
{
  "code":    "SOURCE_NOT_REGISTERED",
  "message": "source not registered: sixun-ysx-99",
  "details": { "source": "sixun-ysx-99", "request_id": "abc123" }
}
```

所有错误响应都带 `X-Request-Id` header。

### 错误码表

| Code | HTTP | 触发条件 |
|---|---|---|
| `SOURCE_FORMAT_INVALID`     | 400 | URL `{source}` 段不匹配 `^[\w-]+-[\w-]+-[\w-]+$` |
| `SOURCE_NOT_REGISTERED`     | 404 | 没收到过该 source 的 /register |
| `SOURCE_OFFLINE`            | 503 | LastSeen 超过 90s **或** dapr 抛 ConnFailure **或** cube app 返回 404 |
| `UPSTREAM_TIMEOUT`          | 504 | ctx deadline exceeded |
| `VERSION_UNSUPPORTED`       | 400 | cube app 返回 `{"code":"VERSION_UNSUPPORTED",…}` |
| `MODEL_NOT_FOUND_IN_SOURCE` | 404 | cube app 返回 `{"code":"MODEL_NOT_FOUND",…}` |
| `QUERY_PARSE_ERROR`         | 400 | body 读不出来 / JSON 无效 |
| `QUERY_INVALID`             | 400 | JSON OK 但 model 字段空 / measures 为空 |
| `NO_PRINCIPAL`              | 401 | `cfg.AuthRequired && Authorization` header 缺失 |
| `FORBIDDEN`                 | 403 | `Authorizer.Allow` 返回 false |
| `RATE_LIMITED`              | 429 | (P2,当前未 wire) |
| `INTERNAL_ERROR`            | 500 | gateway panic |
| `UPSTREAM_ERROR`            | 502 | 其他非 2xx 上游响应;`details.upstream_status` + 截断 512B `details.upstream_body` |

### cube app 4xx 子码协议

cube app 的 /query 在 4xx 响应里 emit `code` 子码:

```json
{ "code": "MODEL_NOT_FOUND", "message": "...", "details": { ... } }
```

子码 → gateway 错误码映射:

| cube app 子码 | gateway 错误码 | HTTP |
|---|---|---|
| `MODEL_NOT_FOUND`      | `MODEL_NOT_FOUND_IN_SOURCE` | 404 |
| `VERSION_UNSUPPORTED`  | `VERSION_UNSUPPORTED`       | 400 |
| `QUERY_PARSE_ERROR`    | `QUERY_PARSE_ERROR`         | 400 |
| `QUERY_INVALID`        | `QUERY_INVALID`             | 400 |
| (其他 / 缺省)            | `UPSTREAM_ERROR`            | 502 |

gateway 用 `errors.As(err, &invErr)` + body 文本扫描(`"MODEL_NOT_FOUND"` 子串)做匹配。

## 6. cube-compiler reload 协议

cube-compiler 编译完一个 cube app 后,通过 pubsub 通知 cube-gateway:

```
Topic: cube.reload
Payload: { "app_id": "sixun-hbposv7-jiale", "action": "reload" }
```

cube-gateway 收到后调 `registry.Reload(appID)`,下次该 app 注册时会刷新元信息。

## 7. 健康检查

- gateway `GET /healthz` → 200 `{"status":"ok"}`
- cube app `GET /healthz` → 200 `{"status":"ok","source":"...","family":"...","version":"...","models":[...],"uptime":"..."}`
- dapr sidecar 自动做 liveness / readiness probe

## 8. Dapr building blocks 利用

| Building block | 用途 |
|---|---|
| Service Invocation | gateway ↔ cube app / cube app ↔ cube app |
| State Store (`cube-statestore`) | 注册表元信息(per app_id) |
| PubSub (`cube-pubsub`) | reload 通知 |
| Secrets (`cube-secrets`) | 数据源 DSN |
| Distributed Lock | (P2)预聚合并发构建互斥 |
