// Package models 是思迅家族共享模型(语义层)的**资产入口**。
//
// 这里只做一件事:把 `<model>/schema.yaml` 用 go:embed 编译进二进制。
//
// 为什么不运行时读盘(历史做法,已废弃):
// 原来是 boot.ResolveModelsDir() 用 os.Stat 探测 CUBE_MODELS_DIR / 相对路径,
// 部署时必须额外把 sixun-models/ 推到远端,并且依赖 systemd 单元里 export
// CUBE_MODELS_DIR 指向对的位置。代价是三个都很容易悄悄错:
//   - 路径没对上 → schema 一个都没加载,但**启动不报错**,查询时才 MODEL_NOT_FOUND
//   - 推了新 schema 但忘了重启 → 用的是旧文件,零症状
//   - CUBE_MODELS_DIR 指向的目录存在但内容过期 → 同样是静默用旧模型
//
// 嵌入之后,"二进制是什么版本" 与 "模型是什么版本" 是同一个事实,不再有对不上的可能。
//
// 仍然留在盘上的只有 config.yaml(含源库 DSN 口令,不能进二进制)。
//
// 命名约定:目录名 = model 名,schema 文件名固定 schema.yaml。
package models

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
)

// schemaFile 是每个 model 目录下的固定 schema 文件名。
const schemaFile = "schema.yaml"

// assets 是编译进二进制的共享 schema 资产。
//
// 模式 `*/schema.yaml` 只匹配一层:product/schema.yaml、settlement/schema.yaml ...
// 不会误抓 preagg.go / go.mod 这类源码(embed 本来也只收非 Go 文件,
// 但这里写窄一点是为了万一将来加子目录时不会被意外带进去)。
//
//go:embed */schema.yaml
var assets embed.FS

// FS 返回 family 级共享 schema 资产,路径形如 `product/schema.yaml`。
//
// 根就是 model 名这一层,调用方直接 `fs.ReadFile(models.FS(), "product/schema.yaml")`,
// 不用关心 sixun-models 在磁盘上的位置 —— 因为它已经不在磁盘上了。
func FS() fs.FS {
	return assets
}

// ModelNames 返回所有带 schema.yaml 的 model 名(已排序)。
//
// 从嵌入资产反推,而不是在调用方硬编码一份名单:硬编码的名单和实际嵌入的文件
// 会分叉(新增 model 忘了改代码 = 该 model 静默不可查),而分叉方向永远是
// "代码比文件旧",排查时表现为"模型明明写了却查不了"。
func ModelNames() ([]string, error) {
	entries, err := assets.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("models: read embedded root: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(assets, path.Join(e.Name(), schemaFile)); err != nil {
			// 有目录但没 schema —— 不是一个 model(例如将来的 assets/ 之类),
			// 跳过而不是报错,免得非 model 目录让整个 app 起不来。
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// ReadSchema 读某个 model 的 schema.yaml 原始字节。
func ReadSchema(model string) ([]byte, error) {
	b, err := assets.ReadFile(path.Join(model, schemaFile))
	if err != nil {
		return nil, fmt.Errorf("models: read schema for %q: %w", model, err)
	}
	return b, nil
}
