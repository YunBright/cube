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
  "order": [
    { "id": "supplier.count", "order": "desc" }
  ],
  "limit": 100
}
```

**`order` 语义**(2026-10-09 起支持,`pkg/cubequery.buildOrder`):

| 规则 | 说明 |
|---|---|
| `id` 格式 | `<model>.<ref>`,可指向 **dimension 或 measure** |
| `order` 取值 | `asc` / `desc`,大小写不敏感;**省略按 `asc`** |
| 其它取值 | **400 `QUERY_INVALID`**(方向是用户可控文本,不在白名单就拒) |
| 必须同时被 select | 排的成员必须出现在 `dimensions` / `measures` 里,否则 **400** |
| member 不在 schema | **400 `QUERY_INVALID`**,`details.order_member` |

排序按 **SELECT 别名**实现(`"settlement.settled_at"` / `[settlement.settled_at]`),
DuckDB 与 T-SQL 两条路径共用同一份编译逻辑,语义逐字一致。

> ⚠️ 不给 `order` 时 `limit` 是**任意截取**(ysx 4348 张结算单、hbposv7 799 张,
> TOP/LIMIT 按存储顺序取前 N 行)。列表类场景必须显式给 `order`,
> 否则返回哪几行不确定。

**`offset` / `total`**

| 字段 | 语义 |
|---|---|
| `offset` | 跳过前 N 行。数字或单元素数组均可;负数/字符串/浮点 → **400** |
| `offset > 0` 且无 `order` | **400** ——分页必须建立在稳定排序上(T-SQL 的 OFFSET/FETCH 也强制要求 ORDER BY) |
| `total: true` | 返回不受 limit/offset 影响的总行数;用 `COUNT(*) OVER()` 一次算出,不额外查第二遍 |

**`timeDimensions`**(按日/周/月/季/年聚合 + 时间窗过滤)

```json
{
  "measures": ["sale_day.net_sale_quantity"],
  "timeDimensions": [
    { "dimension": "sale_day.business_date", "dateRange": "last_90_days", "granularity": "month" }
  ],
  "order": [{ "id": "sale_day.business_date.month", "order": "desc" }],
  "limit": 12,
  "total": true
}
```

- `granularity` 输出 key 是 `<model>.<dim>.<gran>`,可直接用于 `order`
- `dateRange` 支持 `["2026-07-01","2026-09-30"]`、`"2026-08-15"`、以及
  `today / yesterday / last_7_days / last_30_days / last_90_days / this_month / last_month / this_year`
- **看不懂的 dateRange 一律 400**,绝不静默忽略(静默忽略 = 查了全历史却以为是近期)
- schema 里 `char_date: true` 的维度走原列字符串比较(可走索引),不套 `CAST`

**日期类 filter operator**:`inDateRange` / `notInDateRange` / `onTheDate` /
`beforeDate` / `afterDate`(排他)。值必须是 `YYYY-MM-DD`,否则 400。

**`segments`**:schema 里预定义的过滤片段,调用方只写名字。
名字不存在时 400,并在错误里列出可用名字。

**`joins`(多表关联)**

查询里可以引用**别的 model** 的成员,编译器按当前 model 的 `joins:` 声明连表:

```json
{
  "measures": ["settlement_line.amount_yuan"],
  "dimensions": ["settlement.document_type", "purchase_sheet.document_type"]
}
```

| 规则 | 行为 |
|---|---|
| 关联来源 | **只走 schema 显式声明的 joins**,不做 JIT 自动关联 |
| JOIN 类型 | `many_to_one` 用 `LEFT`(查档案,无档案的行不该凭空消失) |
| 支持范围 | 目前只支持 `storage: live` model 之间的 join |
| `one_to_many` 扇出 | 被放大的那一侧的度量 → **400**,错误里指出该用多的一侧 |

最后一条是**安全闸**:join 进 N 行后对"一"那一侧求 SUM 会得到 N 倍金额,
SQL 合法、查询 200 OK、金额凭空翻倍。宁可 400 也不要这种错数字。

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
| `MODEL_NOT_FOUND_IN_SOURCE` | 404 | cube app 返回 `{"code":"MODEL_NOT_FOUND",…}`;**只用于「该 source 不暴露这个 model」** |
| `QUERY_PARSE_ERROR`         | 400 | body 读不出来 / JSON 无效 |
| `QUERY_INVALID`             | 400 | JSON OK 但语义非法:model 字段空 / measures 为空 / 引用了不存在的 dimension 或 measure |
| `NO_PRINCIPAL`              | 401 | `cfg.AuthRequired && Authorization` header 缺失 |
| `FORBIDDEN`                 | 403 | `Authorizer.Allow` 返回 false |
| `RATE_LIMITED`              | 429 | (P2,当前未 wire) |
| `INTERNAL_ERROR`            | 500 | gateway **自身** panic(注意:cube app 的 5xx 归 `UPSTREAM_ERROR` 502,不扫 5xx 子码) |
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
| `INTERNAL_ERROR`       | 500 | `UPSTREAM_ERROR`            | 502 |
| (其他 / 缺省)            | *   | `UPSTREAM_ERROR`            | 502 |

> 上游 5xx 一律归 `UPSTREAM_ERROR` 502(语义就是"我这边好的,是上游 cube app 挂了"),
> 与 4xx 分支"扫 body 子码"的做法不同 —— 5xx 背后的原因对调用方没有可操作性,
> 再细分只会让人以为该重试某个具体环节。

#### `MODEL_NOT_FOUND` 的适用范围(2026-10-10 收窄)

**`MODEL_NOT_FOUND` 只表示一件事:这个 source 不暴露这个 model。**
它**不**覆盖"model 存在、但查询引用了不存在的 dimension / measure"。

2026-10-10 生产现场抓到:查询里把 `settlement.sheet_no` 写错(真实字段是 `id`),
cube app 当时把 `BuildError{Kind:"dimension"}` 兜底成了 404 + `MODEL_NOT_FOUND`,
gateway 扫到子码后翻成 `model not exposed by source: settlement`。
于是排查被引向「这个门店是不是没注册 settlement / schema 有没有推上去」,
而真正要改的只是查询里的一个字段名 —— **契约问题被误报成了查询问题**,
两者的重试策略正好相反(前者重试无用,后者改了就好)。

收窄后的分层(cube app 的 `queryHandler` 必须逐 `BuildError.Kind` 分派,
不允许兜底成同一个码):

| 情形 | cube app | gateway | 含义 |
|---|---|---|---|
| 该 source 不暴露这个 model | 404 `MODEL_NOT_FOUND` | 404 `MODEL_NOT_FOUND_IN_SOURCE` | 契约/部署问题,运维介入 |
| dimension / measure 不存在 | 400 `QUERY_INVALID`(`details.ref` 回传字段名) | 400 `QUERY_INVALID` | 查询写错了,调用方改查询 |
| filter / order 成员不合法、`measures` 为空、model 无法推断 | 400 `QUERY_INVALID` | 400 `QUERY_INVALID` | 同上 |
| 非 `BuildError` 的编译失败 | 500 `INTERNAL_ERROR` | 502 `UPSTREAM_ERROR` | 我们的 bug,**不是任何业务结论** |

**兜底方向的原则**:编译阶段的未知失败绝不能降级成 `MODEL_NOT_FOUND` ——
那是把"我们的 bug"说成"这个 source 没有这个 model",排查方向从查询跑偏到部署。
回归锁在 `cmd/sixun-*/errcode_test.go`,其中 `UnknownModelIs404` 与
`UnknownDimensionIs400` / `UnknownMeasureIs400` 必须成对存在。

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
