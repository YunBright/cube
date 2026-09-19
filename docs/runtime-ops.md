# 运行时运维(P0-4)

## 部署模式

### 本地 dev(当前 MVP)

```bash
# 1. dapr standalone
dapr init

# 2. 启动 redis(state store + pubsub)
docker run -d -p 6379:6379 redis:7-alpine

# 3. 加载 dapr 组件
dapr run --components-path ./deploy/dapr/components -- \
  go run ./gateway/cmd/gateway

# 4. 启动 gateway
dapr run --app-id cube-gateway -- \
  go run ./gateway/cmd/gateway

# 5. 启动 compiler
dapr run --app-id cube-compiler -- \
  go run ./compiler/cmd/compiler

# 6. 启动思迅实例(自动注册到 gateway)
dapr run --app-id sixun-hbposv7 -- \
  go run ./semantic-layers/sixun/cmd/sixun-hbposv7

dapr run --app-id sixun-ysx -- \
  go run ./semantic-layers/sixun/cmd/sixun-ysx
```

### DuckDB 运行时依赖(P0-3 + go-pduckdb)

`pkg/duckdb` 用 **`github.com/fpt/go-pduckdb`**(纯 Go DuckDB driver),**不需 gcc / MinGW**。
但**运行时**机器上要有 `libduckdb` 共享库(.so / .dylib / .dll)。

#### 安装(本地 dev)

**Windows**:
```powershell
# 1. 下载 DuckDB Windows release(选 v1.5.4 或更新)
curl -L -o duckdb.zip `
  "https://github.com/duckdb/duckdb/releases/download/v1.5.4/duckdb_cli-windows-amd64.zip"
Expand-Archive duckdb.zip -DestinationPath C:\tools\duckdb

# 2. 库路径加到环境变量
$env:DUCKDB_LIBRARY_PATH = "C:\tools\duckdb\duckdb.dll"
$env:PATH += ";C:\tools\duckdb"

# 3. 验证
go run ./cmd/sixun-hbposv7
# 应该看到 "SQL Server connected" + "loaded model=supplier rows=N"
```

**macOS**:
```bash
brew install duckdb
# libduckdb 自动装到 /opt/homebrew/lib/libduckdb.dylib
go run ./cmd/sixun-hbposv7
```

**Linux (Ubuntu/Debian)**:
```bash
curl -sSL \
  "https://github.com/duckdb/duckdb/releases/download/v1.5.4/libduckdb-linux-amd64.zip" \
  -o /tmp/libduckdb.zip
sudo unzip -j /tmp/libduckdb.zip libduckdb.so -d /usr/local/lib/
sudo ldconfig
```

#### 库路径查找优先级(go-pduckdb)

1. 环境变量 `DUCKDB_LIBRARY_PATH`(绝对路径)
2. macOS: `DYLD_LIBRARY_PATH` 目录
3. Linux: `LD_LIBRARY_PATH` 目录
4. 系统标准库目录

#### Docker(k8s)

`semantic-layers/sixun/Dockerfile.template` 多阶段 build:
- Stage 1: `golang:1.25-alpine` + `CGO_ENABLED=0` 编译(纯 Go,无需 gcc)
- Stage 2: `alpine:3.20` + 下载 `libduckdb-linux-amd64.zip` 放到 `/usr/local/lib/`

```bash
docker build --build-arg INSTANCE=sixun-hbposv7 -t cube-sixun-hbposv7 .
```

#### 测试集成

```bash
# 装好 libduckdb 后:
cd pkg && go test ./duckdb/...
# 期望看到 3 个 PASS(原来是 SKIP)
```

### Hosted(k8s,P2)

```
┌─────────────────────────────────────┐
│  cube-gateway Deployment           │
│  + dapr sidecar(inject annotation) │
└─────────────────────────────────────┘

┌─────────────────────────────────────┐
│  cube-compiler Deployment          │
│  + RBAC 调 k8s API 重启 Pod        │
└─────────────────────────────────────┘

┌─────────────────────────────────────┐
│  sixun-hbposv7 Deployment          │
│  + 独立 PVC(挂载 .duckdb)         │
└─────────────────────────────────────┘
```

**MVP 不实现 k8s RBAC / Pod 重启**,留给 P2。

## 健康检查

- gateway `GET /health` → 200 OK
- cube app `GET /health` → 200 OK
- dapr sidecar 自动做 liveness / readiness probe

## 日志

所有 app 用 `pkg/log`(JSON 格式),字段:
- `app_id`:实例 ID
- `component`:子模块(handler / registry / ...)
- `principal`:审计用(细粒度权限相关)

输出 stdout,dapr sidecar 转发到日志系统(ELK / Loki)。

## 监控(MVP 不实现)

P2 候选:
- Prometheus exporter 在 `/metrics`
- cube query P50 / P95 latency
- L1 hit rate
- cube app 注册数 / 健康率

## 故障恢复

| 故障 | 行为 |
|---|---|
| cube app 挂 | gateway 收到 /v1/load 时,该 model 路由失败 → 502;BI 工具显示错误 |
| cube-gateway 挂 | BI 工具全失败;启动新实例(注册表由 dapr state store 持久化) |
| cube-compiler 挂 | cube app 正常运行,只是不能 reload 新代码;重启 compiler 即可 |
| dapr sidecar 挂 | dapr 自动重启(根据 liveness probe) |