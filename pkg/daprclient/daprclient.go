// Package daprclient 定义 Dapr 调用统一抽象。
//
// 设计要点:
//   - pkg 不依赖具体 SDK 版本,只暴露 Client interface
//   - 各 app 启动时注入具体实现(dapr-go-sdk / dapr-grpc)
//   - invocation metadata 透传 principal / tenant(x-principal / x-tenant)
//   - 不要各 app 直接 import dapr SDK
package daprclient

import (
	"context"
	"errors"
)

// MetadataKey principal / tenant metadata key(透传到下游 app)。
const (
	MetadataKeyPrincipal = "x-principal"
	MetadataKeyTenant    = "x-tenant"
	MetadataKeyTraceID   = "x-trace-id"
)

// Client 是 Dapr 调用抽象。
//
// 真实实现由各 app 注入(grpc 实现 or http 实现 or mock),脚手架阶段留 Noop。
type Client interface {
	// InvokeMethod 调另一个 dapr app 的方法,自动带上 principal metadata。
	InvokeMethod(ctx context.Context, targetAppID, method string, data []byte, extra map[string]string) ([]byte, error)
	// PublishToPubSub 发布到 pubsub。
	PublishToPubSub(ctx context.Context, pubsub, topic string, data []byte) error
	// GetState 从 state store 取值。
	GetState(ctx context.Context, store, key string) ([]byte, bool, error)
	// SaveState 存到 state store。
	SaveState(ctx context.Context, store, key string, value []byte) error
	// DeleteState 从 state store 删 key。
	DeleteState(ctx context.Context, store, key string) error
	// Close 关闭底层连接。
	Close() error
}

// Noop 是默认空实现,用于本地 dev / 单测。
type Noop struct{}

// NewNoop 构造 Noop client。
func NewNoop() *Noop { return &Noop{} }

// InvokeMethod Noop 实现:返回未实现错误。
func (Noop) InvokeMethod(ctx context.Context, targetAppID, method string, data []byte, extra map[string]string) ([]byte, error) {
	return nil, errors.New("daprclient: noop invoke " + targetAppID + "/" + method)
}

// PublishToPubSub Noop 实现。
func (Noop) PublishToPubSub(ctx context.Context, pubsub, topic string, data []byte) error {
	return errors.New("daprclient: noop publish")
}

// GetState Noop 实现。
func (Noop) GetState(ctx context.Context, store, key string) ([]byte, bool, error) {
	return nil, false, nil
}

// SaveState Noop 实现。
func (Noop) SaveState(ctx context.Context, store, key string, value []byte) error {
	return nil
}

// DeleteState Noop 实现。
func (Noop) DeleteState(ctx context.Context, store, key string) error {
	return nil
}

// Close Noop 实现。
func (Noop) Close() error { return nil }

// ---- ctx 透传 ----

type ctxKeyPrincipal struct{}
type ctxKeyTenant struct{}

// WithPrincipal 把 principal 写入 ctx。
func WithPrincipal(ctx context.Context, p string) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal{}, p)
}

// PrincipalFromCtx 从 ctx 取 principal。
func PrincipalFromCtx(ctx context.Context) string {
	if p, ok := ctx.Value(ctxKeyPrincipal{}).(string); ok {
		return p
	}
	return ""
}

// WithTenant 把 tenant 写入 ctx。
func WithTenant(ctx context.Context, t string) context.Context {
	return context.WithValue(ctx, ctxKeyTenant{}, t)
}

// TenantFromCtx 从 ctx 取 tenant。
func TenantFromCtx(ctx context.Context) string {
	if t, ok := ctx.Value(ctxKeyTenant{}).(string); ok {
		return t
	}
	return ""
}

// PrincipalFromMetadata 从 invocation metadata 取 principal(由 middleware 提取)。
func PrincipalFromMetadata(md map[string]string) string {
	return md[MetadataKeyPrincipal]
}

// TenantFromMetadata 从 invocation metadata 取 tenant。
func TenantFromMetadata(md map[string]string) string {
	return md[MetadataKeyTenant]
}