// Package daprclient: HTTP implementation of the dapr Client interface.
//
// HTTP client calls the local dapr-sidecar via the standard service-invocation
// API:
//
//	POST http://<sidecar-host>:<DAPR_HTTP_PORT>/v1.0/invoke/<app-id>/method/<method>
//	Headers: Content-Type: application/json, dapr-* metadata as HTTP headers
//	Body: raw JSON bytes
//
// 错误模型:InvokeMethod / PublishToPubSub / GetState / SaveState 都返回 *InvokeError
// (errors.go) 区分 ErrTimeout / ErrConnFailure / ErrHTTPStatus 三种 Kind。调用方用
// errors.As 拿到 Kind + StatusCode + Body 后做最终映射。
//
// 超时策略:本包不设默认 http.Client.Timeout —— 0 表示无 client 级 deadline,由调用方
// 通过 context.WithTimeout 自行控制。
package daprclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// HTTPClient 是基于 HTTP 的 dapr 客户端,直接调本机 dapr-sidecar。
//
// 配置项:
//   - SidecarAddr:"http://127.0.0.1:3500"(或 3000 / 3003 / ... 按 dapr-http-port)
//   - HTTPClient: 可注入(测试用),默认 Timeout=0(由 ctx 控制)
type HTTPClient struct {
	SidecarAddr string       // 例 "http://127.0.0.1:3000"
	HTTPClient  *http.Client // 可注入(测试用),默认 Timeout=0
}

// NewHTTP 构造 HTTP dapr 客户端。
//
// sidecarAddr 形如 "http://127.0.0.1:3000" —— dapr run 启动的 sidecar HTTP 端口。
// HTTPClient 默认 Timeout=0,所有 deadline 由调用方通过 ctx.WithTimeout 控制。
func NewHTTP(sidecarAddr string) *HTTPClient {
	return &HTTPClient{
		SidecarAddr: sidecarAddr,
		HTTPClient:  &http.Client{}, // Timeout=0
	}
}

// SetHTTPClient 注入自定义 http.Client(测试用)。
func (c *HTTPClient) SetHTTPClient(h *http.Client) { c.HTTPClient = h }

// InvokeMethod 经 dapr-sidecar 调另一个 dapr app 的 method。
//
// URL: <sidecar>/v1.0/invoke/<targetAppID>/method/<method>
// extra 中的 key 自动转 dapr-<key> 头(透传到目标 sidecar)。
//
// 错误分类:
//   - ctx deadline exceeded / http.Client 超时 → *InvokeError{Kind: ErrTimeout}
//   - 其他 transport 错误(refused / DNS / EOF)→ *InvokeError{Kind: ErrConnFailure}
//   - 上游 4xx / 5xx → *InvokeError{Kind: ErrHTTPStatus, StatusCode, Body}
//   - SidecarAddr 未配置 → errors.New (string error,不带 Kind 信息)
func (c *HTTPClient) InvokeMethod(ctx context.Context, targetAppID, method string, data []byte, extra map[string]string) ([]byte, error) {
	if c.SidecarAddr == "" {
		return nil, errors.New("daprclient http: SidecarAddr 未配置")
	}
	url := fmt.Sprintf("%s/v1.0/invoke/%s/method/%s", c.SidecarAddr, targetAppID, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("daprclient http: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, classifyTransportErr(targetAppID, method, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &InvokeError{
			TargetAppID: targetAppID,
			Method:      method,
			Kind:        ErrHTTPStatus,
			StatusCode:  resp.StatusCode,
			Body:        body,
		}
	}
	return body, nil
}

// PublishToPubSub HTTP 实现:POST <sidecar>/v1.0/publish/<pubsub>/<topic>。
//
// 错误分类与 InvokeMethod 一致。
func (c *HTTPClient) PublishToPubSub(ctx context.Context, pubsub, topic string, data []byte) error {
	if c.SidecarAddr == "" {
		return errors.New("daprclient http: SidecarAddr 未配置")
	}
	url := fmt.Sprintf("%s/v1.0/publish/%s/%s", c.SidecarAddr, pubsub, topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("daprclient http: new publish request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return classifyTransportErr("pubsub:"+pubsub, topic, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return &InvokeError{
			TargetAppID: pubsub,
			Method:      topic,
			Kind:        ErrHTTPStatus,
			StatusCode:  resp.StatusCode,
			Body:        body,
		}
	}
	return nil
}

// GetState HTTP 实现:GET <sidecar>/v1.0/state/<store>/<key>。
//
// 返 (raw value bytes, exists, error) —— 不存在 = (nil, false, nil)。
//
// 注:本路径不是 critical path,保留与旧行为兼容 —— 404 转 (nil, false, nil),
// 不上抛 InvokeError。其他非 2xx 仍返回 *InvokeError{ErrHTTPStatus}。
func (c *HTTPClient) GetState(ctx context.Context, store, key string) ([]byte, bool, error) {
	if c.SidecarAddr == "" {
		return nil, false, errors.New("daprclient http: SidecarAddr 未配置")
	}
	url := fmt.Sprintf("%s/v1.0/state/%s/%s", c.SidecarAddr, store, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("daprclient http: new get-state request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, false, classifyTransportErr("state:"+store, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, false, &InvokeError{
			TargetAppID: store,
			Method:      key,
			Kind:        ErrHTTPStatus,
			StatusCode:  resp.StatusCode,
			Body:        body,
		}
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		return nil, false, nil
	}
	return body, true, nil
}

// SaveState HTTP 实现:POST <sidecar>/v1.0/state/<store>。
//
// value 序列化成单个 state item [{ "key": "...", "value": <any> }]。
func (c *HTTPClient) SaveState(ctx context.Context, store, key string, value []byte) error {
	if c.SidecarAddr == "" {
		return errors.New("daprclient http: SidecarAddr 未配置")
	}
	url := fmt.Sprintf("%s/v1.0/state/%s", c.SidecarAddr, store)
	// value 可能是 JSON 字符串或裸字节。把它包成对象 ——
	// dapr state API 要求数组元素是 {"key": ..., "value": <任意 JSON>}
	// 如果 value 本身已经是 JSON object 就不再 wrap。
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		v = string(value)
	}
	items := []map[string]any{
		{"key": key, "value": v},
	}
	body, _ := json.Marshal(items)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("daprclient http: new save-state request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return classifyTransportErr("state:"+store, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return &InvokeError{
			TargetAppID: store,
			Method:      key,
			Kind:        ErrHTTPStatus,
			StatusCode:  resp.StatusCode,
			Body:        rb,
		}
	}
	return nil
}

// Close 释放 http.Client 闲置连接。no-op for default client。
func (c *HTTPClient) Close() error {
	if c.HTTPClient != nil {
		c.HTTPClient.CloseIdleConnections()
	}
	return nil
}

// DeleteState HTTP 实现:DELETE <sidecar>/v1.0/state/<store>/<key>。
//
// 用于 cube app 优雅关闭时 unregister —— 不强制要求 key 存在
// (dapr DELETE 对不存在的 key 返 204,行为幂等)。
func (c *HTTPClient) DeleteState(ctx context.Context, store, key string) error {
	if c.SidecarAddr == "" {
		return errors.New("daprclient http: SidecarAddr 未配置")
	}
	url := fmt.Sprintf("%s/v1.0/state/%s/%s", c.SidecarAddr, store, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("daprclient http: new delete-state request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return classifyTransportErr("state:"+store, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return &InvokeError{
			TargetAppID: store,
			Method:      key,
			Kind:        ErrHTTPStatus,
			StatusCode:  resp.StatusCode,
			Body:        rb,
		}
	}
	return nil
}
