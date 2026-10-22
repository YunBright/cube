package daprclient

import (
	"net/http"
	"os"
)

// daprAPITokenEnvVar 是 Dapr API token 的环境变量名。
//
// 令牌是**全网共享**的:所有 unit 有同一个 DAPR_API_TOKEN,
// `dapr run` 会把父进程的环境变量传给 app 进程 ⇒ app 不用额外配置。
//
// ⚠️ 令牌**不是**每台机器一个 sidecar 之间会互相同步的
// (sidecar 的服务调用会过滤掉令牌不一致的实例,
// 换句话说令牌必须**全网一致**,换令牌必须全网一起换)。
const daprAPITokenEnvVar = "DAPR_API_TOKEN"

// daprAPIToken 返回当前进程应带的 dapr-api-token(空 = 不带)。
//
// 空值时**不带**头:sidecar 会 401,而"401 + 没带令牌"比"401 + 带错令牌"
// 更容易定位。
func daprAPIToken() string {
	return os.Getenv(daprAPITokenEnvVar)
}

// setAPIToken 给请求带上 dapr-api-token(若配置了)。
//
// 集中在一处 ⇒ 新增调用点不会漏。
//
// # 为什么必须有
//
// sidecar 的 HTTP API(/v1.0/healthz 除外,/v1.0/metadata、/v1.0/state/*、
// /v1.0/invoke/* 全部 401)。
//
// 不带这个头的客户端的每个调用都会 401,而症状是"整个源都离线"
// (SOURCE_OFFLINE)。排查方向会被带到"cube app 没起来"——
// 而真实原因是**令牌没配**。
func setAPIToken(req *http.Request) {
	if req == nil {
		return
	}
	if tok := daprAPIToken(); tok != "" {
		req.Header.Set("dapr-api-token", tok)
	}
}
