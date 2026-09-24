# AGENTS.md — 项目 AI 协作约定

> 本文件给所有 AI 助手(viber coding / Cursor / Aider / MiniMax Code 等)阅读,约束生成代码的风格与边界。
> 任何 AI 在动手前必须读完本文件 + `docs/architecture.md`。

## 1. 项目本质

基于 Dapr 的**多实例语义层网关**。每个数据源类型(思迅/粮油/…)→ 一个 Dapr app 家族,
每个版本(云商x / 7pro)→ 家族内一个 binary,
每个 store/instance(门店 / 租户)→ 一个 dapr cube app 进程,由 `CUBE_APP_ID` 区分
(`sixun-ysx-00` / `sixun-ysx-baiyuan1` / `sixun-hbposv7-jiale`)。
对外暴露 Cube.js 兼容 API(`/v1/source/{source}/load` + `/v1/sources`),内部用 DuckDB 做预聚合 + L1/L2 缓存。
所有非 2xx 响应必须返回统一错误信封(`pkg/apierror`)。

参考 cube-core 的 **schema / measure / dimension / pre-aggregation** 设计,**不照搬 driver 抽象**——driver 由 Dapr app 隔离。

## 2. 仓库结构(Go workspace 多 module)

```
cube/
├── pkg/                       # 公共库(github.com/YunBright/cube/pkg)
├── gateway/                   # cube-gateway(固定 dapr app id)
├── compiler/                  # cube-compiler(固定 dapr app id)
├── sixun-models/              # 思迅家族共享 models(独立 module)
└── semantic-layers/sixun/     # 思迅家族实例 cmd(require sixun-models)
```

新增数据源家族 = 新建 `semantic-layers/<family>/` + 抽出 `<family>-models/`。

## 3. 命名约定

| 维度 | 命名 | 示例 |
|---|---|---|
| 家族 | `[数据源名英文]` | `sixun` / `liangyou` |
| Binary(per family × version) | `[family]-[version]` | `sixun-hbposv7` / `sixun-ysx` |
| Wire source id(URL 用) | `[family]-[version]-[store/instance-alias]` | `sixun-hbposv7-jiale` / `sixun-ysx-00` |
| Dapr app id(寻址用,plan B 解耦) | `cube-[family]-[version]-[store/instance-alias]` | `cube-sixun-hbposv7-jiale` / `cube-sixun-ysx-00` |
| Model | `[业务实体英文单数]` | `supplier` / `product` / `order` |
| Go module | `github.com/YunBright/cube/<子目录>` | `cube/pkg` |

> instance id 在 gateway URL 端用 `^[\w-]+-[\w-]+-[\w-]+$` 校验(3 段)。
> 在 boot 端(dapr app-id 字符集限制)进一步收紧为 `^[a-z0-9][a-z0-9-]*[a-z0-9]$`(不允许下划线 / 首位 hyphen),
> 同时 `len(segments) ≥ 3`。
> family / version **由 CUBE_APP_ID 拆分得到**,不设独立 env。

## 4. 拍板决策(P0 + P1,不要重新发明)

| 决策 | 选择 | 不要做的 |
|---|---|---|
| 多版本共享 schema | 抽 `<family>-models` module,差异在 mapping.yaml | ❌ 复制代码、❌ 写 Go 硬编码映射 |
| DuckDB 存储 | 每个 dapr cube app instance 独立 .duckdb 文件(命名 `<app_id>.duckdb`) | ❌ 共享一个 DB |
| Instance 配置 | env 驱动(`CUBE_APP_ID` 是 wire id;`DAPR_APP_ID` 由 dapr 自动注入,cube app 报给 gateway 当 dapr app-id 寻址用),同 binary 多 instance | ❌ 硬编码 const、❌ 编译时区分 instance |
| 部署模式 | hosted (k8s) + 本地 dev (docker-compose) | ❌ 其他模式 |
| mapping.yaml 语义 | **仅字段名 + 类型 + 单位**(无 enum_map / 无 transform) | ❌ 写 enum_map、❌ 写 transform |
| 权限分层 | gateway 粗粒度(source 访问)+ cube app 细粒度(行/列) | ❌ gateway 实现全部权限 |
| L1 缓存命中 | 直接返回,**完全跳过 cube app** | ❌ 还调 cube app |
| Query 寻址 | `POST /v1/source/{source}/load` → gateway 按注册时上报的 `dapr_app_id` 调 dapr(plan B 解耦) | ❌ model → app_id 路由 |
| MVP API 范围 | `/v1/source/{source}/load` + `/v1/sources` + `/register` + `/unregister` + `/healthz` | ❌ `/v1/sql`、❌ `/v1/load`、❌ `/v1/meta` |
| Cube app 优雅关闭 | cube app 收到 `SIGTERM` / `SIGINT` → 调 `POST cube-gateway/unregister`(body `{app_id: "..."}`,2s deadline,失败仅记日志)→ `os.Exit(0)`;gateway 立即从 registry + state store 删除条目,**幂等**(未注册也返 204) | ❌ 等下次 invoke 失败再发现(BI 视图长时间脏数据),❌ 阻塞退出等 unregister(会被 kubelet 30s SIGKILL 兜底) |
| 错误信封 | 所有非 2xx 用 `pkg/apierror` 统一形状 `{code, message, details}`,`X-Request-Id` header | ❌ `gin.H{"error":...}`、❌ 各端点自定义 |
| HTTP 框架 | **`gin-gonic/gin v1.10.x`**(所有 dapr app 入口端点统一用) | ❌ 直接 `net/http`、`❌ chi/echo/fiber` 等其它 web 框架 |
| Source 在线判定 | **被动验证**:handler 不前置 IsOnline 检查,任何已注册 source 都直接 `dapr.InvokeMethod`,真实 `SOURCE_OFFLINE` 由 dapr 真实调用失败(`ErrConnFailure`)触发;`/v1/sources` 视图 status 字段恒为 "online"(信息性,不代表实际可达),LastSeen 仅作运维排查信号 | ❌ 时间窗口式离线判定(冷启动源 90s 后假 offline),❌ 主动探活(复杂、对 middleware.http.bearer 链路脆弱),❌ 直连 app / kube 健康度(直连 `DAPR_APP_CHANNEL_ADDRESS`,k8s pod IP 飘移即失效) |
| 枚举值归一化 | **不做**,留在 schema.yaml meta + BI 层翻译 | ❌ 在 mapping.yaml 写 enum_map |

## 5. 编码约束

- **每个 module 必须 `go.mod`**,根仓库只放 `go.work`
- 业务代码**只用 Go 1.22 标准库 + 项目内 pkg + gin**;新引外部依赖前先看 `pkg/go.mod` 是否已有
- **所有 HTTP 端点统一用 `gin-gonic/gin`**,handler 签名为 `func(*gin.Context)`;构造 engine 用 `gin.New() + gin.Logger() + gin.Recovery()`,**不要用 `gin.Default()`**(意图不显式)
- `net/http` 在业务代码里**仅保留**:gin Engine 满足 `http.Handler` 接口(用于 `httptest.NewServer`)、自定义 transport、超时控制等 gin 不擅长的底层场景
- Dapr SDK 统一从 `pkg/daprclient` 引,不要各 app 直接 `import "github.com/dapr/go-sdk/client"`
- Dapr building blocks 使用范围:Service Invocation / State Store / PubSub / Secrets / Distributed Lock;**不要引入 Actors / Workflow**(P2 再说)
- 所有错误往上抛,不要吞;日志用 `pkg/log`
- 任何 HTTP 错误响应**必须**走 `pkg/apierror`(`WriteError` / `WriteErrorWithSource`),
  禁止直接 `c.JSON(4xx/5xx, gin.H{"error": ...})`
- 不要写 SQL 注入风险入口(P1-8 选了不做 /v1/sql,不要自行加回来)

## 6. AI Skills

`skills/<skill-name>/SKILL.md` 是项目级 AI 教学材料,任何 AI 在做对应任务前**必须先读**对应 skill:

| Skill | 何时读 |
|---|---|
| `add-new-data-source` | 新建家族(如加"粮油") |
| `add-new-version` | 现有家族加新版本 |
| `add-new-model` | 加新 model(如 customer) |
| `write-field-mapping` | 写 mapping.yaml |
| `design-schema` | 写 schema.yaml |
| `write-preaggregation` | 写 DuckDB 预聚合 |
| `debug-query` | 排查 cube query 慢/错 |

## 7. 完成前自检

- [ ] `go build ./...` 在仓库根能跑通
- [ ] 新增代码有对应 pkg 接口的最小测试(脚手架阶段允许 TODO)
- [ ] 没碰 P1 拍板里 ❌ 的事项
- [ ] 新增 family 必须同时加 `skills/add-new-data-source` 的代码示例(若案例缺失)

## 8. 添加子服务(dapr app)

- 是否属于已存在family，是则新建<family>-models 和 semantic-layers\<family>，否则在所属family下追加，参考已存在的semantic-layers.
- 修改.goreleaser.yaml,增加构建配置，参考已存在的semantic-layers.
- 修改..\deployer\deploy-cube.ps1，并提醒用户在服务器上新增system unit.