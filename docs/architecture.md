# 架构

## 三类 dapr app

```
                BI 工具 / Tableau / Metabase / Superset
                         │
                         ▼ HTTP
            ┌──────────────────────────────────┐
            │   cube-gateway  (固定 dapr app id) │
            │                                  │
            │  • POST /register   dapr cube app │
            │  • POST /v1/load    cube 兼容查询 │
            │  • GET  /v1/meta    cube 兼容元信息│
            │  • 路由(model → app_id)        │
            │  • L1 缓存(命中直接返回)       │
            │  • 粗粒度权限(model 访问)       │
            │  • 注册表(state store)          │
            └──────────────┬───────────────────┘
                           │ dapr invocation
        ┌──────────────────┼──────────────────────┐
        ▼                  ▼                      ▼
 ┌─────────────┐   ┌─────────────┐        ┌─────────────┐
 │ sixun-hbposv7│   │ sixun-ysx   │        │ (未来 liangyou)│
 │             │   │             │        │             │
 │ • mapping   │   │ • mapping   │        │ ...         │
 │ • DuckDB    │   │ • DuckDB    │        │             │
 │ • L2 缓存   │   │ • L2 缓存   │        │             │
 │ • 细粒度权限 │   │ • 细粒度权限 │        │             │
 └──────┬──────┘   └──────┬──────┘        └──────┬──────┘
        │                  │                       │
        ▼                  ▼                       ▼
   思迅 7pro DB        思迅云商x DB             粮油 DB
                                       git push
                                          │
                                          ▼
            ┌──────────────────────────────────┐
            │  cube-compiler  (固定 dapr app id)│
            │                                  │
            │  • git pull(轮询)             │
            │  • go build(每个 cube app)     │
            │  • 重启 dapr cube app 进程    │
            │  • pubsub 通知 gateway 刷新注册 │
            └──────────────────────────────────┘
```

## 关键决策

| 决策 | 拍板 | 详见 |
|---|---|---|
| gateway / compiler 固定 id | P0-1 | [dapr-app-contract.md](dapr-app-contract.md) |
| 多版本共享 schema | P0-2 | [semantic-layer-design.md](semantic-layer-design.md) |
| 每 app 独立 DuckDB | P0-3 | [semantic-layer-design.md](semantic-layer-design.md) |
| 部署模式 | P0-4 | [runtime-ops.md](runtime-ops.md) |
| mapping.yaml 语义 | P1-5 | [semantic-layer-design.md](semantic-layer-design.md) |
| 权限分层 | P1-6 | [security.md](security.md) |
| L1 命中策略 | P1-7 | [cache-strategy.md](cache-strategy.md) |
| MVP API 范围 | P1-8 | [dapr-app-contract.md](dapr-app-contract.md) |