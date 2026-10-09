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
