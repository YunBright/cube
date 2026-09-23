package handler

import (
	"regexp"
	"time"
)

// 上游 cube-app 调用超时。10s 是经验值,与 stocktake cubeclient 一致。
//
// handler 在每次 dapr.InvokeMethod 前用 context.WithTimeout 包一层 ctx。
const upstreamCallTimeout = 10 * time.Second

// upstreamBodyTruncate 是把 upstream error body 写入 Details.upstream_body 时
// 的最大字节数(防 OOM / 防超长日志)。
const upstreamBodyTruncate = 512

// sourceFormatRE 是 source URL 段的格式约束(与 registry.SourceFormatValid 保持一致)。
//
// 与 registry 包内的 regex 是同一份 —— 这里重复定义避免 handler 反向依赖 registry 的正则常量。
var sourceFormatRE = regexp.MustCompile(`^[\w-]+-[\w-]+-[\w-]+$`)

// defaultFreshness 是 L1 缓存 key 的 freshness tag。MVP 阶段 schema 不会变化,
// 写死 "v1"。schema 升级时 bump 成 "v2" 让旧 entry 自动失配。
const defaultFreshness = "v1"

// defaultPrincipal / defaultTenant 是没从 Authorization 头解析到时使用的占位。
// 多租户 / 鉴权接入后再换成真实值。
const (
	defaultPrincipal = "anon"
	defaultTenant    = "default"
)

// truncateBytes 把 b 截到 n 字节内;超长时附加 "...(truncated N bytes)"。
func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
