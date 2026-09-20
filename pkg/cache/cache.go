// Package cache 定义 L1/L2 缓存抽象接口。
//
// 设计要点(P1-7):
//   - L1 在 cube-gateway:命中直接返回,**完全跳过 cube app**
//   - L2 在 dapr cube app:per-tenant 缓存,可被 PubSub 失效
//   - key 包含 principal,避免跨用户泄漏
package cache

import (
	"crypto/md5"
	"encoding/hex"
	"time"
)

// Entry 是缓存条目。
type Entry struct {
	Key       string
	Value     []byte
	ExpiresAt time.Time
}

// IsExpired 判断是否过期。
func (e *Entry) IsExpired() bool {
	return time.Now().After(e.ExpiresAt)
}

// Cache 是缓存接口。
//
// Implementations:
//   - in-memory:进程内 map,本地 dev 用
//   - redis:接 dapr state store (component: statestore.yaml)
type Cache interface {
	// Get 取值,不存在或过期返回 false。
	Get(key string) ([]byte, bool)
	// Set 存值,ttl=0 表示永不过期。
	Set(key string, value []byte, ttl time.Duration)
	// Delete 显式失效。
	Delete(key string)
	// DeletePrefix 删除某前缀所有 key(用于模型重建后批量失效)。
	DeletePrefix(prefix string)
}

// KeyBuilder 构造缓存 key。
//
// 公式:md5(model + cube_query_json + principal + tenant + freshness_tag)
//
// 注意:这个 key **必须** 包含 query body,否则不同请求(不同 filters / dims /
// measures)会共享同一个 cache slot,后到的请求被错误地命中前一个请求的结果。
// 历史上 Build() 返 "" (一个 TODO),所有请求共享同一 slot,表现为"任何 query
// 都拿到第一条 cached 结果"。
type KeyBuilder struct{}

// Build 构造 key。
func (k *KeyBuilder) Build(model string, query []byte, principal, tenant, freshness string) string {
	h := md5.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write(query)
	h.Write([]byte{0})
	h.Write([]byte(principal))
	h.Write([]byte{0})
	h.Write([]byte(tenant))
	h.Write([]byte{0})
	h.Write([]byte(freshness))
	return hex.EncodeToString(h.Sum(nil))
}

// TODO: pkg/cache/inmem.go 实现 in-memory cache
// TODO: pkg/cache/redis.go 实现接 dapr state store 的 cache