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
| `CUBE_DUCKDB_PATH`|   | `./data/<app_id>.duckdb` | 本地 DuckDB 文件 |
| `CUBE_GATEWAY_URL`|   | `http://localhost:8080` | cube-gateway HTTP base |
| `CUBE_REFRESH_EVERY`|  | `30m` | duck 模型定时重拉间隔;`0` 关闭 |

> **没有** `CUBE_MODELS_DIR` / `CUBE_MAPPING_DIR` —— schema 与 mapping 都用
> `go:embed` 编译进二进制了(2026-10-09 改动)。曾经有,已删,理由见下方
> 「模型已编译进二进制」。

### 配置从哪读:`config.local.yaml` > `config.yaml`

| 文件 | 进版本库 | 作用 |
|---|---|---|
| `config.yaml` | ❌(`.gitignore` 第 26 行) | 实例配置本体,**也是打包进归档的那份** |
| `config.local.yaml` | ❌ | 本地验证用的覆盖文件,存在即生效 |
| `config.example.yaml` | ✅ | 模板(新环境从这里拷) |

app / `sqlcheck` / `sqlq` 都按这个优先级读,**不读环境变量**。规则实现在
`semantic-layers/sixun/internal/appcfg`,有测试钉住。

本地连真实源库验证(比如把 DSN 指到 ssh 隧道端口、或指向测试库)时:

```bash
# 1. 拷一份本地覆盖(此文件不会进版本库)
cp cmd/sixun-hbposv7/config.example.yaml cmd/sixun-hbposv7/config.local.yaml
#    编辑它填 dsn 和 11 个 table_*

# 2. 跑验证,不带 -config,自动选中 config.local.yaml
go run ./cmd/sqlcheck -family hbposv7
go run ./cmd/sqlq -family hbposv7 -q "SELECT COUNT(*) FROM t_fm_recpay_gx_master"
go run ./cmd/sqlq -config cmd/sixun-hbposv7 -q "..."   # 也可以直接给目录
```

每次运行都会打印实际用的文件,别靠猜:

```
=== config: cmd\sixun-hbposv7\config.local.yaml ===
```

> ⚠️ **`config.local.yaml` 绝不能留在服务器上**。它优先级高于 `config.yaml`,
> 一旦存在就会盖住部署刚推下去的 `config.yaml`,症状是「部署改了配置但没生效」
> 且**零报错**。`deploy-cube.ps1 -Step prune` 每次部署都会删掉远端同名文件。
> app 启动日志的 `config loaded` 行也带 `file=`,一眼能看出用的是哪份。

### ⚠️ `config.yaml` 会被部署覆盖

归档里**含** `config.yaml`,所以每次部署都用**打包机本机工作区的那份**
覆盖远端。因为它不在版本库,「仓库里那份」并不存在 —— 换台机器打包,
部署的就是那台机器的配置。两个后果:

1. 远端任何对手工 `config.yaml` 的修改(例如轮换源库口令)会在下次部署被**静默还原**
2. 要改配置就改**工作区那份**,然后重新部署

详见 `.goreleaser.yaml` 头部与 `deploy-cube.ps1` 的 `.DESCRIPTION`。

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

cube app 的 `/healthz` 另有三段,排查刷新问题时先看它们:

```json
{
  "status": "ok",
  "data":    { "interval_seconds": 1800, "data_age_seconds": 52,
               "consecutive_failures": 0, "models": { "product": { ... } } },
  "refresh": { "mode": "dapr-job", "job_name": "cube-refresh",
               "triggered": 3, "skipped_overlap": 0 }
}
```

- `status` = `degraded` 时仍返 **200**(数据旧 ≠ 服务不可用),别用 5xx 判活
- `data_age_seconds` 超过 `2 × interval_seconds` 才判 stale,避免刚启动时误报
- `refresh.mode` 见下节

## 刷新驱动:dapr-job 优先,ticker 兜底

duck 模型的定时重拉由 `internal/refreshschedule` 统一管,两种模式:

| mode | 含义 | 触发源 |
|---|---|---|
| `dapr-job` | 已登记到 Dapr Scheduler | sidecar 到点 POST `/job/cube-refresh` |
| `ticker` | 登记失败,退回进程内定时器 | `time.Ticker` |
| `disabled` | `interval <= 0` | 无 |

**为什么必须有兜底**:Dapr Jobs 是 alpha 能力,可用性取决于集群里 Scheduler 与 sidecar 的连通性。
2026-10-10 实测:sidecar 被指向 `localhost:50006` 时 Jobs API 直接挂死(超时,不是 404),
表现是「登记没成功也没报错」。纯 job 的话结果就是**数据永远不刷新、healthz 全绿**。

**ticker 不是终点 —— 收到 job 触发会自动切回。** Scheduler 恢复后,之前留在 etcd 里的
job 会继续投递(触发时找不到可用 sidecar 的会进 staging queue,等 sidecar 可用后自动补投)。
app 一收到自己的 job,就同时证明了 Scheduler 活着、etcd 数据完好、sidecar 在线,
于是**立刻切回 `dapr-job` 并停掉 ticker**:

```
收到 dapr job 触发,判定 Scheduler 已恢复,切回 dapr-job 并停止 ticker
```

不切的话,ticker 与 job 会同时刷同一个 DuckDB(刷新频率翻倍),而且 `refresh.mode`
会显示 `ticker` 而 job 其实一直在触发 —— **那个字段会开始说谎**。

所以运维上:

- `refresh.mode: ticker` 且 `refresh.schedule_error` 有值 = Scheduler 不可用,查调度器
- 恢复后不必重启 cube app,等下一次 job 触发会自动切回(最迟一个间隔)
- `refresh.triggered` / `skipped_overlap` 是**进程内**计数,重启归零;
  `skipped_overlap` 持续增长说明刷新耗时已超过间隔(在途守卫在丢触发),
  该调大 `refresh.every` 或优化加载

## 改动 schema / mapping 后:生效与验证

> 完整流程见 [数据源勘察与 mapping 编写手册](data-source-mapping-playbook.md) §3~§6。

### 模型已编译进二进制(go:embed)

`schema.yaml` 与 `mapping.yaml` 都用 `go:embed` 编译进各自的二进制:

- schema: `sixun-models/<model>/schema.yaml`,经 `sixun-models/embed.go` 暴露 `models.FS()`
- mapping: `semantic-layers/sixun/cmd/<binary>/mapping/mapping-<model>.yaml`,每个 binary 一份

所以:

- 部署**只需要推二进制**(`config.yaml` 仍手工维护,含 DSN 口令)
- `deploy-cube.ps1` 的 tar 里**不再包含** `mapping/` 与 `sixun-models/`
- **改了 schema / mapping 必须重新构建** —— 只重推旧二进制不会有任何变化
- 读盘路径(`CUBE_MODELS_DIR`)已彻底删除。原因是它静默失败:路径对不上时
  一个模型都加载不到,而启动**不报错**,要到查询时才炸

资产完整性由测试兜底(`go test ./cmd/sixun-ysx/... ./cmd/sixun-hbposv7/...`):
schema 缺 mapping、mapping 为空、mapping 拼错 model 名,都会让测试失败。

验证二进制里确实带了新模型,看启动日志这一行即可:

```bash
ssh gyy "journalctl --user -u cube-sixun-ysx.service -n 200 | grep 'loading schemas'"
```

### 必须重启才会重拉

新列写进 DuckDB 的唯一路径是启动时的 `Fetch → mapping → LoadFrom`:

```bash
ssh gyy "systemctl --user restart cube-sixun-ysx.service && sleep 90 && systemctl --user is-active cube-sixun-ysx.service"
```

### ⚠️ 空结果不等于配置错了

重拉过程中查询会返回 `{"data":[]}`。**先判断是不是还没拉完,再怀疑配置**:

```bash
ssh gyy "ls -la /opt/YunBright/cube/semantic-layers/sixun-ysx/data/"   # 看 .duckdb/.wal 时间戳
```

### 用 COUNT(*) 自检有没有被 row_limit 截断

```bash
curl -s -X POST http://127.0.0.1:8083/query -H 'Content-Type: application/json' \
  -d '{"measures":["product.count"],"dimensions":[]}'
# → 27299 == 源库实测行数 ✅
# → 10000 / 50000 这类整数 = 被 row_limit 截断了 ❌
```

`COUNT(*)` **恰好等于某个上限值**,基本可以断定数据被静默截断。

多 family 一致性验证:同一条 query body 打不同端口,比对返回的 `sql` 字段是否**逐字节相同**。

## row_limit(拉取行数上限)

`source.DefaultRowLimit = 50000`(`semantic-layers/sixun/internal/source/source.go`),
`config.yaml` 可按角色覆盖:

```yaml
source:
  row_limit: 50000        # 未覆盖角色的兜底
  row_limits:
    product: 50000         # 源库实测 27299 / 44313
    stock:   40000         # 源库实测 23576 / 10607
    supplier: 2000
    category: 2000
    # sale 流水表不要跟着放大 —— 它才是该单独限流的那个
```

2026-10-09 实测:硬编码 `TOP 10000` 曾让 ysx 静默丢 63% 商品、hbposv7 丢 77%,
**库存表还丢 57%**(会让盘点账面数量直接是错的),而全链路零报错。
实测 27299 行 × 65 列全量拉取仅 3.6MB / 1.4s。
**默认值必须高于所有维表的实测行数** —— 正确性不能依赖"记得配这一项"。

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
| Scheduler 不可用(广播地址错 / 容器停) | 启动时登记失败 → 自动回退 ticker,`refresh.mode=ticker` + `schedule_error` 有值;数据照常刷新。Scheduler 恢复后由 job 触发自动切回,不必重启 app |
