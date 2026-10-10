# 语义层设计

## 家族 vs 实例 vs Model(v2)

> v2 起,`semantic-layers/<family>/cmd/` 下每 (family, version) 一个 binary,
> 同一 binary 通过 **env(CUBE_APP_ID)** 启动多个 instance(每家门店一个进程)。

```
family (思迅 / 粮油 / 生鲜)
├── sixun-models/                  共享 schema(Go module)
│   ├── supplier/
│   │   ├── schema.yaml            cube 风格 schema
│   │   └── preagg.go              DuckDB 预聚合
│   ├── product/
│   └── sale_detail/
│
└── semantic-layers/sixun/         实例(每版本一个 binary)
    ├── internal/
    │   ├── boot/                  env → Config(Load / HealthHandler / RegisterBody)
    │   └── source/
    │       ├── hbposv7/           思迅7pro connector
    │       └── ysx/               思迅云商x connector
    └── cmd/
        ├── sixun-hbposv7/         思迅7pro binary 入口
        │   ├── main.go            (boot.Load → cfg.AppID)
        │   ├── mapping/           思迅7pro 字段映射
        │   └── config.yaml        (DSN + 表名)
        └── sixun-ysx/             思迅云商x binary 入口
            ├── main.go            (boot.Load → cfg.AppID)
            ├── mapping/           思迅云商x 字段映射
            └── config.yaml        (DSN + 表名)
```

每个 dapr cube app 进程由 `CUBE_APP_ID` 区分,例:

| 二进制 | CUBE_APP_ID (wire) | dapr `--app-id` | family | version | instance |
|---|---|---|---|---|---|
| `sixun-ysx`     | `sixun-ysx-00`        | `cube-sixun-ysx-00`        | sixun | ysx     | 00 |
| `sixun-ysx`     | `sixun-ysx-baiyuan1`  | `cube-sixun-ysx-baiyuan1`  | sixun | ysx     | baiyuan1 |
| `sixun-hbposv7` | `sixun-hbposv7-jiale` | `cube-sixun-hbposv7-jiale` | sixun | hbposv7 | jiale |

family / version / instance 由 `CUBE_APP_ID` 拆分得到,**不设独立 env**。
DSN 与表名仍走 `./config.yaml`(每店可能不同)。

`dapr --app-id` 与 `CUBE_APP_ID` 唯一不同点:多 `cube-` 前缀(plan B 解耦)。
cube app 启动时 dapr 自动注入 `DAPR_APP_ID` 环境变量,cube app 读后上报给 gateway
(详见 `docs/dapr-app-contract.md` §2.1)。

## mapping.yaml 语义(P1-5 选 A)

> 动手前先读 **[数据源勘察与 mapping 编写手册](data-source-mapping-playbook.md)** ——
> 里面有勘察数据源的 5 个必查项、共享 schema 的连带影响、row_limit 的实测依据,
> 以及本次「商品单位」打通的完整过程。

```yaml
version: 1
model: supplier

mappings:
  - source: cups                  # 数据源原始字段
    target: id                    # 统一字段名(与 schema.yaml 对齐)
    type: string
    unit: ""                      # 可选
```

**禁止**:
- ❌ enum_map / transform(枚举归一化留给 schema.yaml meta + BI)
- ❌ 跨字段派生表达式(需 JEXL 引擎,不 MVP)

### mapping 是白名单(高频踩坑)

DuckDB 里一张表**有哪些列,完全由该 model 的 mapping 决定**(不是 `SELECT *`)。
所以:

- ✅ 加字段 = schema.yaml 加 dimension **+** mapping.yaml 加映射
- ❌ 只改 schema → schema 里有 `unit`,但 DuckDB 表里没这列 → 查询报 column not found

> 实例实测:`product` 的 DuckDB 表**当前 9 列**
> (`id / name / category_id / supplier_id / unit / price_yuan / cost_yuan / status / created_at`),
> 其中 `unit` 是 2026-10-09 新增的(加之前 8 列)。
> 注意 schema.yaml 里 dimension + measure 的条数(13)≠ DuckDB 表的列数(9),
> 因为 measure 是查询时聚合出来的,不对应物理列。

### 共享 schema 的连带影响

`六un-models/` 由 ysx + hbposv7 **共用**。
定任何一个字段前,必须确认**两个源库都有对应列**,
且**两个 family 的 mapping 都要写**,漏一边则该 family 查该字段时直接 SQL 报错。

> 实例:思迅 `t_bd_item_info.unit_no`(char),两个 family 的源库都有该列,
> 值**直接就是单位名本身**(实测 `6922303199721` = `"提"`),因此不需要 join 单位表。

### 脏数据原样留着

实测有 130 条商品的 `unit_no` 是 `"1*12"` 这类规格串。
mapping 里**不要**清洗 —— 脏数据是数据质量问题,
在 mapping 里偷偷清洗等于把"源库长这样"这个事实藏起来,让排查时无从查证。

## 枚举值处理(P1-5 副作用)

思迅7pro `category='1'` vs 云商x `category='食品'` **都不归一化**,由 schema.yaml 留 enum_values 给 BI 翻译:

```yaml
# sixun-models/supplier/schema.yaml
dimensions:
  - name: category
    sql: category
    type: string
    enum_values:
      - { value: "1", label: 食品 }    # hbposv7 原值
      - { value: "2", label: 百货 }
      - { value: "3", label: 生鲜 }
      - { value: "食品", label: 食品 }  # ysx 原值
      - { value: "百货", label: 百货 }
      - { value: "生鲜", label: 生鲜 }
```

## DuckDB 预聚合(P0-3)

每个 instance 独立 .duckdb 文件(挂载 PV 持久化),命名规则:

```
data/
├── sixun-ysx-00.duckdb             ← CUBE_APP_ID=sixun-ysx-00(默认路径)
├── sixun-ysx-baiyuan1.duckdb       ← CUBE_APP_ID=sixun-ysx-baiyuan1
├── sixun-hbposv7-jiale.duckdb      ← CUBE_APP_ID=sixun-hbposv7-jiale
└── liangyou-v1.duckdb              ← 未来(占位)
```

`boot.Config.ResolveDuckDBPath()` 默认 `./data/<app_id>.duckdb`;可用 `CUBE_DUCKDB_PATH` 覆盖。

预聚合表名见 `sixun-models/<model>/preagg.go` 的 `PreAggName` 常量,与 schema.yaml 的 `sql_table` 对齐。

## 查询语义由共用包统一提供

`Query → SQL` 的编译**只有一份物理实现**:`pkg/cubequery.Build`。
`cmd/sixun-ysx/main.go` 与 `cmd/sixun-hbposv7/main.go` 都调用它。

> 2026-10-09 之前,两个 main.go 各写了一份 `queryHandler`,其中 hbposv7 那份
> **完全忽略 filters**、只取单个 measure/dimension、LIMIT 硬编码、无 RTRIM。
> 后果不是"功能少"而是**返回错误的数据**:带 filter 的查询退化成"取前 1000 行",
> 调用方取 `data[0]` 就可能拿到**另一个实体**,200 OK、零报错。
> → 共享 schema 的查询语义必须共用同一份代码;补丁只能修一次症状。

RTRIM 规则也固化在共用包里:char 定长列的补空格在**查询层**统一处理,
SELECT 表达式与 filter **两侧**都包,GROUP BY 用裸列名。
详见 [手册 §4.3 / §5](data-source-mapping-playbook.md)。

## Segment:可复用的判定口径(结算审批用)

`segment` 是写在 schema 里的一段静态 SQL 谓词,调用方只传名字
(`"segments": ["purchase_return"]`),由 `pkg/cubequery` 编译进 WHERE。

它存在的理由是**判定条件要复用**:审批机器人、BI 报表、回归脚本判断
"这是不是一张采购退货单"时,不该各写一份 `WHERE trans_no='RO'` ——
写散了就会在某处变成 `'RO '`(char 补空格)或漏了 RTRIM,且两边都不报错。

加载期就有校验(`pkg/cubeschema`):名字唯一、SQL 非空、
**不得含 `?`**(segment 不带参数,占位符会变成无人绑定的位置)、
**不得含子查询**(列名重写是标识符级的盲替换,子查询会误伤)。

> ⚠️ segment / measure 的 SQL 里**只能用 canonical 名**(即 mapping 的 target),
> 由 mapping 在查询时重写成该 family 的源列名。`mapping_parity_test.go` 对
> measure 与 segment 都查这一条 —— segment 曾经漏查,写源库列名时
> 恰好在源列名一致的两个 family 上能跑,不一致时才炸。

### 现有 segment

| model | segment | 含义 |
|---|---|---|
| `settlement_line` | `purchase_only` | 结算明细里属于采购的行(`voucher_id` 前缀 PI/RO) |
| `purchase_sheet` | `purchase_receipt` | 仅采购收货单(PI) |
| `purchase_sheet` | `purchase_return` | 仅采购退货单(RO) |

### 采购退货单:数据在哪、金额符号怎么算

**先回答一个常见误解:pm 模块没有独立的退货单表。**
实测两库 `t_pm%` 只有三张:`t_pm_sheet_master` / `t_pm_sheet_detail` /
`t_pm_process_yh`。采购退货单就是 `t_pm_sheet_master` 里 `trans_no='RO'` 的行 ——
所以要退货单用 `segments: ["purchase_return"]`,**不要**为它单开一个 model
(那会让同一张表的维度定义维护两份,必然分叉)。

**金额符号在两侧相反,这是审批对账最容易踩的坑**(实测 2026-10-10):

| | 采购侧 `t_pm_sheet_master` 的 RO | 结算侧 `t_fm_recpay_detail` 的 RO |
|---|---|---|
| ysx | **+175.2 万**(3565 张) | **-171.7 万**(3474 行) |
| hbposv7 | **+130.5 万**(2081 张) | **-174.4 万**(2668 行) |

也就是说,采购侧的退货金额是**正数**。直接用 `total_amount_yuan`(`SUM(sheet_amt)`)
聚合而不带 segment,结果是"进货 **+** 退货",**实测多算 350 万(ysx)/
261 万(hbposv7)** —— 数字偏大、且没有任何报错。

对账请用 `purchase_sheet.total_net_amount_yuan`
(`SUM(CASE WHEN document_type='RO' THEN -amount ELSE amount END)`),
它已把 RO 取负,与 `settlement_line.total_goods_yuan`(本身就是净额)同口径:

```jsonc
// 进货额(正)
{"model": "purchase_sheet", "measures": ["total_net_amount_yuan"],
 "segments": ["purchase_receipt"]}
// 退货额(负)—— 与结算侧同号,可直接相加
{"model": "purchase_sheet", "measures": ["total_net_amount_yuan"],
 "segments": ["purchase_return"]}
// 全口径净额:进货减退货
{"model": "purchase_sheet", "measures": ["total_net_amount_yuan"]}
```

⚠️ `purchase_sheet_line` **没有 `trans_no` 列**(实测两库只有 `sheet_no`),
所以退货**明细**不能用 segment 直接过滤,必须 join `purchase_sheet`;
净额一律从 `purchase_sheet` 侧聚合 —— 明细表刻意没有造一个"净额"measure,
因为没有 `trans_no` 就分不出 RO,硬造只会给出错误数字。

### segment 绑定到 model,不能跨 model 用

`"segments": ["purchase_return"]` 只在 **base model 自己声明了该 segment** 时有效。
想用别的 model 的 segment,编译期直接报 `segment not found: purchase_return
(available: )` —— 这一点是 `pkg/cubequery` 的显式失败,不是静默忽略。

所以明细侧要按单据类型筛,正确写法是 **join + filter**,而不是 segment:

```jsonc
{
  "model": "purchase_sheet_line",
  "measures": ["purchase_sheet_line.total_amount_yuan"],
  "dimensions": ["purchase_sheet.document_type"],
  "filters": [{"member": "purchase_sheet.document_type",
               "operator": "equals", "values": ["RO"]}]
}
```

> 顺带一句:即便明细侧能筛出 RO,它的金额**也仍是正数**(实测与头表一致:
> ysx +175.2 万 / hbposv7 +130.5 万),所以净额永远要从头表那侧算。
