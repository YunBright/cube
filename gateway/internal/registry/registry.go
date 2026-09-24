// Package registry 是 cube-gateway 的注册表。
//
// 设计要点:
//   - dapr cube app 启动时调 /register 注册
//   - 注册信息写 in-memory map,同时持久化到 dapr state store
//   - 每个 AppInfo 带 LastSeen —— handler 在 invoke 成功后调 MarkSeen 刷新,
//     仅供运维排查;不用于"在线判定"(见下)
//   - cube-compiler 重启 app 后通过 pubsub 发"reload"事件,gateway 收到后重新拉
//
// 被动验证模式(取代旧 90s 时间窗口判定):
//   - handler 不再前置 IsOnline 检查;任何已注册 source 都直接调 dapr
//   - 真实 SOURCE_OFFLINE 由 dapr 真实失败(ErrConnFailure / 找不到 sidecar)触发
//   - /v1/sources 视图的 status 字段总是 "online"(信息性,不代表实际可达)
//   - LastSeen 仅作为"上次成功 invoke 时间"暴露给运维,不做离线判定
//
// v2 改动:
//   - 删除 byModel(多实例共享 model 时 first-write-wins 会丢数据)
//   - AppInfo 加 LastSeen / Online 辅助
//   - Register 校验 AppID 格式 `^[\w-]+-[\w-]+-[\w-]+$`
//
// v2 二次解耦(plan B):
//   - AppInfo 增 DaprAppID 字段 — gateway 调 dapr 时按它寻址,
//     不再把 source 直接当 app-id 用。
//   - 校验 dapr_app_id 字符集 ^[a-z0-9-]+$ — 与 dapr sidecar 约定一致,
//     旧 cube app 不上报 dapr_app_id 时 fallback 到 AppID(向后兼容)。
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

// daprAppIDRE 是 dapr app-id 的字符集约束(与 boot.appIDRE 同步)。
//
// dapr 实际不接受下划线 / 数字开头;这里用 [a-z0-9][a-z0-9-]*[a-z0-9] 与单字符兜底,
// 保证 `cube-sixun-hbposv7-jiale` 这种带前缀的长 id 合法。
var daprAppIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

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

// ErrDaprAppIDInvalid 表示 dapr_app_id 字段缺失或字符集非法。
//
// 正常 cube app 总是报 dapr_app_id(从 DAPR_APP_ID 自动读);
// 旧版 cube app 不报时 gateway 自动 fallback 到 AppID,不视为错误。
type ErrDaprAppIDInvalid struct{ Value string }

func (e *ErrDaprAppIDInvalid) Error() string {
	return "registry: dapr_app_id invalid (must match ^[a-z0-9][a-z0-9-]*[a-z0-9]$): " + e.Value
}

// SourceFormatValid 校验 s 是否符合 source 格式。供 handler / main 直接调用。
func SourceFormatValid(s string) bool { return sourceFormatRE.MatchString(s) }

// DaprAppIDValid 校验 s 是否符合 dapr app-id 字符集。
func DaprAppIDValid(s string) bool { return daprAppIDRE.MatchString(s) }

// AppInfo 是 dapr cube app 启动注册时上报的元信息。
type AppInfo struct {
	// AppID = source = wire id(URL /v1/source/{AppID}/load 段)。
	AppID string `json:"app_id"`
	// DaprAppID 是 dapr sidecar 的 app-id — gateway 调 dapr 时按它寻址。
	// 默认 = AppID(1:1 兼容);cube app 一般报 "cube-" + AppID。
	DaprAppID string `json:"dapr_app_id"`
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
//
// **已被弃用**:被动验证模式下不再用于 /v1/sources 视图或 handler 前置检查。
// 保留只是为了兼容旧测试和外部诊断代码 —— handler 现在不再调用此方法,
// /v1/sources 视图始终报告 "online",由实际 invoke 失败决定 SOURCE_OFFLINE。
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
//
// **已被弃用**:理由同 IsOnline。handler 现在 hard-code "online"。
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
// 校验 AppID 格式 + dapr_app_id(若提供);记录 LastSeen = now;
// 持久化到 dapr state store(若有 store)。
// 同 AppID 重复注册 → 刷新 LastSeen + 覆盖字段。
//
// dapr_app_id 缺失或空时:fallback 到 AppID(向后兼容旧 cube app)。
// dapr_app_id 非空但字符集不合法:返 ErrDaprAppIDInvalid(handler 映射为 400
// + DAPR_APP_ID_INVALID — 见 plan B §2)。
func (r *Registry) Register(ctx context.Context, info *AppInfo) error {
	if info == nil {
		return sourceFormatError{}
	}
	if !SourceFormatValid(info.AppID) {
		return ErrSourceFormatInvalid
	}
	if info.DaprAppID == "" {
		info.DaprAppID = info.AppID // 兼容旧 cube app 不报 dapr_app_id
	} else if !DaprAppIDValid(info.DaprAppID) {
		return &ErrDaprAppIDInvalid{Value: info.DaprAppID}
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
// 在被动验证模式下,MarkSeen 不再用于"在线判定"(handler 不再前置 IsOnline 检查),
// **仅作为运维排查信号**:LastSeen == 成功 invoke 时间。
// 真实 SOURCE_OFFLINE 由 dapr 真实调用失败的 ErrConnFailure 触发。
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

// Unregister 显式删除一个 source 的注册(cube app 优雅关闭时调用)。
//
// 行为:
//   - byID 删除
//   - state store key 删除(失败仅记日志,不阻塞)
//   - 返回 ok=true 表示删除了一条;ok=false 表示原本就没注册(幂等)
//
// 幂等性:这是 POST /unregister 行为正确性的核心 —— cube app 在 SIGTERM/SIGINT
// 路径上调用 unregister,期间 gateway 可能已经重启 / 自己挂了 / 别的 cube app
// 替它 register 过;Unregister 不应因此报错,让 cube app 正常退出。
//
// 调用方约定:handler 层对任何 appID 都允许(即使是无效 / 未注册的),
// 因为 unregister 失败只是优化(让 registry 立即反映"已离开"),不是数据丢失;
// 真正的事实是 dapr sidecar 后续连不上 → 调用走 SOURCE_OFFLINE 路径。
func (r *Registry) Unregister(ctx context.Context, appID string) (ok bool) {
	if appID == "" {
		return false
	}
	r.mu.Lock()
	_, present := r.byID[appID]
	delete(r.byID, appID)
	r.mu.Unlock()

	if !present {
		// 不在内存里 —— 也尝试删 state store(可能别的 gateway 实例有)
		if r.dapr != nil && r.store != "" {
			key := "registry:" + appID
			if err := r.dapr.DeleteState(ctx, r.store, key); err != nil {
				r.logger.Info("unregister state-delete failed", "app_id", appID, "err", err.Error())
			}
		}
		return false
	}

	// 删除持久化层 —— 失败仅记日志,不阻塞
	if r.dapr != nil && r.store != "" {
		key := "registry:" + appID
		if err := r.dapr.DeleteState(ctx, r.store, key); err != nil {
			r.logger.Info("unregister state-delete failed", "app_id", appID, "err", err.Error())
		}
	}
	r.logger.Info("source unregistered", "app_id", appID)
	return true
}
