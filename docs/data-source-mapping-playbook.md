# 数据源勘察与 mapping 编写手册

> ⚠️ **2026-10-23:本文档里关于本地副本 / 预聚合 / 刷新 job / 水位 / 行数上限 的部分已作废。**
> 全部 model 改成 `storage: live`(查询时把 schema 编译成 T-SQL 直打源库),
> **不保留任何本地副本、不做任何原始数据同步**。
> 规矩与理由见 **AGENTS.md §4.1**;现状见 **docs/cache-strategy.md**。
> 下面保留的是"当时的决策与踩过的坑",**读的时候按历史资料对待** ——
> 照着它加"刷新 job / 预聚合 / 水位"会直接违反 AGENTS.md §4.1。
>
> 这份手册来自 2026-10-09 一次真实排障:为了把「商品单位」显示到盘点页,
> 需要给 `product` model 加一列 `unit`,顺带修掉了三个会**静默出错**的缺陷
> (行数截断 / 两套查询实现分叉 / RTRIM 缺失)。
>
> 本文回答两个问题:
> 1. **怎么准确可靠地把一个陌生数据源摸清楚**(而不是靠猜)
> 2. **摸清楚之后,mapping / schema / 全量重拉 / 验证**按什么顺序做

---

## 0. 先记住这四条铁律

在读任何细节之前,先接受这四条。它们对应本次踩的每一个坑。

| # | 铁律 | 违反后的症状 |
|---|---|---|
| 1 | **连接不通时不要猜任何列名** | 猜错 → 整表 `LoadFrom` 失败 / 或更糟:拉到空数据 |
| 2 | **mapping 是白名单** | schema 加了 dimension,mapping 里**根本没有这一条**,查询报 column not found |
| 3 | **共享 schema 必须所有 family 一起改** | 改一边 → 另一边查该字段直接 SQL 报错 |
| 4 | **任何静默截断 / 静默忽略都比报错危险** | 过载会报错;截断只会让大半数据凭空消失,且 healthz 全绿 |

第 4 条是本次真正的教训。三次故障的共同点是**没有任何报错**:
商品被截断 63%、`hbposv7` 丢弃全部 filters、`hbposv7` 的条码少个 `RTRIM`。
每一种都是 HTTP 200 + 看似合理的数据。

---

## 1. 勘察数据源:五个必须实测的问题

不要相信任何文档(包括思迅的官方表结构说明),也不要靠字段名语义猜。
**连上库,用 SQL 问出来。**

### 1.1 连得上吗 —— 先解决连接,再谈别的

```powershell
Test-NetConnection -ComputerName 127.0.0.1 -Port 1433 -InformationLevel Quiet
# ssh 侧也要单独确认(cube app 跑在远端,本机能连 ≠ 远端能连)
ssh gyy "timeout 8 bash -c '</dev/tcp/192.168.1.200/1433' 2>&1 && echo OK || echo BLOCKED"
```

> **本次教训**:源库隧道断了,导致「列名是什么」这个本该 5 分钟查清的问题卡了两天。
> 更糟的是,在没查清之前**很容易动手去猜列名**(比如猜 `unit`、`unit_name`、`spec_unit`),
> 猜错会让整张表的 `LoadFrom` 失败。**连不通的唯一正确动作是把它当成阻塞项上报,不是猜。**

DSN 从 cube app 的配置里读(不要问人要):

```bash
ssh gyy "grep -n 'dsn' /opt/YunBright/cube/semantic-layers/sixun-ysx/config.yaml"
# → source.dsn: "sqlserver://<user>:<pass>@<host>:1433?database=<db>&encrypt=disable&trustservercertificate=true"
```

### 1.2 这张表**有多大** —— 直接 COUNT(*),不要估

```sql
SELECT COUNT(*) FROM t_bd_item_info;
SELECT COUNT(*) FROM t_im_branch_stock;
SELECT COUNT(*) FROM t_bd_supcust_info;
SELECT COUNT(*) FROM t_bd_item_cls;
```

实测值(2026-10-09)。⚠️ **2026-10-23:`row_limit` / `row_limits` 整套配置已删除**
(那是给"拉全量物化副本"封顶用的;副本层没了 ⇒ 没有"拉"这一步 ⇒ 没有上限可配)。
今天这张表只用来判断"查询会不会太慢",不再用来配上限:

| 表 | ysx / hbposv10 | hbposv7 / hbposepro |
|---|---|---|
| `t_bd_item_info` 商品 | 27299 | 44313 |
| `t_im_branch_stock` 库存 | 23576 | 10607 |
| `t_bd_supcust_info` 供应商 | 218 | 300 |
| `t_bd_item_cls` 分类 | 186 | 596 |
| `t_mp_saleflow` / `t_rm_order_saleflow` 流水 | 0 | 0 |

> **为什么要实测**:硬编码 `SELECT TOP 10000` 曾让 ysx 静默丢掉 63% 商品、hbposv7 丢掉 77%,
> 库存表还丢 57%。而实测「27299 行 × 65 列全量拉取 = 3.6MB / 1.4s」——
> **"别一次性拉太多"在这个量级上是一个想象出来的约束**。过载会报错,截断不会。

### 1.3 有哪些候选列 —— 查 `INFORMATION_SCHEMA`,别猜

```sql
-- 先按语义模糊捞一遍候选
SELECT COLUMN_NAME, DATA_TYPE, ISNULL(CAST(CHARACTER_MAXIMUM_LENGTH AS VARCHAR(10)),'') AS maxlen
FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_NAME = 't_bd_item_info'
  AND (COLUMN_NAME LIKE '%unit%' OR COLUMN_NAME LIKE '%spec%'
       OR COLUMN_NAME LIKE '%pack%'  OR COLUMN_NAME LIKE '%std%'
       OR COLUMN_NAME LIKE '%aux%'  OR COLUMN_NAME LIKE '%uom%')
ORDER BY ORDINAL_POSITION;
-- → unit_no | char | 4
```

`maxlen = 4` 本身就是线索:能放两个汉字,不可能是 `0012` 这种编号。

### 1.4 这一列**装的是什么值** —— 必须 SELECT 真实行

这一步决定了架构,不能跳。`SELECT TOP 5 * FROM t_bd_item_info WHERE item_no IN (...)`:

```
item_no        | item_name                    | unit_no | sale_price | item_size
6922303199721  | 唯得水乡特色S码抽纸（8包装）  | 提      | 15.0000    | 1*10
6957583900828 | 凤派XL码抽纸8包              | 提      | 9.9000     | 1*8*10
```

`unit_no` 的值**直接就是「提」这个单位名本身,不是单位编号**。
所以 `product.unit` 可以直接映射,不需要 join 单位表、不需要 enum_map。

> 如果它装的是 `"0012"`,那 schema 就得多一个 `unit` model + join,
> 这是完全不同的工作量。**一个字段的值形态,决定整个建模方案。**

### 1.5 char 定长列有没有补空格

```sql
SELECT '[' + item_no + ']' FROM t_bd_item_info WHERE item_no LIKE '6922303199721%';
-- ysx:     [6922303199721]         ← 查询侧 RTRIM 即可
-- hbposv7: [6922303199721       ]  ← 尾部 7 个空格
```

思迅的 char 定长列普遍补空格,而源库会**原样保留**。
查询层统一 `RTRIM`,详见 §4.3。

---

## 2. 所有 family 都要查 —— 共享 schema 的前提

`sixun-models/` 是 ysx 与 hbposv7 **共用**的 schema。
所以定任何一个字段之前,必须确认**两个源库都有对应列**:

```sql
-- 在 ysx 源库(hbposv10)查一次
SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_NAME='t_bd_item_info' AND COLUMN_NAME LIKE '%unit%';
-- 再在 hbposv7 源库(hbposepro)查一次
-- → 两边都有 unit_no,才可以只加一次 schema、两边各加一条 mapping
```

如果只有一个库有,就必须二选一:
- 给另一个 family 映射一个**恒空值**(语义不诚实,不推荐)
- 把该字段拆到 family 私有的 schema(违反共享设计)

**先查再动手,不要写完才发现另一边没有。**

---

## 3. 改 mapping / schema:顺序与影响面

### 3.1 加一个字段,需要改三处

以「加 `product.unit`」为例,三处缺一不可:

```
① sixun-models/product/schema.yaml              加 dimension
     - name: unit
       sql: unit          ← canonical 侧的列名,由 mapping 产出
       type: string
       title: 商品单位

② semantic-layers/sixun/cmd/sixun-ysx/mapping/mapping-product.yaml
     - source: unit_no    ← 源库列名(勘察出来的)
       target: unit       ← 必须与 schema 的 name 对齐
       type: string

③ semantic-layers/sixun/cmd/sixun-hbposv7/mapping/mapping-product.yaml
     同上,两个 family 都要写
```

### 3.2 mapping.yaml 的边界(AGENTS.md §4 拍板,不要越界)

**mapping 条数 = model 的物理列数。** `product` 的 mapping 当前 **9 条**
(`id / name / category_id / supplier_id / unit / price_yuan / cost_yuan / status / created_at`),
正好等于它能查出来的列数(加 `unit` 之前是 8 条)。

schema.yaml 里 dimension + measure 共 13 条,**不等于**物理列数 ——
measure 是查询时聚合出来的,不对应物理列。数列时别拿 schema 条数去对。

> ⚠️ 现在的硬约束由 `mapping_parity_test.go` 断言:**mapping 的 target 集合
> == schema 的 dimension 集合**(两个 family 都查)。少一条 → 源库 column not found;
> 多一条 → schema 不认识它。

```yaml
mappings:
  - source: unit_no        # 只写:字段名 / 类型 / 单位
    target: unit
    type: string
```

**禁止**:
- ❌ `enum_map` / `transform`(枚举归一化留给 schema meta + BI)
- ❌ 跨字段派生表达式(需要 JEXL 引擎,不 MVP)

需要清洗的脏值怎么办?**原样留着**。
实测确实存在脏数据(130 条商品的 `unit_no` 是 `"1*12"` 这种规格串),
但那是**数据质量问题,不是 mapping 的问题** —— 在 mapping 里偷偷清洗等于
把"源库长这样"这个事实藏起来,让排查时无从查证。

### 3.3 schema/mapping 已用 `go:embed` 编译进二进制(2026-10-09 起)

- schema:`sixun-models/<model>/schema.yaml`,经 `sixun-models/embed.go` 的 `models.FS()` 暴露
- mapping:`semantic-layers/sixun/cmd/<binary>/mapping/mapping-<model>.yaml`,每个 binary 一份

所以现在:

- 部署**只推二进制**,`deploy-cube.ps1` 的 tar 里**不再包含** mapping/ 与 sixun-models/
  (脚本的 `prune` 步骤会把远端遗留的这两个目录删掉 —— 留着比删掉更危险,
  因为 `ls mapping/` 成功会让人以为"模型已部署",而运行的其实是二进制里的内嵌副本)
- **改了 schema/mapping 必须重新构建**,只重推旧二进制不会有任何变化
- `config.yaml` 仍在盘上(含源库 DSN 口令),也仍不在 tar 里 —— 这点是好事,不会被覆盖
- `CUBE_MODELS_DIR` / `CUBE_MAPPING_DIR` 已删除。读盘路径静默失败:路径对不上时
  一个模型都加载不到,启动却**不报错**,要到查询时才炸

改完先跑资产完整性测试(缺 mapping / 空 mapping / 拼错 model 名都会红):

```bash
cd semantic-layers/sixun
go test ./cmd/sixun-ysx/... ./cmd/sixun-hbposv7/... ./internal/embedcheck/...
```

部署后看启动日志确认二进制带了新模型:

```bash
ssh gyy "journalctl --user -u cube-sixun-ysx.service -n 200 | grep 'loading schemas'"
```

### 3.4 新增 model 的完整清单(加 go:embed 之后)

1. `sixun-models/<model>/schema.yaml` + `preagg.go`
2. `semantic-layers/sixun/cmd/sixun-ysx/mapping/mapping-<model>.yaml`
3. `semantic-layers/sixun/cmd/sixun-hbposv7/mapping/mapping-<model>.yaml`
4. 若是 `storage: live`:该实例 `config.yaml` 补 `table_<model>`
5. `go test ./...` —— 少任何一步 `embedcheck` 都会直接失败
6. 重新构建部署 + 重启(§4)

第 2、3 步以前可以漏(漏了只是少一份 mapping),现在会**测试失败**;
第 4 步漏了则是启动期日志里的 `live model 缺 source.table_*`,该 model 不可查。

---

## 4. 让改动真正生效:重新构建 + 重启

> ⚠️ **2026-10-23:这一节重写过。**
> 以前改完 schema/mapping 要"全量重拉"—— 因为东西要写进本地副本。
> 现在**没有副本、也没有"拉"这一步**:schema/mapping 是 `go:embed` 编译进二进制的,
> 所以生效路径只有一条:**重新构建 → 重新部署 → 重启进程**。

### 4.1 必须重新构建 + 重启

改 schema/mapping 后必须**重新构建二进制并重启**
(`systemctl --user restart cube-sixun-*.service`)。
只重启不重建 ⇒ 跑的还是旧二进制里的旧 schema(模型是 embed 进去的)。

```bash
ssh gyy "systemctl --user restart cube-sixun-ysx.service && sleep 90 && systemctl --user is-active cube-sixun-ysx.service"
```

### 4.2 **空结果不代表配置错了**

⚠️ 旧流程里"查一次返回 `{"data":[]}`"是**表正在重建**造成的;
现在没有重建这一步 ⇒ 空结果就是**真的空**,要么过滤条件写错,
要么源库那段区间本来就没有数据。

正确的判读顺序:

```bash
# 1. 看进程是不是刚重启、日志里有没有 "live model 缺 source.table_*"
ssh gyy "journalctl --user -u cube-sixun-ysx.service -n 200 | grep -E 'live model|schemas loaded'"
# 2. 去掉过滤条件再查一次(确认不是被过滤掉的)
# 3. 最后才怀疑 mapping / schema
```

**先看"是不是没加载上",再想"是不是配错了"。**

### 4.3 RTRIM:统一在查询层,不要在数据层

char 定长列的补空格有两个处理位置,选**查询层**:

- ✅ 查询层 `RTRIM(col)`(SELECT 表达式与 filter **两侧**都包)
- ❌ 加载层 trim 掉(会丢失"源库原样"这个事实,且 mapping 禁止 transform)
- ❌ 调用方 LIKE / trim 兜底(把 schema 问题推给每个调用方)

**GROUP BY 用原列,不要用 RTRIM 后的表达式**:

```sql
SELECT RTRIM(id) AS "product.id", COUNT(*) FROM product
WHERE RTRIM(id) = ?          -- filter 侧也要 RTRIM
GROUP BY id                  -- 裸列名
LIMIT 5
```

这条规则已固化在共用包 `pkg/cubequery.BuildTSQLJoin` 里,见 §5。

---

## 5. 共享逻辑必须只有一份物理代码

> 本次最严重的一处缺陷,和 mapping 无关 —— 是**同一份 schema 被两套查询实现消费**。

历史上 `cmd/sixun-ysx/main.go` 与 `cmd/sixun-hbposv7/main.go` 各写了一份 `queryHandler`:

| 能力 | sixun-ysx | sixun-hbposv7(修复前) |
|---|---|---|
| 多 measure / 多 dimension | ✅ | ❌ 只取 `[0]` |
| **filters** | ✅ | ❌ **完全忽略** |
| limit | ✅ 读 `q.Limit` | ❌ 硬编码 1000 |
| RTRIM | ✅ | ❌ |

**filters 被丢弃不是"功能少",是返回错误的数据**:
带 filter 的查询退化成"取前 1000 行",而 supertrade 的 `GetProduct` 直接取 `data[0]`
→ 扫一个条码可能拿到**另一个商品**的库存,200 OK、零报错。

修法**不是**给 hbposv7 打一个 RTRIM 补丁,而是把 SQL 构造抽进共用包:

```
pkg/cubequery/                ← Query → SQL 的唯一实现
   build.go   buildFilter / buildOrder / TrimExpr   (共用逻辑)
   tsql.go    rewriteColumns / numberPlaceholders    (T-SQL 方言,live 透传)
        ↑ buildOrder 与 buildFilter 是**两个方言共用**的,只有别名列名引号不同
   ├── cmd/sixun-ysx/main.go      调 BuildTSQLJoin
   └── cmd/sixun-hbposv7/main.go  同上
```

**补丁只能修这一次症状;共用代码才能让分叉不可能再发生。**

> ⚠️ 2026-10-23 之前 `build.go` 里的 `Build` 是**打本地副本**的第二条路
> (别名引号用 `"`、日期用 `DATE_TRUNC`、分页用 `LIMIT`)。
> 副本层删除后只剩 `BuildTSQLJoin` 一条 ⇒ **不再有两套方言可分叉**。
> `build.go` 里那一半 DuckDB 方言暂时留在代码里(它只被自己的单测覆盖),
> 清理它是独立的一次改动 —— 见 AGENTS.md §4 的"存储分层"行。

> 加 ORDER BY 时这条规则直接生效:`buildOrder` 写在 `build.go`,
> 两条路径只通过 `quote` 参数区分别名引号(`"` vs `[`),
> 并由 `TestOrder_DialectsAgree` 锁住"同一条 query 两条路径逐字一致"。
> 若当初把 ORDER BY 各写一份,就会重演"live 有排序、副本没有"的分叉。

### 5.1 抽共用代码时的陷阱:顺手"优化"语义

把 ysx 的 `buildFilterExpr` 搬进共用包时,我一度把 `contains` 从 `%v%`(子串)
写成了 `v%`(前缀),还漏了 `notContains` / `startsWith`。

**那会悄悄改掉 ysx 线上既有行为,而编译和测试都不会报。**

规则:
- 搬代码前**逐字读原实现**,不要凭印象
- 为**每个分支**补测试,而不是只测 happy path
- 给容易被"优化"掉的语义单独写一条锁,并在测试注释里写明**为什么不能改**
  (例:`TestBuild_ContainsIsSubstringNotPrefix` 注明"ysx 另有 startsWith 才是前缀")

### 5.2 最强验证:同一条查询打多个 family,比对 SQL 字符串

```bash
# 同一份 query body 打两个端口,直接比 resp.sql
curl -s -X POST http://127.0.0.1:8083/query -H 'Content-Type: application/json' -d @/tmp/parity.json
curl -s -X POST http://127.0.0.1:8082/query -H 'Content-Type: application/json' -d @/tmp/parity.json
```

修复后两边返回**逐字节相同**的 `sql` 字段:

```sql
SELECT RTRIM(id) AS "product.id", RTRIM(name) AS "product.name", RTRIM(unit) AS "product.unit",
       COUNT(*) AS "product.count", AVG(price_yuan) AS "product.avg_price_yuan"
FROM product WHERE RTRIM(id) = ? GROUP BY id, name, unit LIMIT 5
```

**比对 SQL 字符串 >> 断言"返回行数 > 0"**。后者两边都能骗过。

---

## 6. 验证清单

改完 mapping/schema,按顺序逐项过。**任何一项没验证就不要宣布完成。**

```bash
# ① 远端文件确实更新了(部署可能没带上)
ssh gyy "grep -n 'target: unit' .../sixun-ysx/mapping/mapping-product.yaml"

# ② 服务已用新配置重启
ssh gyy "systemctl --user is-active cube-sixun-ysx.service"

# ③ 行数 = 源库实测行数(证明没被截断)
curl -s -X POST http://127.0.0.1:8083/query -d '{"measures":["product.count"],"dimensions":[]}'
# → 与 §1.2 实测值同量级;若正好等于 10000 / 50000 这类整数,基本可以断定被截了

# ④ 目标数据真的有值,且 SQL 里出现了新字段
curl -s -X POST http://127.0.0.1:8083/query -d @/tmp/query.json
# → data[0] 含 product.unit;resp.sql 里含 RTRIM(unit)

# ⑤ 多 family 一致性(§5.2)
```

第 ③ 项是最容易被跳过、但**最能暴露截断**的一项。
⚠️ 2026-10-23:`row_limit` / `DefaultRowLimit` 已删除 —— 现在**没有任何配置上限**,
所以这一项从"检查有没有被上限截断"变成"检查**源库侧**是不是本来就少"。

---

## 7. 行数上限(`row_limit`)—— **已删除**

> ⚠️ **2026-10-23:整套机制删除。**
>
> 它存在的前提是"cube 会把 model 拉全量物化进本地副本",
> 上限的作用是给那次拉取封顶(`SELECT TOP n *`,无 ORDER BY ⇒ 截断丢的是随机行)。
>
> 副本层删除后 cube **根本不拉数** ⇒ 没有"拉"这一步 ⇒ 没有上限可配。
> `row_limit` / `row_limits` 配置键、`source.DefaultRowLimit`、`source.RowLimits`
> 与 `source.Role*` 全部删除。
>
> 今天的对应物只有一条:**查询里的 `limit`**
> (`pkg/cubequery.DefaultLimit = 1000` 是没给 limit 时的兜底,
> 调用方 cube-router 每页 5000)—— 它限制的是**返回给调用方的行数**,
> 不是"从源库拉多少行",而且是**显式**的、每次查询都在 SQL 里看得见的。

```yaml
# ❌ 2026-10-23 起这些键不再被读取(留着无害,但会被误以为还在生效)
source:
  row_limit: 50000
  row_limits:
    product: 50000
```

历史教训仍然成立(只是现在没有"默认值"可配了):
**过载会报错,截断不会** —— 任何写死的 `TOP n` 都要拿实测行数对一遍。

---

## 8. 测试要锁住"正确约束",不是"当前实现"

⚠️ 2026-10-23:`DefaultRowLimit` 相关的那两条测试**随机制一起删除**。
下面留作**反面教材**(它们曾经把一个缺陷钉成了规格):

```go
func TestDefaultRowLimitIsBelowRealProductTableSize(t *testing.T) {
    // 断言 DefaultRowLimit < 27299,注释写"提醒默认值不是安全网"
}
```

它把**缺陷本身变成了规格**。于是:
后来有人把 `TOP 10000` 外置成配置(确实是进步)→ "忘记配置"退回默认值 →
**缺陷复活,而这条测试因为缺陷还在而全绿**。

正确的方向是反过来,锁住真实约束(下面是**当时的**写法,机制已删):

```go
func TestDefaultRowLimitCoversRealTableSizes(t *testing.T) {
    cases := []struct{ table string; rows int }{
        {"ysx t_bd_item_info (商品)", 27299},
        {"hbposv7 t_bd_item_info (商品)", 44313},
        // ... 四个 family × 四张表全部列出
    }
    for _, c := range cases {
        if DefaultRowLimit < c.rows {
            t.Fatalf("DefaultRowLimit %d < 实测 %d (%s):会静默截断维表", ...)
        }
    }
}
```

**发现某条测试在保护一个已知缺陷时,先问"这个行为是对的吗",再问"代码符合测试吗"。**

---

## 9. 速查:本次的完整改动序列

| 步骤 | 动作 | 验证点 |
|---|---|---|
| 1 | 确认源库连通(本机 + cube 所在机器) | 不连通则停,不猜列名 |
| 2 | `COUNT(*)` 实测 4~5 张表行数 | 判断查询代价(旧流程用它定 `row_limits`,那套已删) |
| 3 | `INFORMATION_SCHEMA` 找候选列 | 拿到准确列名 |
| 4 | `SELECT 真实行` 看值形态 | 决定要不要 join |
| 5 | 在**另一个 family 源库**重复 3~4 | 确认共享 schema 可行 |
| 6 | 改 schema + 两个 family 的 mapping | 三个文件 |
| 7 | `gofmt` + `go build` + `go test` | |
| 8 | `deploy-cube.ps1` | 远端 grep 确认文件已更新 |
| 9 | `systemctl --user restart cube-*` | 等 90s |
| 10 | `COUNT(*)` 对比实测值 | 确认没截断 |
| 11 | 同一条查询打所有 family 比对 SQL | 确认一致 |

---

## 相关文档

- [语义层设计](semantic-layer-design.md) —— schema / mapping / 枚举 / 预聚合
- [运行时运维](runtime-ops.md) —— env、多 instance 启动、故障恢复
- [Dapr app 协议](dapr-app-contract.md) —— `/query` 与 `/v1/source/{source}/load` 的契约
- [AGENTS.md](../AGENTS.md) §4 —— 拍板决策表(改 schema/mapping 前必读)