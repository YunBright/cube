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

| 二进制 | CUBE_APP_ID | family | version | instance |
|---|---|---|---|---|
| `sixun-ysx`     | `sixun-ysx-00`        | sixun | ysx     | 00 |
| `sixun-ysx`     | `sixun-ysx-baiyuan1`  | sixun | ysx     | baiyuan1 |
| `sixun-hbposv7` | `sixun-hbposv7-jiale` | sixun | hbposv7 | jiale |

family / version / instance 由 `CUBE_APP_ID` 拆分得到,**不设独立 env**。
DSN 与表名仍走 `./config.yaml`(每店可能不同)。

## mapping.yaml 语义(P1-5 选 A)

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
