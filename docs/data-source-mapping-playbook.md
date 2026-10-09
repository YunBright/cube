# 数据源勘察与 mapping 编写手册

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
| 2 | **mapping 是白名单** | schema 加了 dimension,DuckDB 里**根本没有这一列**,查询报 column not found |
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

实测值(2026-10-09),这张表直接决定了 `row_limit` 怎么配:

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

思迅的 char 定长列普遍补空格,而 **DuckDB 会原样保留**。
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
       sql: unit          ← 这是 DuckDB 侧的列名,由 mapping 产出
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

**mapping 条数 = DuckDB 物理列数。** `product` 的 DuckDB 表当前 **9 列**
(`id / name / category_id / supplier_id / unit / price_yuan / cost_yuan / status / created_at`),
正好等于 `mapping-product.yaml` 的 9 条映射(加 `unit` 之前是 8 列)。

schema.yaml 里 dimension + measure 共 13 条,**不等于**物理列数 ——
measure 是查询时聚合出来的,不对应物理列。数列时别拿 schema 条数去对。

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

### 3.3 schema/mapping 是**运行时读盘**,不是编译进二进制

`boot.ResolveModelsDir()` 用 `os.Stat` 探测路径(`CUBE_MODELS_DIR` 或相对路径),
**没有 `go:embed`**。所以:

- 只部署二进制 = 模型完全没变
- `deploy-cube.ps1` 的 tar 里**包含** mapping/ 与 sixun-models/,正常部署会带上
- 但 `config.yaml` **不在 tar 里**(手工维护),不会被动覆盖 —— 这点是好事

部署后**必须主动验证**远端文件真的更新了,不要假设:

```bash
ssh gyy "grep -n 'target: unit' /opt/YunBright/cube/semantic-layers/sixun-ysx/mapping/mapping-product.yaml"
ssh gyy "grep -n -A2 'name: unit' /opt/YunBright/cube/sixun-models/product/schema.yaml"
```

---

## 4. 让改动真正生效:全量重拉

### 4.1 必须重启才会重拉

新增列要写进 DuckDB,唯一的路径是启动时的 `Fetch → mapping → LoadFrom`。
所以改完 schema/mapping 后**必须 `systemctl --user restart cube-sixun-*.service`**。

```bash
ssh gyy "systemctl --user restart cube-sixun-ysx.service && sleep 90 && systemctl --user is-active cube-sixun-ysx.service"
```

### 4.2 **空结果不代表配置错了**

本次在重拉过程中查了一次,返回 `{"data":[]}` —— 当时几乎要回滚。
实际上只是**表正在重建**。

正确的判读顺序:

```bash
# 1. 看 duckdb 文件时间戳/大小有没有在动
ssh gyy "ls -la /opt/YunBright/cube/semantic-layers/sixun-ysx/data/"
# 2. 再等一会儿重查
# 3. 最后才怀疑配置
```

**先看"是不是还没拉完",再想"是不是配错了"。**

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

这条规则已固化在共用包 `pkg/cubequery.Build` 里,见 §5。

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
pkg/cubequery/build.go   ← Query → SQL 的唯一实现
   ├── cmd/sixun-ysx/main.go      调 cubequery.Build
   └── cmd/sixun-hbposv7/main.go  调 cubequery.Build
```

**补丁只能修这一次症状;共用代码才能让分叉不可能再发生。**

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

# ③ 行数 = 源库实测行数(证明没被 row_limit 截断)
curl -s -X POST http://127.0.0.1:8083/query -d '{"measures":["product.count"],"dimensions":[]}'
# → 27299 == §1.2 实测值;若正好等于 10000,就是被截断了

# ④ 目标数据真的有值,且 SQL 里出现了新字段
curl -s -X POST http://127.0.0.1:8083/query -d @/tmp/query.json
# → data[0] 含 product.unit;resp.sql 里含 RTRIM(unit)

# ⑤ 多 family 一致性(§5.2)
```

第 ③ 项是最容易被跳过、但**最能暴露截断**的一项。
`COUNT(*)` 恰好等于 `DefaultRowLimit` 或某个整数上限时,基本可以断定被截了。

---

## 7. row_limit:默认值必须高于实测行数

```go
// semantic-layers/sixun/internal/source/source.go
const DefaultRowLimit = 50000
```

`row_limits` 按角色独立配置(`config.yaml`):

```yaml
source:
  row_limit: 50000        # 未覆盖角色的兜底
  row_limits:
    product: 50000         # 实测 27299 / 44313
    stock:   40000         # 实测 23576 / 10607
    supplier: 2000         # 实测 218 / 300
    category: 2000         # 实测 186 / 596
    # sale 流水表**不要**跟着放大 —— 它才是该单独限流的那个
```

两条规则:

1. **默认值必须高于所有维表的实测行数。** 漏配配置时,"多拉一点"远比"静默丢数据"安全。
   正确性不能依赖"记得配这项"。
2. **只有真正常驻增长的表才配上限。** 本项目流水表实测 0 行,
   真要限流时单独配 `sale`,别用同一个值把维表和流水表一起放开。

---

## 8. 测试要锁住"正确约束",不是"当前实现"

本次发现仓库里一条测试:

```go
func TestDefaultRowLimitIsBelowRealProductTableSize(t *testing.T) {
    // 断言 DefaultRowLimit < 27299,注释写"提醒默认值不是安全网"
}
```

它把**缺陷本身变成了规格**。于是:
后来有人把 `TOP 10000` 外置成配置(确实是进步)→ "忘记配置"退回默认值 →
**缺陷复活,而这条测试因为缺陷还在而全绿**。

正确的方向是反过来,锁住真实约束:

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
| 2 | `COUNT(*)` 实测 4~5 张表行数 | 决定 `row_limits` |
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