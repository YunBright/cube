// Package l1cache 是 cube-gateway 的 L1 缓存(P1-7 选 A:命中直接返回)。
//
// 设计要点:
//   - 进程内 sync.Map(简单;多 gateway 实例时换 dapr state store)
//   - key 由 pkg/cache.KeyBuilder 生成(含 principal/tenant/freshness)
//   - TTL=0 表示永不过期(由 pubsub 主动失效)
package l1cache

import (
	"sync"
	"time"
)

// Cache 是 L1 缓存。
type Cache struct {
	mu       sync.RWMutex
	entries  map[string]*entry
	defaultTTL time.Duration
}

type entry struct {
	value     []byte
	expiresAt time.Time
}

// New 构造 L1 缓存。
func New(ttlSeconds int) *Cache {
	ttl := time.Duration(ttlSeconds) * time.Second
	if ttl == 0 {
		ttl = 60 * time.Second // 默认 60s
	}
	return &Cache{
		entries:    map[string]*entry{},
		defaultTTL: ttl,
	}
}

// Get 取值。
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

// Set 存值。ttl=0 用 defaultTTL。
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	if ttl == 0 {
		ttl = c.defaultTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &entry{
		value:     value,
		expiresAt: time.Now().Add(ttl),
	}
}

// Delete 显式失效。
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Stats 返回条目数(调试用)。
func (c *Cache) Stats() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}