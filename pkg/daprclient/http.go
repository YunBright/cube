// Package daprclient: HTTP implementation of the dapr Client interface.
//
// HTTP client calls the local dapr-sidecar via the standard service-invocation
// API:
//
//	POST http://<sidecar-host>:<DAPR_HTTP_PORT>/v1.0/invoke/<app-id>/method/<method>
//	Headers: Content-Type: application/json, dapr-* metadata as HTTP headers
//	Body: raw JSON bytes
//
// This is the production path for cube-gateway (and any cube app) when running
// under dapr run with --app-protocol http. The "noop" stub is only for tests
// and pre-dapr development.

package daprclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPClient 是基于 HTTP 的 dapr 客户端,直接调本机 dapr-sidecar。
//
// 配置项:
//   - SidecarAddr:"http://127.0.0.1:3500"(或 3000 / 3003 / ... 按 dapr-http-port)
//   - Timeout:    10s 默认(跟 stocktake cubeclient 保持一致)
type HTTPClient struct {
	SidecarAddr string        // 例 "http://127.0.0.1:3000"
	HTTPClient  *http.Client  // 可注入(测试用),默认 10s timeout
}

// NewHTTP 构造 HTTP dapr 客户端。
//
// sidecarAddr 形如 "http://127.0.0.1:3000" —— dapr run 启动的 sidecar HTTP 端口。
func NewHTTP(sidecarAddr string) *HTTPClient {
	return &HTTPClient{
		SidecarAddr: sidecarAddr,
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// SetHTTPClient 注入自定义 http.Client(测试用)。
func (c *HTTPClient) SetHTTPClient(h *http.Client) { c.HTTPClient = h }

// InvokeMethod 经 dapr-sidecar 调另一个 dapr app 的 method。
//
// URL: <sidecar>/v1.0/invoke/<targetAppID>/method/<method>
// extra 中的 key 自动转 dapr-<key> 头(透传到目标 sidecar)。
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
		return nil, fmt.Errorf("daprclient http: invoke %s/%s: %w", targetAppID, method, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("daprclient http: invoke %s/%s: status=%d body=%s",
			targetAppID, method, resp.StatusCode, string(body))
	}
	return body, nil
}

// PublishToPubSub HTTP 实现:POST <sidecar>/v1.0/publish/<pubsub>/<topic>。
func (c *HTTPClient) PublishToPubSub(ctx context.Context, pubsub, topic string, data []byte) error {
	url := fmt.Sprintf("%s/v1.0/publish/%s/%s", c.SidecarAddr, pubsub, topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("daprclient http: new publish request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("daprclient http: publish %s/%s: %w", pubsub, topic, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daprclient http: publish %s/%s: status=%d body=%s",
			pubsub, topic, resp.StatusCode, string(body))
	}
	return nil
}

// GetState HTTP 实现:GET <sidecar>/v1.0/state/<store>/<key>。
//
// 返 (raw value bytes, exists, error) —— 不存在 = (nil, false, nil)。
func (c *HTTPClient) GetState(ctx context.Context, store, key string) ([]byte, bool, error) {
	url := fmt.Sprintf("%s/v1.0/state/%s/%s", c.SidecarAddr, store, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("daprclient http: new get-state request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("daprclient http: get state %s/%s: %w", store, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, false, fmt.Errorf("daprclient http: get state %s/%s: status=%d body=%s",
			store, key, resp.StatusCode, string(body))
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
		return fmt.Errorf("daprclient http: save state %s/%s: %w", store, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daprclient http: save state %s/%s: status=%d body=%s",
			store, key, resp.StatusCode, string(rb))
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