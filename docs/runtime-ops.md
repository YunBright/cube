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
```

### 启动 cube app 实例(v2 — env 驱动)

> **核心变化**:cube app 的所有差异(数据源 DSN / 端口 / DuckDB / 注册地址)
> 都通过 **环境变量** 注入,同一份 binary 可以服务多个 instance(门店)。

#### 通用环境变量(`boot.Load()` 读取)

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `CUBE_APP_ID`     | ✓ | —   | **wire source id**(`<family>-<version>-<instance>`);family / version / instance 由其拆分得到 |
| `DAPR_APP_ID`     |   | (dapr 自动注入) | dapr sidecar app-id。**首选源** — dapr 启动时自动注入。/register body 用此上报给 gateway 当 dapr app-id |
| `CUBE_DAPR_APP_ID`|   | `"cube-" + CUBE_APP_ID` | 手动覆盖 dapr app-id。空值走推导默认值。**通常不需要设** |
| `CUBE_PORT`       |   | `:8080` | gin 监听端口 |
| `CUBE_MAPPING_DIR`|   | `./mapping` | mapping-*.yaml 所在目录 |
| `CUBE_MODELS_DIR` |   | (自动探测) | sixun-models 路径 |
| `CUBE_DUCKDB_PATH`|   | `./data/<app_id>.duckdb` | 本地 DuckDB 文件 |
| `CUBE_GATEWAY_URL`|   | `http://localhost:8080` | cube-gateway HTTP base |

DaprAppID 解析优先级(首个非空胜出):`DAPR_APP_ID` → `CUBE_DAPR_APP_ID` → 推导默认。
详见 `docs/dapr-app-contract.md` §2.1。

#### 启动一个思迅云商x 实例(`sixun-ysx-00`)

```bash
cd semantic-layers/sixun

CUBE_APP_ID=sixun-ysx-00 CUBE_PORT=:8083 \
  CUBE_GATEWAY_URL=http://localhost:8080 \
  dapr run --app-id cube-sixun-ysx-00 -- \
    go run ./cmd/sixun-ysx
```

DSN / 表名 仍然从 `./cmd/sixun-ysx/config.yaml` 的 `source.dsn` / `source.table_*` 读
(这部分每家供应商的实例可能有差异,**保持文件驱动**)。

#### 启动同一 binary 的另一实例(`sixun-ysx-baiyuan1`)

```bash
CUBE_APP_ID=sixun-ysx-baiyuan1 CUBE_PORT=:8084 \
  CUBE_GATEWAY_URL=http://localhost:8080 \
  dapr run --app-id cube-sixun-ysx-baiyuan1 -- \
    go run ./cmd/sixun-ysx
```

`family`/`version` 自动从 `CUBE_APP_ID` 拆分得 `"sixun"` / `"ysx"`(`instance` = `"baiyuan1"`),
无需另设 `CUBE_FAMILY` / `CUBE_VERSION`。

#### 启动思迅7pro 实例

```bash
CUBE_APP_ID=sixun-hbposv7-jiale CUBE_PORT=:8085 \
  CUBE_GATEWAY_URL=http://localhost:8080 \
  dapr run --app-id cube-sixun-hbposv7-jiale -- \
    go run ./cmd/sixun-hbposv7
```

### 关于 wire id 与 dapr app-id

启动时 `CUBE_APP_ID` 是 **wire source id**(URL 用),`dapr run --app-id` 是
**dapr 寻址用的 app-id**(默认带 `cube-` 前缀)。两者不必相同 — cube app 启动时
会读 dapr 注入的 `DAPR_APP_ID` 环境变量,然后在 /register body 里把
`dapr_app_id` 上报给 gateway。绝大多数场景 cube app **不需要任何额外配置**,
仅 `CUBE_APP_ID` + `dapr run --app-id cube-...` 两行即可,具体见
`docs/dapr-app-contract.md` §2.1。

### 验证注册

```bash
curl -s http://localhost:8080/v1/sources | jq
# → { "sources": [
#     { "source":"sixun-ysx-00",  "dapr_app_id":"cube-sixun-ysx-00",  "status":"online", ... },
#     { "source":"sixun-ysx-baiyuan1",  "dapr_app_id":"cube-sixun-ysx-baiyuan1",  "status":"online", ... },
#     { "source":"sixun-hbposv7-jiale", "dapr_app_id":"cube-sixun-hbposv7-jiale", "status":"online", ... }
#   ] }
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
CUBE_APP_ID=sixun-ysx-00 go run ./cmd/sixun-ysx
# 应该看到 "SQL Server connected" + "loaded model=supplier rows=N"
```

**macOS**:
```bash
brew install duckdb
# libduckdb 自动装到 /opt/homebrew/lib/libduckdb.dylib
CUBE_APP_ID=sixun-ysx-00 go run ./cmd/sixun-ysx
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
docker build \
  --build-arg INSTANCE=sixun-hbposv7 \
  -t cube-sixun-hbposv7 .
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
│  cube-gateway Deployment            │
│  + dapr sidecar(inject annotation)  │
└─────────────────────────────────────┘

┌─────────────────────────────────────┐
│  cube-compiler Deployment           │
│  + RBAC 调 k8s API 重启 Pod         │
└─────────────────────────────────────┘

┌─────────────────────────────────────────────┐
│  cube-app Deployment(per-instance)          │
│  每 instance 一个 Deployment / StatefulSet   │
│  CUBE_APP_ID / DSN 走 env / Secret         │
│  + 独立 PVC(挂载 ./data/<app_id>.duckdb)    │
└─────────────────────────────────────────────┘
```

**MVP 不实现 k8s RBAC / Pod 重启**,留给 P2。

## 健康检查

- gateway `GET /healthz` → 200 `{"status":"ok"}`
- cube app `GET /healthz` → 200 `{status, source, family, version, models, uptime}`
- dapr sidecar 自动做 liveness / readiness probe

## 日志

所有 app 用 `pkg/log`(JSON 格式),字段:
- `app_id`:实例 ID
- `component`:子模块(handler / registry / ...)
- `principal`:审计用(细粒度权限相关)
- `request_id`:端到端追踪 ID(由 gateway middleware 生成,经 x-request-id 传递到 cube app)

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
| cube app 挂 | gateway 收到 /v1/source/{source}/load 时,InvokeMethod 抛 ConnFailure → SOURCE_OFFLINE;BI 工具看到 503 + code=SOURCE_OFFLINE |
| cube app 网络 OK 但 cpu 卡死 | ctx 10s 超时 → UPSTREAM_TIMEOUT(504) |
| cube app 重启(进程消失) | gateway registry 不删;新 invoke 自动尝试新的 dapr sidecar 实例;注册再次成功后 LastSeen 刷新 |
| cube-gateway 挂 | BI 工具全失败;启动新实例(注册表由 dapr state store 持久化) |
| cube-compiler 挂 | cube app 正常运行,只是不能 reload 新代码;重启 compiler 即可 |
| dapr sidecar 挂 | dapr 自动重启(根据 liveness probe) |
