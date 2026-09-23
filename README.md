# cube

基于 Dapr 的多实例语义层网关。每个数据源类型 → 一个 Dapr app 家族,
每个 (family, version) → 一个 binary,
每个 store/instance(门店 / 租户)→ 一个由 `CUBE_APP_ID` 驱动的 dapr cube app 进程。
对外暴露 Cube.js 兼容 API(`/v1/source/{source}/load` + `/v1/sources`),
内部用 DuckDB 做预聚合 + L1/L2 缓存。所有非 2xx 响应返回统一错误信封。

## 仓库结构

```
pkg/                       公共库(含 pkg/apierror 错误信封 + pkg/daprclient 类型化调用)
gateway/                   cube-gateway(固定 dapr app id)
compiler/                  cube-compiler(固定 dapr app id)
sixun-models/              思迅家族共享 models
semantic-layers/sixun/     思迅家族实例 cmd(每 binary 由 env 驱动多 instance)
skills/                    AI skills(viber coding 用)
deploy/                    Dapr 组件 / docker-compose / k8s
docs/                      架构与协议文档
```

## 快速开始(本地 dev)

```bash
# 1. 启动 Dapr standalone
dapr init

# 2. 启动注册中心 + 编译守护
cd gateway   && dapr run --app-id cube-gateway   -- go run ./cmd/gateway
cd compiler  && dapr run --app-id cube-compiler  -- go run ./cmd/compiler

# 3. 启动思迅实例(env 驱动 instance 配置)
cd semantic-layers/sixun

CUBE_APP_ID=sixun-ysx-00 CUBE_PORT=:8083 CUBE_GATEWAY_URL=http://localhost:8080 \
  dapr run --app-id sixun-ysx-00 -- go run ./cmd/sixun-ysx

CUBE_APP_ID=sixun-hbposv7-jiale CUBE_PORT=:8085 CUBE_GATEWAY_URL=http://localhost:8080 \
  dapr run --app-id sixun-hbposv7-jiale -- go run ./cmd/sixun-hbposv7

# 4. BI 工具连 cube-gateway
#    POST http://localhost:8080/v1/source/sixun-ysx-00/load
#    GET  http://localhost:8080/v1/sources
```

## 文档

- [架构](docs/architecture.md)
- [Dapr app 协议](docs/dapr-app-contract.md)(v2:`/v1/source/{source}/load` + 错误信封)
- [运行时运维](docs/runtime-ops.md)(env 驱动 + 多 instance 启动)
- [语义层设计](docs/semantic-layer-design.md)
- [权限模型](docs/security.md)
- [缓存策略](docs/cache-strategy.md)

## AI 协作

任何 AI 助手必须先读 [AGENTS.md](AGENTS.md),按对应任务读 `skills/<name>/SKILL.md`。
