// Package source 定义思迅各版本数据源 connector 的统一接口。
//
// 设计要点:
//   - 每个版本(hbposv7 / ysx)实现 Connector,返回思迅原始数据
//   - 数据经 mapping.yaml 映射后写 DuckDB 预聚合表
//   - 各实例 cmd 启动时通过 build tag / config 选具体 connector
package source

import "context"

// Connector 是数据源连接器接口。
//
// 实现:hbposv7.Connector / ysx.Connector
type Connector interface {
	// FetchSupplier 拉供应商数据。
	FetchSupplier(ctx context.Context) ([]map[string]any, error)
	// FetchProduct 拉商品数据。
	FetchProduct(ctx context.Context) ([]map[string]any, error)
	// FetchCategory 拉商品分类数据。
	FetchCategory(ctx context.Context) ([]map[string]any, error)
	// FetchSaleDetail 拉销售明细(含销售退货)。
	FetchSaleDetail(ctx context.Context) ([]map[string]any, error)
	// FetchStock 拉库存数据。
	FetchStock(ctx context.Context) ([]map[string]any, error)
	// Close 关闭连接。
	Close() error
}

// Config 是 connector 共享配置。
type Config struct {
	// DSN 由 dapr secrets 提供,不要写死
	DSN string `yaml:"dsn"`
	// Version 用于日志
	Version string `yaml:"version"`
}