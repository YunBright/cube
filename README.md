# cube

基于 Dapr 的多实例语义层网关。每个数据源类型 → 一个 Dapr app 家族,每个版本 → 家族内一个独立 app 实例。对外暴露 Cube.js 兼容 API(`/v1/load` + `/v1/meta`),内部用 DuckDB 做预聚合 + L1/L2 缓存。

## 仓库结构

```
pkg/                       公共库
gateway/                   cube-gateway(固定 dapr app id)
compiler/                  cube-compiler(固定 dapr app id)
sixun-models/              思迅家族共享 models
semantic-layers/sixun/     思迅家族实例 cmd
skills/                    AI skills(viber coding 用)
deploy/                    Dapr 组件 / docker-compose / k8s
docs/                      架构与协议文档
```

## 快速开始(本地 dev)

```bash
# 1. 启动 Dapr standalone
dapr init

# 2. 启动注册中心 + 编译守护
cd gateway && dapr run --app-id cube-gateway -- go run ./cmd/gateway
cd compiler && dapr run --app-id cube-compiler -- go run ./cmd/compiler

# 3. 启动思迅实例(会自动注册到 cube-gateway)
cd semantic-layers/sixun && dapr run --app-id sixun-hbposv7 -- go run ./cmd/sixun-hbposv7
dapr run --app-id sixun-ysx -- go run ./cmd/sixun-ysx

# 4. BI 工具连 cube-gateway:http://localhost:3500/v1/load
```

## 文档

- [架构](docs/architecture.md)
- [Dapr app 协议](docs/dapr-app-contract.md)
- [语义层设计](docs/semantic-layer-design.md)
- [权限模型](docs/security.md)
- [缓存策略](docs/cache-strategy.md)

## AI 协作

任何 AI 助手必须先读 [AGENTS.md](AGENTS.md),按对应任务读 `skills/<name>/SKILL.md`。