# 运行时运维(P0-4)

> ⚠️ **2026-10-23:本文档里关于本地副本 / 预聚合 / 刷新 job / 水位 / 行数上限 的部分已作废。**
> 全部 model 改成 `storage: live`(查询时把 schema 编译成 T-SQL 直打源库),
> **不保留任何本地副本、不做任何原始数据同步**。
> 规矩与理由见 **AGENTS.md §4.1**;现状见 **docs/cache-strategy.md**。
> 下面保留的是"当时的决策与踩过的坑",**读的时候按历史资料对待** ——
> 照着它加"刷新 job / 预聚合 / 水位"会直接违反 AGENTS.md §4.1。
>
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

> **核心变化**:cube app 的所有差异(数据源 DSN / 端口 / 注册地址)
> 都通过 **环境变量** 注入,同一份 binary 可以服务多个 instance(门店)。

#### 通用环境变量(`boot.Load()` 读取)

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `CUBE_APP_ID`     | ✓ | —   | **wire source id**(`<family>-<version>-<instance>`);family / version / instance 由其拆分得到 |
| `DAPR_APP_ID`     |   | (dapr 自动注入) | dapr sidecar app-id。**首选源** — dapr 启动时自动注入。/register body 用此上报给 gateway 当 dapr app-id |
| `CUBE_DAPR_APP_ID`|   | `"cube-" + CUBE_APP_ID` | 手动覆盖 dapr app-id。空值走推导默认值。**通常不需要设** |
| `CUBE_PORT`       |   | `:8080` | gin 监听端口 |
| `CUBE_GATEWAY_URL`|   | `http://localhost:8080` | cube-gateway HTTP base |

> ⚠️ **2026-10-23:两个 env 一起删掉了** ——
> `CUBE_DUCKDB_PATH`(本地副本文件路径)与 `CUBE_REFRESH_EVERY`(副本定时重拉间隔)。
> 没有副本 ⇒ 既没有路径也没有"重拉"这回事。
> 留着它们会让人以为实例还在自己维护一份数据副本。

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

### 本地副本运行时依赖 —— **已删除**

> ⚠️ **2026-10-23:整节作废。**
>
> 当时 `pkg/duckdb` 用 `github.com/fpt/go-pduckdb`(纯 Go driver),
> 运行期还要在机器上装 `libduckdb` 共享库(Windows / macOS / Linux 三套安装步骤),
> Docker 镜像也要多一个下载 stage。
>
> 这些**全部删除**:`pkg/duckdb` 包没了,依赖 `go-pduckdb` 也从
> `pkg/go.mod` 移除了(2026-10-23 `go mod tidy`)。
>
> ⇒ **现在没有任何 DuckDB 运行时依赖** —— 只剩 `go-mssqldb`(直连思迅源库)。
> 机器上不需要装任何额外的共享库,镜像也不需要额外的下载 stage。
> 规矩与理由见 **AGENTS.md §4.1**。

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
│  (无 PVC:实例不持有任何数据副本)             │
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
  "source": "sixun-ysx-00",
  "dapr_app_id": "cube-sixun-ysx-00",
  "models": ["product", "member", "..."],
  "uptime": "12m3s"
}
```

- `status` **恒为 `ok`** —— 本实例**不持有任何数据副本**,查询实时透传源库,
  所以没有"数据过期"这回事(新鲜度 = 源库的新鲜度)。
- ⚠️ 2026-10-23 之前还有 `data`(副本物化状态与年龄)与 `refresh`(刷新 job 统计)
  两段,**现在都删了** —— 留着它们会是**假信号**:一个恒为 "ok" 的健康检查
  会让人以为"有人在管数据的新鲜度"。`internal/boot` 的
  `TestHealthHandler_NoCopySegments` 钉住了这一点。

## 刷新驱动(dapr-job / ticker)—— **已删除**

> ⚠️ **2026-10-23:整节作废。**
>
> 当时 `internal/refreshschedule` 用两种模式驱动副本的定时重拉:
> `dapr-job`(登记到 Dapr Scheduler,sidecar 到点 POST `/job/cube-refresh`)
> 与 `ticker`(登记失败时退回进程内定时器),并把 `mode` / `triggered` /
> `skipped_overlap` 写进 healthz。
>
> 这些**全部删除**:没有副本 ⇒ 没有"重拉"⇒ 没有刷新 job、没有调度器登记、
> 没有 `cube-refresh` / `cube-refresh-full` 这两个 job。
> `config.yaml` 的 `refresh:` 段(`incremental` / `incremental_fallback` /
> `full_schedule`)也一并删除。
>
> ⇒ 现在**没有任何后台任务**。要"只看变过的行",用 model 上显式声明的
> "最后变动时间"维度(如 `product.updated_at`),由**下游编排方**加 `gt` 过滤
> —— 见 AGENTS.md §4「增量过滤维度」行。

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

### 必须重新构建 + 重启

⚠️ 2026-10-23 之前这里是"必须重启才会重拉"(把新列写进本地副本)。
现在**没有副本、也没有"拉"这一步** —— schema/mapping 是 `go:embed` 编译进二进制的,
所以生效路径是:**重新构建 → 重新部署 → 重启**。

```bash
ssh gyy "systemctl --user restart cube-sixun-ysx.service && sleep 90 && systemctl --user is-active cube-sixun-ysx.service"
```

### ⚠️ 空结果不等于配置错了

⚠️ 旧流程里"重拉过程中查一次返回 `{"data":[]}`"是**表正在重建**造成的。
现在没有重建这一步 ⇒ 空结果就是**真的空**:要么过滤条件写错,
要么源库那段区间本来就没有数据。

```bash
ssh gyy "journalctl --user -u cube-sixun-ysx.service -n 200 | grep -E 'live model 缺|schemas loaded'"
# → 有 "live model 缺 source.table_*" ⇒ 是漏配表名,不是数据问题
```

### 用 COUNT(*) 对一下源库实测行数

```bash
curl -s -X POST http://127.0.0.1:8083/query -H 'Content-Type: application/json' \
  -d '{"measures":["product.count"],"dimensions":[]}'
# → 与源库 COUNT(*) 同量级 ✅
# → 10000 / 50000 这类整数 = 被某个写死的上限截断了 ❌
```

⚠️ 2026-10-23:`row_limit` / `DefaultRowLimit` 已删除,现在**没有任何配置上限**。
所以这项从"检查有没有被上限截断"变成"检查**源库侧**是不是本来就少"。

多 family 一致性验证:同一条 query body 打不同端口,比对返回的 `sql` 字段是否**逐字节相同**。

## 行数上限(`row_limit`)—— **已删除**

> ⚠️ **2026-10-23:整套机制删除。**
>
> `source.DefaultRowLimit = 50000`、`source.RowLimits` / `source.NewRowLimits`、
> `source.Role*`、以及 `config.yaml` 的 `row_limit:` / `row_limits:` 键全部删除。
> 它们只服务于"把 model 拉全量物化进本地副本"时的封顶。
>
> 现在的对应物只有一条:**查询里的 `limit`**
> (`pkg/cubequery.DefaultLimit = 1000` 是没给 limit 时的兜底,
> 调用方 cube-router 每页 5000)—— 它限制的是**返回给调用方的行数**,
> 不是"从源库拉多少行",而且**显式**、每次查询的 SQL 里都看得见。

历史教训仍然成立(只是现在没有"默认值"可配了):
**过载会报错,截断不会** —— 任何写死的 `TOP n` 都要拿实测行数对一遍。

2026-10-09 实测:硬编码 `TOP 10000` 曾让 ysx 静默丢 63% 商品、hbposv7 丢 77%,
**库存表还丢 57%**(会让盘点账面数量直接是错的),而全链路零报错。
实测 27299 行 × 65 列全量拉取仅 3.6MB / 1.4s —— **"别一次性拉太多"在这个量级上是想象出来的约束**。

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
