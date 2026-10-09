// Jobs API 客户端(HTTP)。
//
// # 为什么用 HTTP 而不是 Go SDK
//
// Dapr 官方建议生产用 SDK(gRPC)。本仓库的 daprclient 整个包已经是 HTTP 实现
// (`HTTPClient` + 本机 sidecar),jobs 保持同一风格可以复用同一套错误分类
// 和同一个 sidecar 地址来源,不引入第二套传输层。
// 代价是 alpha API 的 JSON 编解码开销 —— 对"每 30 分钟调一次"的刷新任务,
// 这一点开销可以忽略。
//
// # 语义要点(来自 Dapr 官方文档)
//   - Jobs 是**调度器,不是执行器**:保证「不早于计划时间触发」,
//     **不保证**「到点后多久触发」,语义是 at-least-once。
//   - 同一个名字重复创建会失败,除非 `overwrite: true`。
//     本客户端默认 overwrite —— cube app 每次启动都要能重新登记自己的 job。
//   - job 记录存在 Scheduler 的内嵌 etcd 里,**跨 app 重启存活**。
//   - 触发时 sidecar 会 POST 到 app 的 `/job/<job-name>`。
package daprclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// jobsAPIPrefix 是 Jobs API 的路径前缀(注意是 alpha 版本)。
const jobsAPIPrefix = "/v1.0-alpha1/jobs"

// JobsClient 调本机 dapr sidecar 的 Jobs API。
type JobsClient struct {
	SidecarAddr string       // 例 "http://127.0.0.1:3003"
	HTTPClient  *http.Client // 可注入(测试用),默认 Timeout=0(由 ctx 控制)
}

// NewJobsClient 构造 Jobs 客户端。
func NewJobsClient(sidecarAddr string) *JobsClient {
	return &JobsClient{
		SidecarAddr: strings.TrimRight(sidecarAddr, "/"),
		HTTPClient:  &http.Client{},
	}
}

// ScheduleJobRequest 是 Jobs API 的请求体(只声明我们用得到的字段)。
type ScheduleJobRequest struct {
	// Schedule 支持 "@every 30m" 这类周期表达式,或 6 字段 cron。
	Schedule string `json:"schedule,omitempty"`
	// Data 会在触发时原样回传给 app 的 /job/<name>。
	Data string `json:"data,omitempty"`
	// Repeats 不设 = 一直重复。
	Repeats *int `json:"repeats,omitempty"`
	// TTL 是 job 的过期时间。
	TTL string `json:"ttl,omitempty"`
	// Overwrite 允许覆盖同名 job。**默认给 true**:
	// app 每次重启都要能重新登记,否则第二次启动会因为名字已存在而静默拿不到调度。
	Overwrite bool `json:"overwrite"`
}

// ScheduleJob 创建/覆盖一个定时 job。
func (c *JobsClient) ScheduleJob(ctx context.Context, name string, req ScheduleJobRequest) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("daprclient: job name 不能为空")
	}
	req.Overwrite = true

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("daprclient: 编码 job %s: %w", name, err)
	}
	endpoint := c.SidecarAddr + jobsAPIPrefix + "/" + url.PathEscape(name)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		// 这里**必须**和调用方约定:连不上 = scheduler 不可用,
		// 调用方据此回退到进程内 ticker,而不是假装调度成功。
		return classifyTransportErr("jobs", "schedule:"+name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return &InvokeError{
		Kind:        ErrHTTPStatus,
		TargetAppID: "jobs",
		Method:      "schedule:" + name,
		StatusCode:  resp.StatusCode,
		Body:        []byte(truncateBytes(raw, 512)),
	}
}

// DeleteJob 删除一个 job。用于优雅关闭时清理(可选)。
func (c *JobsClient) DeleteJob(ctx context.Context, name string) error {
	endpoint := c.SidecarAddr + jobsAPIPrefix + "/" + url.PathEscape(name)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return classifyTransportErr("jobs", "delete:"+name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return &InvokeError{
		Kind:        ErrHTTPStatus,
		TargetAppID: "jobs",
		Method:      "delete:" + name,
		StatusCode:  resp.StatusCode,
		Body:        []byte(truncateBytes(raw, 512)),
	}
}
