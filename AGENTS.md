# AGENTS.md — 项目 AI 协作约定

> 本文件给所有 AI 助手(viber coding / Cursor / Aider / MiniMax Code 等)阅读,约束生成代码的风格与边界。
> 任何 AI 在动手前必须读完本文件 + `docs/architecture.md`。

## 1. 项目本质

基于 Dapr 的**多实例语义层网关**。每个数据源类型(思迅/粮油/…)→ 一个 Dapr app 家族,每个版本(云商x / 7pro)→ 家族内一个独立 app 实例(`sixun-ysx` / `sixun-hbposv7`)。对外暴露 Cube.js 兼容 API(`/v1/load` + `/v1/meta`),内部用 DuckDB 做预聚合 + L1/L2 缓存。

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
| 实例(dapr app id) | `[家族]-[版本短码]` | `sixun-hbposv7` / `sixun-ysx` |
| Model | `[业务实体英文单数]` | `supplier` / `product` / `order` |
| Go module | `github.com/YunBright/cube/<子目录>` | `cube/pkg` |

## 4. 拍板决策(P0 + P1,不要重新发明)

| 决策 | 选择 | 不要做的 |
|---|---|---|
| 多版本共享 schema | 抽 `<family>-models` module,差异在 mapping.yaml | ❌ 复制代码、❌ 写 Go 硬编码映射 |
| DuckDB 存储 | 每个 dapr cube app 独立 .duckdb 文件 | ❌ 共享一个 DB |
| 部署模式 | hosted (k8s) + 本地 dev (docker-compose) | ❌ 其他模式 |
| mapping.yaml 语义 | **仅字段名 + 类型 + 单位**(无 enum_map / 无 transform) | ❌ 写 enum_map、❌ 写 transform |
| 权限分层 | gateway 粗粒度(model 访问)+ cube app 细粒度(行/列) | ❌ gateway 实现全部权限 |
| L1 缓存命中 | 直接返回,**完全跳过 cube app** | ❌ 还调 cube app |
| MVP API 范围 | `/v1/load` + `/v1/meta` | ❌ `/v1/sql`、❌ 其它端点 |
| HTTP 框架 | **`gin-gonic/gin v1.10.x`**(所有 dapr app 入口端点统一用) | ❌ 直接 `net/http`、`❌ chi/echo/fiber` 等其它 web 框架 |
| 枚举值归一化 | **不做**,留在 schema.yaml meta + BI 层翻译 | ❌ 在 mapping.yaml 写 enum_map |

## 5. 编码约束

- **每个 module 必须 `go.mod`**,根仓库只放 `go.work`
- 业务代码**只用 Go 1.22 标准库 + 项目内 pkg + gin**;新引外部依赖前先看 `pkg/go.mod` 是否已有
- **所有 HTTP 端点统一用 `gin-gonic/gin`**,handler 签名为 `func(*gin.Context)`;构造 engine 用 `gin.New() + gin.Logger() + gin.Recovery()`,**不要用 `gin.Default()`**(意图不显式)
- `net/http` 在业务代码里**仅保留**:gin Engine 满足 `http.Handler` 接口(用于 `httptest.NewServer`)、自定义 transport、超时控制等 gin 不擅长的底层场景
- Dapr SDK 统一从 `pkg/daprclient` 引,不要各 app 直接 `import "github.com/dapr/go-sdk/client"`
- Dapr building blocks 使用范围:Service Invocation / State Store / PubSub / Secrets / Distributed Lock;**不要引入 Actors / Workflow**(P2 再说)
- 所有错误往上抛,不要吞;日志用 `pkg/log`
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