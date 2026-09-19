# 语义层设计

## 家族 vs 实例 vs Model

```
family (思迅 / 粮油 / 生鲜)
├── sixun-models/           共享 schema(Go module)
│   ├── supplier/
│   │   ├── schema.yaml     cube 风格 schema
│   │   └── preagg.go       DuckDB 预聚合
│   ├── product/
│   └── order/
│
└── semantic-layers/sixun/  实例(每版本一个)
    ├── cmd/
    │   ├── sixun-hbposv7/  ← 思迅 7pro 实例
    │   │   ├── main.go
    │   │   ├── mapping.yaml  思迅7pro 字段映射
    │   │   └── config.yaml
    │   └── sixun-ysx/      ← 思迅云商x 实例
    └── internal/source/
        ├── hbposv7/         思迅7pro connector
        └── ysx/             思迅云商x connector
```

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

每个实例独立 .duckdb 文件(挂载 PV 持久化),例:

```
data/
├── sixun-hbposv7.duckdb    ← 思迅7pro 数据
├── sixun-ysx.duckdb        ← 思迅云商x 数据
└── liangyou-v1.duckdb      ← 粮油(未来)
```

预聚合表名见 `sixun-models/<model>/preagg.go` 的 `PreAggName` 常量,与 schema.yaml 的 `sql_table` 对齐。