# AGENTS.md — 项目 AI 协作约定

> 本文件给所有 AI 助手(viber coding / Cursor / Aider / MiniMax Code 等)阅读,约束生成代码的风格与边界。
> 任何 AI 在动手前必须读完本文件 + `docs/architecture.md`。
> **要改 schema / mapping / 拉数,再加读 `docs/data-source-mapping-playbook.md`。**

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
| 模型资产交付 | **schema.yaml / mapping.yaml 一律 `go:embed` 编译进二进制**(`sixun-models/embed.go` + 各 cmd 的 `mappingAssets`);只有 `config.yaml` 留盘(含 DSN 口令) | ❌ 运行时读盘(原 `CUBE_MODELS_DIR` 路径探测:**路径对不上时一个模型都加载不到,但启动不报错**,已删除)、❌ 把 DSN 打进二进制、❌ 归档里塞模型文件 |
| 资产完整性 | 由 `internal/embedcheck` 兜底:schema 缺 mapping / mapping 为空 / mapping 拼错 model 名 → **测试失败** | ❌ 靠运行期 INFO 日志提示"mapping missing"然后 continue |
| 配置读取 | `internal/appcfg` 统一:**`config.local.yaml` > `config.yaml`**,存在即生效,不读 env。两份都在 `.gitignore`;`config.example.yaml` 进版本库 | ❌ 用 env 变量指配置路径、❌ 允许 config.local.yaml 留在服务器上(会盖住部署下去的 config.yaml,「部署不生效」且零报错,`deploy-cube.ps1 -Step prune` 负责删) |
| config.yaml 随包发布 | **归档含 config.yaml,部署会用打包机工作区那份覆盖远端**(用户 2026-10-09 决定不移出) | ❌ 只改远端不改本地(下次部署静默还原)、❌ 在别处维护"生产配置" |
| 部署模式 | hosted (k8s) + 本地 dev (docker-compose) | ❌ 其他模式 |
| mapping.yaml 语义 | **仅字段名 + 类型 + 单位**(无 enum_map / 无 transform) | ❌ 写 enum_map、❌ 写 transform |
| 权限分层 | gateway 粗粒度(source 访问)+ cube app 细粒度(行/列) | ❌ gateway 实现全部权限 |
| L1 缓存命中 | 直接返回,**完全跳过 cube app** | ❌ 还调 cube app |
| Query 寻址 | `POST /v1/source/{source}/load` → gateway 按注册时上报的 `dapr_app_id` 调 dapr(plan B 解耦) | ❌ model → app_id 路由 |
| MVP API 范围 | `/v1/source/{source}/load` + `/v1/source/{source}/meta` + `/v1/sources` + `/register` + `/unregister` + `/healthz` | ❌ `/v1/sql`、❌ `/v1/load` |
| Cube app 优雅关闭 | cube app 收到 `SIGTERM` / `SIGINT` → 调 `POST cube-gateway/unregister`(body `{app_id: "..."}`,2s deadline,失败仅记日志)→ `os.Exit(0)`;gateway 立即从 registry + state store 删除条目,**幂等**(未注册也返 204) | ❌ 等下次 invoke 失败再发现(BI 视图长时间脏数据),❌ 阻塞退出等 unregister(会被 kubelet 30s SIGKILL 兜底) |
| 错误信封 | 所有非 2xx 用 `pkg/apierror` 统一形状 `{code, message, details}`,`X-Request-Id` header | ❌ `gin.H{"error":...}`、❌ 各端点自定义 |
| 4xx 子码分层 | **`MODEL_NOT_FOUND` 只表示「该 source 不暴露这个 model」**;dimension/measure 写错 → 400 `QUERY_INVALID`(必须带 `details.ref` 回传字段名);非 `BuildError` 的编译失败 → 500 `INTERNAL_ERROR`。`queryHandler` 必须逐 `BuildError.Kind` 分派,见 `docs/dapr-app-contract.md` | ❌ 兜底成同一个 404(注释写"不能混成 X"而代码正好混成 X,是最危险的状态)、❌ 把内部失败降级成"model 不存在" |
| HTTP 框架 | **`gin-gonic/gin v1.10.x`**(所有 dapr app 入口端点统一用) | ❌ 直接 `net/http`、`❌ chi/echo/fiber` 等其它 web 框架 |
| Source 在线判定 | **被动验证**:handler 不前置 IsOnline 检查,任何已注册 source 都直接 `dapr.InvokeMethod`,真实 `SOURCE_OFFLINE` 由 dapr 真实调用失败(`ErrConnFailure`)触发;`/v1/sources` 视图 status 字段恒为 "online"(信息性,不代表实际可达),LastSeen 仅作运维排查信号 | ❌ 时间窗口式离线判定(冷启动源 90s 后假 offline),❌ 主动探活(复杂、对 middleware.http.bearer 链路脆弱),❌ 直连 app / kube 健康度(直连 `DAPR_APP_CHANNEL_ADDRESS`,k8s pod IP 飘移即失效) |
| 枚举值归一化 | **不做**,留在 schema.yaml meta + BI 层翻译 | ❌ 在 mapping.yaml 写 enum_map |
| 查询语义共用 | **`Query → SQL` 只有一份实现:`pkg/cubequery.Build`**,各 family 的 main.go 都调它 | ❌ 各 main.go 各写一份 queryHandler(2026-10-09 教训:hbposv7 那份**完全忽略 filters** → 带 filter 查询退化成"取前 1000 行",调用方取 data[0] 拿到**错误实体**,200 OK 零报错) |
| 拉取行数上限 | **`DefaultRowLimit = 50000`,必须高于所有维表实测行数**;按角色在 `config.yaml` 的 `source.row_limits` 覆盖 | ❌ 设成小于实测行数的值(曾硬编码 `TOP 10000` → ysx 丢 63% 商品、hbposv7 丢 77%、**库存表丢 57%**,零报错) |
| char 补空格 | 在**查询层**统一 `RTRIM`(SELECT 与 filter 两侧都包,GROUP BY 用裸列),已固化在 `pkg/cubequery` | ❌ 加载层 trim、❌ 调用方 LIKE/trim 兜底 |
| 存储分层 | schema.yaml 里 `storage: live` = **透传源库不落盘**,`storage: duck` = 物化进 DuckDB。**明细/实时(settlement、settlement_line、purchase_sheet(_line)、sale_day、stock)走 live;product / supplier / category / sale_detail 留 duck** | ❌ cube 做原始数据同步、❌ 把明细灌进 DuckDB(cube 只存聚合结果 + 枚举/档案) |
| live 表名来源 | live 源表名**只来自实例 `config.yaml` 的 `table_<model>`**,不进共享 `sixun-models/` | ❌ 在共享 schema 写死源库表名(会让两个 family 的 schema 分叉) |
| join 解析 | **只走 schema 显式声明的 joins**,BFS 找最短路径;链深度不限;列名必须 model 别名限定(`settlement_line.id`);`live → duck` 不可 join,编译期拒绝 | ❌ JIT 自动关联、❌ 把 model 名当物理表名(2026-10-09 真库验证抓到:JOIN 用了 model 名当表名,而单测的假 registry 恰好同名,所以 40+ 单测全绿) |
| 扇出防护 | join 链上存在 `one_to_many` 时,**被放大一侧的 measure → 400**,错误信息指出该用多的那一侧;**不做链式传播**(曾误加,把唯一正确的用法一起拦了) | ❌ 静默返回翻倍的数字 |
| 旧版 SQL Server 兼容 | **能力探测而非配置**:`source.DetectLegacyTSQL` 读 `SERVERPROPERTY('ProductMajorVersion')`,< 12 走 2008 方言(`DATEADD(month, DATEDIFF(month,0,d),0)` + `ROW_NUMBER()`),探测失败保守返 true | ❌ 硬编码 `DATEFROMPARTS` / `OFFSET-FETCH`(两个生产库都是 **2008 R2**,直接语法错)、❌ 用配置开关描述能力(配置会骗人) |
| 只读约束 | `QueryLive` 前置 `source.AssertReadOnly`;live model 的 `preagg.Build` 是显式守卫,永远返错 | ❌ 给 live model 接 fetchers(会静默同步百万行) |

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

> ⚠️ **现状:`skills/` 目录尚未建立**,下表是规划中的 skill 清单。
> 当前这些内容散落在 `docs/` 里;**动手前请先读 `docs/data-source-mapping-playbook.md`**
> (写 mapping / 改 schema 的完整实施办法 + 2026-10-09 实战记录)。

| Skill | 何时读 | 当前落点 |
|---|---|---|
| `add-new-data-source` | 新建家族(如加"粮油") | `AGENTS.md` §8 + `docs/architecture.md` |
| `add-new-version` | 现有家族加新版本 | `AGENTS.md` §8 |
| `add-new-model` | 加新 model(如 customer) | `docs/semantic-layer-design.md` |
| `write-field-mapping` | 写 mapping.yaml | **`docs/data-source-mapping-playbook.md`** |
| `design-schema` | 写 schema.yaml | `docs/semantic-layer-design.md` |
| `write-preaggregation` | 写 DuckDB 预聚合 | `docs/semantic-layer-design.md` |
| `debug-query` | 排查 cube query 慢/错 | `docs/dapr-app-contract.md` |

## 7. 完成前自检

- [ ] `go build ./...` 在**对应 module 目录**能跑通
      (本仓库是多 module + `go.work`,在**仓库根**跑 `go build ./...` 会
      输出 `matched no packages` —— 这不是错误,要在 `pkg/`、
      `semantic-layers/sixun/`、`gateway/` 等各自 module 目录里跑)
- [ ] 新增代码有对应 pkg 接口的最小测试(脚手架阶段允许 TODO)
- [ ] 测试断言的方向服务于**真实约束**,不是当前的实现细节
      (反例:曾有一条测试断言 `DefaultRowLimit < 27299`,把"默认值截断数据"
      这个**缺陷**钉成了规格,导致缺陷复活时它仍然全绿)
- [ ] 改了 schema/mapping → **已重新构建二进制**(模型是 go:embed 进去的,只重推旧二进制不会有任何变化)
      → 已重启 cube app 全量重拉 → 已用 `COUNT(*)`
      确认行数等于源库实测值(没有被 row_limit 截断)
- [ ] 新增 / 改名 model → **两个 family 的 `mapping/mapping-<model>.yaml` 都加了**
      (漏一个 `go test ./cmd/sixun-*/...` 会红,别用"先提交再说"绕过)
- [ ] 改了 `.goreleaser.yaml` 或 deploy 脚本 → 归档里**不含** schema/mapping,
      远端遗留的 `sixun-models/`、`mapping/`、**`config.local.yaml`** 已由
      `deploy-cube.ps1 -Step prune` 删除
- [ ] 改了 `config.yaml` → 改的是**工作区**那份(它不在版本库),
      并确认与远端是否一致;别只改服务器上那份,下次部署会被静默还原
- [ ] 共享 schema(`sixun-models/`)的改动 → **所有 family 的 mapping 都同步改了**
- [ ] 新增 `storage: live` 的 model → **每个部署实例的 `config.yaml` 都补了 `table_<model>`**,
      并已重启验证(缺表名会在启动期报错,不是运行期静默空结果)
- [ ] 改过 `pkg/cubequery` 的 SQL 构造 → **已对两个生产源库各跑一遍真实执行**
      (`go run ./cmd/sqlcheck -config <config.yaml> -family ysx|hbposv7`)。
      单测只比对 SQL 字符串,**不执行**,所以 2008 R2 语法错、JOIN 用错物理表名这类 bug
      只有真库能抓(2026-10-09:40+ 单测全绿,真库 6 个 bug)
- [ ] 没碰 P1 拍板里 ❌ 的事项
- [ ] 新增 family 必须同时加 `skills/add-new-data-source` 的代码示例(若案例缺失)

## 8. 添加子服务(dapr app)

- 是否属于已存在family，是则新建<family>-models 和 semantic-layers\<family>，否则在所属family下追加，参考已存在的semantic-layers.
- 修改.goreleaser.yaml,增加构建配置，参考已存在的semantic-layers.
- 修改..\deployer\deploy-cube.ps1，并提醒用户在服务器上新增system unit.