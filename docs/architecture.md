# 架构

## 三类 dapr app

```
                BI 工具 / Tableau / Metabase / Superset
                         │
                         ▼ HTTP
            ┌──────────────────────────────────┐
            │   cube-gateway  (固定 dapr app id) │
            │                                  │
            │  • POST /register                │
            │  • POST /v1/source/{source}/load │ ← source 直接寻址
            │  • GET  /v1/sources              │
            │  • L1 缓存(per-source 隔离)      │
            │  • 粗粒度权限(source 访问)       │
            │  • 注册表(state store,per-app_id)│
            │  • 统一错误信封(13 个机器可读码) │
            └──────────────┬───────────────────┘
                           │ dapr invocation
        ┌──────────────────┼──────────────────────────┐
        ▼                  ▼                          ▼
 ┌──────────────┐   ┌──────────────┐           ┌──────────────┐
 │ sixun-ysx-00 │   │ sixun-ysx-   │           │ sixun-       │
 │              │   │   baiyuan1   │           │   hbposv7-   │
 │              │   │              │           │   jiale      │
 │  同 binary   │   │  同 binary   │           │              │
 │  不同 app_id │   │  不同 app_id │           │  另一 binary │
 │  不同 DSN    │   │  不同 DSN    │           │  各自一份    │
 │              │   │              │           │              │
 │ • mapping    │   │ • mapping    │           │ • mapping    │
 │ • DuckDB     │   │ • DuckDB     │           │ • DuckDB     │
 │   (独立)     │   │   (独立)     │           │   (独立)     │
 │ • L2 缓存    │   │ • L2 缓存    │           │ • L2 缓存    │
 │ • 细粒度权限 │   │ • 细粒度权限 │           │ • 细粒度权限 │
 └──────┬───────┘   └──────┬───────┘           └──────┬───────┘
        ▼                  ▼                          ▼
   思迅云商x DB-A     思迅云商x DB-B           思迅7pro DB-jiale
                                          git push
                                             │
                                             ▼
            ┌──────────────────────────────────┐
            │  cube-compiler  (固定 dapr app id)│
            │                                  │
            │  • git pull(轮询)             │
            │  • go build(每个 cube app 实例) │
            │  • 重启 dapr cube app 进程     │
            │  • pubsub 通知 gateway 刷新注册 │
            └──────────────────────────────────┘
```

> **关键变化(v2)**:gateway **不再做 model → app_id 路由**。
> 每个 cube query 由 URL 上的 `{source}` 段直接寻址到对应 dapr app,
> **store 级隔离**(同一 family+version 多店时不再相互覆盖)。

## 关键决策

| 决策 | 拍板 | 详见 |
|---|---|---|
| gateway / compiler 固定 id | P0-1 | [dapr-app-contract.md](dapr-app-contract.md) |
| source = app_id,1:1 寻址 | v2 | [dapr-app-contract.md](dapr-app-contract.md) |
| 多版本共享 schema | P0-2 | [semantic-layer-design.md](semantic-layer-design.md) |
| 每 instance 独立 DuckDB | P0-3 | [semantic-layer-design.md](semantic-layer-design.md) |
| env 驱动 instance 配置 | v2 | [runtime-ops.md](runtime-ops.md) |
| 部署模式 | P0-4 | [runtime-ops.md](runtime-ops.md) |
| mapping.yaml 语义 | P1-5 | [semantic-layer-design.md](semantic-layer-design.md) |
| 权限分层 | P1-6 | [security.md](security.md) |
| L1 命中策略 | P1-7 | [cache-strategy.md](cache-strategy.md) |
| MVP API 范围 + 错误信封 | v2 | [dapr-app-contract.md](dapr-app-contract.md) |
