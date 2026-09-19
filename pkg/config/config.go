// Package config 提供多源配置加载(env / yaml / secrets)。
//
// 设计要点:
//   - 优先级:flags > env > yaml > 默认值
//   - 密钥统一通过 dapr secrets 拿,不要写死
//   - 每个 dapr app 启动时调一次 Load(),然后冻结配置
package config

import "os"

// Source 表示配置的来源。
type Source int

const (
	SourceYAML Source = iota
	SourceEnv
	SourceSecret
)

// Loader 是配置加载器接口。
type Loader interface {
	// String 取字符串配置。
	String(key string) string
	// Int 取整数配置。
	Int(key string) int
	// Bool 取布尔配置。
	Bool(key string) bool
	// Get 取原始字节(由调用方反序列化)。
	Get(key string) ([]byte, bool)
}

// DefaultLoader 是默认实现,先查 env 再查 yaml。
type DefaultLoader struct {
	yaml map[string]any
}

// NewDefaultLoader 构造一个默认加载器。
func NewDefaultLoader(yamlPath string) (*DefaultLoader, error) {
	// TODO: 解析 yaml 文件
	_ = yamlPath
	return &DefaultLoader{yaml: map[string]any{}}, nil
}

// String 查 env,再查 yaml。
func (l *DefaultLoader) String(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	if v, ok := l.yaml[key].(string); ok {
		return v
	}
	return ""
}

// Int 查 env,再查 yaml。
func (l *DefaultLoader) Int(key string) int {
	if v, ok := os.LookupEnv(key); ok {
		// TODO: parse int
		_ = v
	}
	if v, ok := l.yaml[key].(int); ok {
		return v
	}
	return 0
}

// Bool 查 env,再查 yaml。
func (l *DefaultLoader) Bool(key string) bool {
	if v, ok := os.LookupEnv(key); ok {
		return v == "true" || v == "1"
	}
	if v, ok := l.yaml[key].(bool); ok {
		return v
	}
	return false
}

// Get 返回 yaml 原始值。
func (l *DefaultLoader) Get(key string) ([]byte, bool) {
	v, ok := l.yaml[key]
	if !ok {
		return nil, false
	}
	// TODO: 序列化
	_ = v
	return nil, false
}