// Package registry 是 cube-gateway 的注册表。
//
// 设计要点(P0-1):
//   - dapr cube app 启动时调 /register 注册
//   - 注册信息写 in-memory map,同时持久化到 dapr state store
//   - cube-compiler 重启 app 后通过 pubsub 发"reload"事件,gateway 收到后重新拉
package registry

import (
	"context"
	"sync"

	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"
)

// AppInfo 是 dapr cube app 启动注册时上报的元信息。
type AppInfo struct {
	AppID        string   `json:"app_id"`        // 例:sixun-hbposv7
	Family       string   `json:"family"`        // 例:sixun
	Version      string   `json:"version"`       // 例:hbposv7
	Models       []string `json:"models"`        // 例:["supplier","product","order"]
	Capabilities []string `json:"capabilities"`  // 例:["query","preagg","cache_l2"]
	HealthURL    string   `json:"health_url"`    // 例:/health(dapr sidecar 转发)
	RegisteredAt string   `json:"registered_at"` // ISO8601
}

// Registry 是注册表。
type Registry struct {
	mu     sync.RWMutex
	byID   map[string]*AppInfo
	byModel map[string]string // model → app_id(首个注册)
	dapr   daprclient.Client
	store  string
	logger *log.Logger
}

// New 构造注册表。
func New(dapr daprclient.Client, store string, lg *log.Logger) *Registry {
	return &Registry{
		byID:    map[string]*AppInfo{},
		byModel: map[string]string{},
		dapr:    dapr,
		store:   store,
		logger:  lg,
	}
}

// Register 注册一个 cube app。
func (r *Registry) Register(ctx context.Context, info *AppInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.byID[info.AppID] = info
	for _, m := range info.Models {
		// 多个 app 支持同一 model 时,后注册的覆盖前者(由 cube-compiler 决定启动顺序)
		if _, exists := r.byModel[m]; !exists {
			r.byModel[m] = info.AppID
		}
	}

	// 持久化到 dapr state store(给其他 gateway 实例同步)
	if r.dapr != nil && r.store != "" {
		key := "registry:" + info.AppID
		// TODO: json.Marshal(info)
		_ = ctx
		_ = key
	}
	return nil
}

// LookupByID 按 app_id 查。
func (r *Registry) LookupByID(appID string) (*AppInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.byID[appID]
	return info, ok
}

// LookupByModel 按 model 查 app_id。
func (r *Registry) LookupByModel(model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byModel[model]
	return id, ok
}

// All 返回所有注册信息(给 /v1/meta 用)。
func (r *Registry) All() []*AppInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*AppInfo, 0, len(r.byID))
	for _, v := range r.byID {
		out = append(out, v)
	}
	return out
}

// Reload 删除一个 app 的注册(cube-compiler 重启时通过 pubsub 通知)。
func (r *Registry) Reload(ctx context.Context, appID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, appID)
	// byModel 不主动清,等下次 Register 重新填
}