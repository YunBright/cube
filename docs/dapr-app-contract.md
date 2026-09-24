# Dapr App 协议(v2 — plan B 解耦版)

> v2 起按 **wire source id → dapr app-id** 解耦寻址:URL 上的 source 与
> dapr sidecar 的 app-id 可以不同,默认 cube app 启动时把 `DAPR_APP_ID`
> (dapr 自动注入)或 `CUBE_DAPR_APP_ID` 上报给 gateway。13 → 14 个错误码
> (新增 `DAPR_APP_ID_INVALID`)。

## 1. Source 寻址模型(双 id)

每个 dapr cube app 进程有两个标识,**必须都满足 dapr 字符集 `^[a-z0-9-]+$`**:

| 字段 | 用途 | 字符集 | 示例 |
|---|---|---|---|
| `app_id` / `source` | **wire id** —— URL 段 `/v1/source/{source}/load` 用 | `^[\w-]+-[\w-]+-[\w-]+$` | `sixun-hbposv7-jiale` |
| `dapr_app_id` | dapr sidecar app-id,`dapr run --app-id` 设值,gateway 寻址时用 | `^[a-z0-9][a-z0-9-]*[a-z0-9]$` | `cube-sixun-hbposv7-jiale` |

### wire source id 格式(3 段):

```
<family>-<version>-<instance>
   ^       ^         ^
   |       |         └─ 门店 / 租户别名(必须 ≥ 1 段)
   |       └─ 版本短码(hbposv7 / ysx / ...)
   └─ 家族名(sixun / liangyou / ...)

正则: ^[\w-]+-[\w-]+-[\w-]+$
```

### 默认映射约定(运维不需配)

| wire id | dapr app-id | 含义 |
|---|---|---|
| `sixun-ysx-00`        | `cube-sixun-ysx-00`        | 思迅云商x 第 0 号 |
| `sixun-ysx-fb`        | `cube-sixun-ysx-fb`        | 思迅云商x fb 实例 |
| `sixun-hbposv7-jiale` | `cube-sixun-hbposv7-jiale` | 思迅7pro 家乐店 |

`cube-` 前缀用于:dapr 命名空间隔离(同集群可能有非 cube app),运维 grep 一眼可见。

### 解耦动机

- **wire id 干净**:URL 上的 `sixun-hbposv7-jiale` 易记、易 grep、对客户端友好
- **dapr app-id 工程化**:`cube-sixun-hbposv7-jiale` 与同集群其他非 cube 应用天然不冲突
- **未来灵活**:dapr app-id 可以独立 rename / 蓝绿(如 `cube-sixun-hbposv7-jiale-v2`),不影响 URL

> gateway **只校验** source 的 3 段正则,**不解析** family / version。
> gateway **必须**校验 `dapr_app_id` 字符集(`^[a-z0-9-]+$`),非法 → 400 DAPR_APP_ID_INVALID。

## 2. 注册协议(POST /register + POST /unregister)

### 注册 — POST /register

dapr cube app 启动后调 cube-gateway:

```
POST cube-gateway/register
Content-Type: application/json

{
  "app_id":      "sixun-hbposv7-jiale",        // wire id(env CUBE_APP_ID)
  "dapr_app_id": "cube-sixun-hbposv7-jiale",    // dapr sidecar app-id(env DAPR_APP_ID 或 "cube-" + app_id)
  "family":      "sixun",                       // 从 app_id 拆分得到(冗余)
  "version":     "hbposv7",                     // 从 app_id 拆分得到(冗余)
  "source":      "sixun-hbposv7-jiale",         // == app_id(冗余,wire 一致)
  "models":      ["supplier", "product", "category", "sale_detail", "stock"],
  "capabilities":["query", "preagg", "cache_l2"],
  "health_url":  "/healthz"
}

204 No Content                 // 注册成功
400 Bad Request                // SOURCE_FORMAT_INVALID (app_id 不匹配正则)
                                // DAPR_APP_ID_INVALID (dapr_app_id 字符集非法)
```

gateway 写入 in-memory registry + dapr state store `cube-statestore`,key=`registry:<app_id>`。
**被动验证**:`LastSeen` 在每次成功 `/v1/source/{source}/load` 时刷新,但**不再驱动离线判定**;
handler 不再前置 IsOnline 检查,任何已注册 source 都直接调 dapr。
真实 `SOURCE_OFFLINE` 由 dapr 真实调用失败(`ErrConnFailure` / sidecar 找不到 app)触发。
详见 §7 与 [AGENTS.md](../AGENTS.md) "Source 在线判定"决策行。

### 反注册 — POST /unregister

cube app 收到 SIGTERM / SIGINT 时主动调 cube-gateway 取消注册,让 gateway 立即
从 registry + state store 删除条目,不再等下次 invoke 失败:

```
POST cube-gateway/unregister
Content-Type: application/json

{ "app_id": "sixun-hbposv7-jiale" }   // wire id(必填,3 段格式)

204 No Content    // 已删除,或原本就没注册(幂等)
400 Bad Request   // SOURCE_FORMAT_INVALID(app_id 格式不合法)
                   // QUERY_PARSE_ERROR( body 解析失败)
```

**幂等性**:未注册 source 也返 204(允许 cube app 在 SIGTERM 路径上无脑发,
不关心 gateway 当前状态)。持久化层失败仅记日志不阻塞响应。

**cube app 端调用约定**(cmd/sixun-{ysx,hbposv7}/main.go):

- `signal.Notify(SIGTERM, SIGINT)` → 触发 `unregisterFromGateway(cfg, lg)`
- `http.Client.Do` 带 `context.WithTimeout(2s)` —— **不可阻塞退出**,否则 kubelet 30s 后 SIGKILL
- 失败仅记日志,不重试,直接退出(配合被动验证,即便 unregister 失败,下次 invoke 也会被 dapr 真实失败拦下)

实现细节:见 `gateway/internal/handler/handler.go: Unregister` 与 `registry.Unregister`。

### cube app 端如何得到 `dapr_app_id`

`boot.Config.Load()` 三档优先级(首个非空胜出):

1. **`DAPR_APP_ID`** — dapr sidecar 注入的"事实源",最权威
2. **`CUBE_DAPR_APP_ID`** — 运维手动覆盖(罕见;裸起 / 调试时用)
3. **`"cube-" + CUBE_APP_ID`** — 推导默认值

绝大多数场景 cube app **不需要配置**:`dapr run --app-id cube-sixun-hbposv7-jiale -- ...`
启动时 dapr 会把 `DAPR_APP_ID=cube-sixun-hbposv7-jiale` 注入到 app 进程环境。

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
POST <dapr_app_id>/query                    // 注意:用 dapr_app_id 不是 source
Content-Type: application/json
dapr-app-id: <dapr_app_id>
x-principal: <principal_id>                 // 从 Authorization 解出
x-tenant: <tenant_id>
x-request-id: <request_id>                  // 端到端传递

<cube query JSON>

{ "data": [...], ... }
```

**关键**:gateway 按 **registry 里存的 `dapr_app_id`** 寻址,不是 URL 上的 source。
例:`POST /v1/source/sixun-hbposv7-jiale/load` → gateway 内部
`dapr.InvokeMethod(ctx, "cube-sixun-hbposv7-jiale", "/query", ...)`。

超时:`context.WithTimeout(10s)` 在 handler 层控制;dapr sidecar 与后端 cube app 复用该 ctx。

## 4. 元信息接口(GET /v1/sources)

```json
{
  "sources": [
    {
      "source":      "sixun-ysx-00",         // wire id
      "dapr_app_id": "cube-sixun-ysx-00",    // dapr 寻址用(plan B 新增)
      "family":      "sixun",
      "version":     "ysx",
      "models":      ["supplier", "product", "category", "sale_detail", "stock"],
      "capabilities":["query", "preagg", "cache_l2"],
      "status":      "online",                // online | offline
      "last_seen":   "2026-09-23T10:30:00Z"
    },
    {
      "source":      "sixun-hbposv7-jiale",
      "dapr_app_id": "cube-sixun-hbposv7-jiale",
      "family":      "sixun",
      "version":     "hbposv7",
      "models":      [...],
      "capabilities":[...],
      "status":      "offline",
      "last_seen":   "2026-09-23T09:50:00Z"
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
| `DAPR_APP_ID_INVALID`       | 400 | /register body 的 `dapr_app_id` 字符集非法(不匹配 `^[a-z0-9][a-z0-9-]*[a-z0-9]$`) |
| `SOURCE_NOT_REGISTERED`     | 404 | 没收到过该 source 的 /register |
| `SOURCE_OFFLINE`            | 503 | dapr 真实抛 `ErrConnFailure`(sidecar 死了 / placement 找不到 app)**或** cube app 返回 404 且 body 不含 `MODEL_NOT_FOUND` 子码 |
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

子码 → gateway 错误码映射(扫 body 子码,不依赖 HTTP 状态):

| cube app 子码 | cube app HTTP | gateway 错误码 | gateway HTTP |
|---|---|---|---|
| `MODEL_NOT_FOUND`      | 404 | `MODEL_NOT_FOUND_IN_SOURCE` | 404 |
| `VERSION_UNSUPPORTED`  | 400 | `VERSION_UNSUPPORTED`       | 400 |
| `QUERY_PARSE_ERROR`    | 400 | `QUERY_PARSE_ERROR`         | 400 |
| `QUERY_INVALID`        | 400 | `QUERY_INVALID`             | 400 |
| (其他 / 缺省)            | *   | `UPSTREAM_ERROR`            | 502 |

gateway 用 `errors.As(err, &invErr)` + body 文本扫描(`"MODEL_NOT_FOUND"` 子串)做匹配。
**关键**:对 404 响应也要先扫 body 子码 — cube app 按协议对未知 model 返 404 + MODEL_NOT_FOUND,
不能武断归类为 `SOURCE_OFFLINE`(那表示 dapr sidecar 找不到 app)。两种 404 的区分:

- body 含 `"code":"MODEL_NOT_FOUND"` → `MODEL_NOT_FOUND_IN_SOURCE` 404(业务)
- body 无该子码 → `SOURCE_OFFLINE` 503(dapr 路由不到 app,运维介入)

## 6. cube-compiler reload 协议

cube-compiler 编译完一个 cube app 后,通过 pubsub 通知 cube-gateway:

```
Topic: cube.reload
Payload: { "app_id": "cube-sixun-hbposv7-jiale", "action": "reload" }
```

注:reload payload 用的是 **dapr_app_id**(因为它是 dapr 集群内寻址的
统一身份),与 wire source id 不同。

cube-gateway 收到后调 `registry.Reload(appID)`,下次该 app 注册时会刷新元信息。

## 7. 健康检查 & 被动验证

- gateway `GET /healthz` → 200 `{"status":"ok"}`
- cube app `GET /healthz` → 200 `{"status":"ok","source":"sixun-hbposv7-jiale","dapr_app_id":"cube-sixun-hbposv7-jiale","family":"sixun","version":"hbposv7","models":[...],"uptime":"..."}`
- dapr sidecar 自动做 liveness / readiness probe

### 被动验证(plan B 修订)

**不再前置 IsOnline 检查**,handler 对已注册 source 一律直接 `dapr.InvokeMethod`:

```
请求 → LookupByID(source)
         │
// 不再:
// if !info.IsOnline(staleAfter) { 503 SOURCE_OFFLINE }
// (这条 90s 假离线判定已移除 —— 注册后无人调就假 offline,污染视图)
         │
       dapr.InvokeMethod(target=info.DaprAppID, "/query", body, extra)
         │
       ├── 成功 → MarkSeen(source) + L1 缓存 + 200 OK
       ├── ErrConnFailure → 503 SOURCE_OFFLINE (sidecar 死了 / placement 找不到)
       ├── ErrTimeout → 504 UPSTREAM_TIMEOUT
       └── 其它 → 走 writeUpstreamError 映射(400/404 + 子码 / 5xx 等)
```

`LastSeen` 字段在响应中保留(运维排查用),但**不再驱动离线判定**;
`/v1/sources` 视图的 `status` 字段恒为 `"online"`(信息性,不代表实际可达)。
真实可用性由下一次 `/v1/source/{source}/load` 的真实 dapr 调用失败判定。

### 为什么不做主动探活(历史决策)

曾经考虑过 gateway 周期 30s 调 `/healthz` 主动刷 LastSeen,后来撤了。三条理由:

1. **`middleware.http.bearer` 会拦 target sidecar 的 service invocation** —
   该中间件绑在 sidecar 的 app channel pipeline 上;gateway 调 `dapr.InvokeMethod(target, "/healthz")`
   时,请求经 gateway-side 出站 → target-side 进 app channel → target-side 的 bearer 中间件拒,
   返 401。`userd` 的 `dapr/components/bearer-auth.yaml` 当前没有 `scopes:` 字段,
   等于 default namespace 全 app 生效,cube-* sidecar 也被卡。
2. **直连 cube app 的 `/healthz`** 看似能绕开 middleware,但 `DAPR_APP_CHANNEL_ADDRESS`
   是 pod IP,k8s 下 Pod 飘移即失效;NetworkPolicy / Service Mesh 还得再开洞,不划算。
3. **placement 服务** 不带 Actor 时 dapr 会禁用部分功能,API 行为不稳。

被动验证是这三条坑里唯一不依赖外部机制的方案;真实 SOURCE_OFFLINE 由 dapr 真实失败触发,
`ErrConnFailure` 响应很快(TLS 握手 / placement lookup 失败毫秒级),不会真卡超时窗口。

| Building block | 用途 |
|---|---|
| Service Invocation | gateway ↔ cube app / cube app ↔ cube app |
| State Store (`cube-statestore`) | 注册表元信息(per app_id) |
| PubSub (`cube-pubsub`) | reload 通知 |
| Secrets (`cube-secrets`) | 数据源 DSN |
| Distributed Lock | (P2)预聚合并发构建互斥 |
