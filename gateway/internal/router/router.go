// Package router 负责 model → dapr app_id 的路由决策。
//
// 优先级:
//   1. 查 registry(动态注册的 app)
//   2. fallback 到 config.yaml 的 routing 段(dev 用)
package router

import (
	"github.com/YunBright/cube/gateway/internal/registry"
)

// StaticRoute 是 config.yaml 的静态路由条目。
type StaticRoute struct {
	Model string `yaml:"model"`
	AppID string `yaml:"app_id"`
}

// Router 决策路由。
type Router struct {
	reg     *registry.Registry
	static  []StaticRoute
}

// New 构造 Router。
func New(reg *registry.Registry, static []StaticRoute) *Router {
	return &Router{reg: reg, static: static}
}

// Route 返回 model 对应的 app_id。
func (r *Router) Route(model string) (string, bool) {
	// 1. 优先查注册表
	if id, ok := r.reg.LookupByModel(model); ok {
		return id, true
	}
	// 2. fallback 静态
	for _, s := range r.static {
		if s.Model == model {
			return s.AppID, true
		}
	}
	return "", false
}