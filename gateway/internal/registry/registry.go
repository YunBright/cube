// Package registry 是 cube-gateway 的注册表。
//
// 设计要点:
//   - dapr cube app 启动时调 /register 注册
//   - 注册信息写 in-memory map,同时持久化到 dapr state store
//   - 每个 AppInfo 带 LastSeen;handler 在 invoke 成功后调 MarkSeen 刷新
//   - gateway 用 IsOnline(LastSeen, staleAfter) 判断 source 是否"在线"
//   - cube-compiler 重启 app 后通过 pubsub 发"reload"事件,gateway 收到后重新拉
//
// v2 改动:
//   - 删除 byModel(多实例共享 model 时 first-write-wins 会丢数据)
//   - AppInfo 加 LastSeen / Online 辅助
//   - Register 校验 AppID 格式 `^[\w-]+-[\w-]+-[\w-]+$`
package registry

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	"github.com/YunBright/cube/pkg/daprclient"
	"github.com/YunBright/cube/pkg/log"
)

// sourceFormatRE 是 AppID / source 的格式约束:3 段以 `-` 分隔的 [\w-] 段。
//
// 例:`sixun-ysx-00`、`sixun-hbposv7-jiale`、`liangyou-v2-foo`。
// 不允许 `bad`(1 段)、`sixun-ysx`(2 段)、`a-b-`(尾段空)。
//
// 校验在 gateway Register / MarkSeen 两处发生。
var sourceFormatRE = regexp.MustCompile(`^[\w-]+-[\w-]+-[\w-]+$`)

// DefaultStaleAfter 是判定 source "在线"的最大 LastSeen 时间。
//
// 90s 选自:小规模 dev dapr 进程通常 30s 内能 register,生产侧加
// 健康检查间隔 60s,90s 留 30s 余量。
const DefaultStaleAfter = 90 * time.Second

// ErrSourceFormatInvalid 表示 AppID 不符合 3 段格式。
var ErrSourceFormatInvalid = sourceFormatError{}

type sourceFormatError struct{}

func (sourceFormatError) Error() string {
	return "registry: source format must be 3 hyphen-delimited segments ([\\w-]+)"
}

// SourceFormatValid 校验 s 是否符合 source 格式。供 handler / main 直接调用。
func SourceFormatValid(s string) bool { return sourceFormatRE.MatchString(s) }

// AppInfo 是 dapr cube app 启动注册时上报的元信息。
type AppInfo struct {
	AppID        string    `json:"app_id"`
	Family       string    `json:"family"`
	Version      string    `json:"version"`
	Source       string    `json:"source"`        // == AppID;冗余字段,方便 /v1/sources 直接返回
	Models       []string  `json:"models"`
	Capabilities []string  `json:"capabilities"`
	HealthURL    string    `json:"health_url,omitempty"`
	RegisteredAt string    `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"` // 最近一次成功 invoke 或 register 时间
}

// IsOnline 判断本 source 是否在 staleAfter 时间内"在线"。
//
// staleAfter = 0 时视为常在线(用于测试)。
func (a *AppInfo) IsOnline(staleAfter time.Duration) bool {
	if a == nil {
		return false
	}
	if staleAfter <= 0 {
		return true
	}
	return time.Since(a.LastSeen) <= staleAfter
}

// Status 字符串化返回 "online" / "offline"。
func (a *AppInfo) Status(staleAfter time.Duration) string {
	if a.IsOnline(staleAfter) {
		return "online"
	}
	return "offline"
}

// Registry 是注册表。
type Registry struct {
	mu     sync.RWMutex
	byID   map[string]*AppInfo
	dapr   daprclient.Client
	store  string
	logger *log.Logger

	// staleAfter 决定 IsOnline 判定;默认 90s。
	staleAfter time.Duration
}

// New 构造注册表。
func New(dapr daprclient.Client, store string, lg *log.Logger) *Registry {
	return &Registry{
		byID:       map[string]*AppInfo{},
		dapr:       dapr,
		store:      store,
		logger:     lg,
		staleAfter: DefaultStaleAfter,
	}
}

// SetStaleAfter 设置 IsOnline 判定窗口(测试用)。
func (r *Registry) SetStaleAfter(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staleAfter = d
}

// StaleAfter 返回当前 IsOnline 判定窗口。
func (r *Registry) StaleAfter() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.staleAfter
}

// Register 注册一个 cube app。
//
// 校验 AppID 格式;记录 LastSeen = now;持久化到 dapr state store(若有 store)。
// 同 AppID 重复注册 → 刷新 LastSeen + 覆盖字段。
func (r *Registry) Register(ctx context.Context, info *AppInfo) error {
	if info == nil {
		return sourceFormatError{}
	}
	if !SourceFormatValid(info.AppID) {
		return ErrSourceFormatInvalid
	}
	info.LastSeen = time.Now()
	if info.Source == "" {
		info.Source = info.AppID
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.byID[info.AppID] = info

	// 持久化到 dapr state store(给其他 gateway 实例同步)。
	// 失败仅记日志 —— 不阻塞本实例的 in-memory 注册。
	if r.dapr != nil && r.store != "" {
		key := "registry:" + info.AppID
		payload, err := json.Marshal(info)
		if err != nil {
			r.logger.Info("registry marshal failed", "app_id", info.AppID, "err", err.Error())
			return nil
		}
		if err := r.dapr.SaveState(ctx, r.store, key, payload); err != nil {
			r.logger.Info("registry persist failed", "app_id", info.AppID, "err", err.Error())
		}
	}
	return nil
}

// MarkSeen 刷新 source 的 LastSeen(成功 invoke 之后调用)。
//
// 未知 source → 静默 no-op(handler 不应在未注册的 source 上调 invoke,
// 此处不报错以保持 forward 兼容)。
func (r *Registry) MarkSeen(source string) {
	if !SourceFormatValid(source) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if info, ok := r.byID[source]; ok {
		info.LastSeen = time.Now()
	}
}

// LookupByID 按 source(= AppID)查。
func (r *Registry) LookupByID(source string) (*AppInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.byID[source]
	return info, ok
}

// All 返回所有注册信息(给 /v1/sources 用)。
func (r *Registry) All() []*AppInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*AppInfo, 0, len(r.byID))
	for _, v := range r.byID {
		out = append(out, v)
	}
	return out
}

// Online 仅返回 IsOnline=true 的 entries。
func (r *Registry) Online() []*AppInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*AppInfo, 0, len(r.byID))
	for _, v := range r.byID {
		if v.IsOnline(r.staleAfter) {
			out = append(out, v)
		}
	}
	return out
}

// Reload 删除一个 app 的注册(cube-compiler 重启时通过 pubsub 通知)。
//
// 删除 byID;持久化层由 pubsub handler 负责重新写回。
func (r *Registry) Reload(ctx context.Context, appID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, appID)
}
