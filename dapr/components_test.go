// 本文件是**部署契约**的回归锁:随 cube 归档分发的 dapr 组件目录不允许
// 藏着会让 daprd fatal 的组件。
//
// # 事故背景(2026-10-20,实测两次)
//
// 1. sidecar 的 `middleware.http.bearer` 组件(auth-validator)2026-10-12 移除
//    (它会打断 pubsub 的 /dapr/subscribe 订阅发现),应用层验签接管。
// 2. auth 合并(auth-login 进程退出,app 端口 **8081 → 8082**)之后,
//    那份组件里的 jwksURL 指向了**已经不存在的 :8081**。
//
// ⇒ 症状:components 目录里的 Component YAML 是**自动加载**的
// ⇒ daprd 启动时去拉 JWKS ⇒ connection refused ⇒ **daprd fatal 退出**
// ⇒ 整个 cube app 跟着死 ⇒ systemd 反复重启(实测 30 秒一轮)
// ⇒ catalog / inventory / sales-agg 的契约测试全线 502。
//
// ⚠️⚠️ **锁为什么必须在本仓(cube)而不是 deployer 仓**:
//
//	goreleaser 的 gateway 归档里**带着** dapr/components
//	(见 .goreleaser.yaml:`src: dapr/components` → `dst: components`)
//	⇒ 每次部署 cube 都会把**仓里这份**覆盖到服务器
//	⇒ 仓里这份是活组件,就等于**每次部署都把 cube 再打死一次**。
//
//	2026-10-20 就是这么复发一轮的:生产上 14:14 把它挪进了
//	components/deprecated/,可当天 22:24 的部署又把仓里那份覆盖回 components/,
//	cube 三个服务立刻崩溃循环 ⇒ 整条 cube 链(以及依赖它的 9 个 supertrade 服务)全挂。
//
//	deployer 仓那份(dapr/gyy/cube/components/bearer-auth.yaml)是**墓碑**,
//	它锁的是"同步到服务器的那份",而**不是**"被打包进归档的那份" —— 两者是不同文件。
package dapr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// componentsDir 是"会被打进归档、并在生产上被 daprd 自动加载"的目录。
//
// ⚠️ 相对路径:测试的工作目录就是本包目录(dapr/)。
const componentsDir = "components"

// deadAuthPort 是 auth 合并后**已经不存在的**端口。
//
// 合并前 auth-login 监听 8081;合并成一个进程(userd)后监听 **8082**
// (见 deployer 仓 tests/test_configuration.default.yaml 的 auth.internal_base_url)。
//
// ⇒ 任何组件里再出现 :8081,就是**过期配置** —— 它不会报错,只会让
// 拉 JWKS 的那个组件 fatal。
const deadAuthPort = ":8081"

// readComponent 读 components 目录下的一个 yaml(原样字节)。
func readComponent(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(componentsDir, name))
	if err != nil {
		t.Fatalf("读不到 %s/%s: %v", componentsDir, name, err)
	}
	return string(raw)
}

// TestNoBearerMiddlewareComponent —— components 目录里不许有
// `middleware.http.bearer` 组件。
//
// 理由见文件头:它在 2026-10-12 被移除(会打断 pubsub 订阅发现),
// 而 components 目录是**自动加载**的 —— 文件还在,sidecar 就会去拉 JWKS,
// 拉不到就 fatal 退出 ⇒ 整个 app 跟着死。
//
// 唯一允许存在的形式是**墓碑**:只有注释、没有 apiVersion/kind
// ⇒ daprd 认不出它是 Component ⇒ 直接跳过。
func TestNoBearerMiddlewareComponent(t *testing.T) {
	entries, err := os.ReadDir(componentsDir)
	if err != nil {
		t.Fatalf("读不到 %s 目录: %v", componentsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		body := readComponent(t, name)
		if !strings.Contains(body, "middleware.http.bearer") {
			continue
		}
		// 提到了这个类型 —— 必须是墓碑(没有 apiVersion/kind ⇒ daprd 会跳过)。
		if strings.Contains(body, "apiVersion:") || strings.Contains(body, "kind:") {
			t.Errorf("%s/%s 是一个**活的** middleware.http.bearer 组件。\n\n"+
				"它会被 daprd 自动加载 ⇒ sidecar 去拉 JWKS ⇒ 拉不到就 fatal 退出 ⇒\n"+
				"整个 cube app 跟着死、systemd 反复重启(实测 30 秒一轮)。\n"+
				"2026-10-12 这个组件已被移除(会打断 pubsub 订阅发现),应用层验签接管。\n"+
				"⇒ 只能留**墓碑**:把文件内容换成纯注释(没有 apiVersion/kind),\n"+
				"  daprd 认不出它是 Component 就直接跳过。", componentsDir, name)
		}
	}
}

// TestNoComponentReferencesDeadAuthPort —— components 目录里不许指向已下线的 :8081。
//
// 症状对照:指针错了不会报"配置错误",而是 daprd 启动时
// "failed to fetch JWKS: ... :8081 ... connection refused" 然后 fatal。
// 看着像"服务挂了",真因是**一行过期的 URL**。
func TestNoComponentReferencesDeadAuthPort(t *testing.T) {
	entries, err := os.ReadDir(componentsDir)
	if err != nil {
		t.Fatalf("读不到 %s 目录: %v", componentsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		body := readComponent(t, name)
		if !strings.Contains(body, deadAuthPort) {
			continue
		}
		// 墓碑里可以**提到**这个端口(它在解释为什么不能复活)——
		// 判据是"它是不是一个活组件"。
		if strings.Contains(body, "apiVersion:") || strings.Contains(body, "kind:") {
			t.Errorf("%s/%s 指向已下线的 auth 端口 %s。\n\n"+
				"auth 合并后 app 监听 **8082**,:8081 已经没有进程了 ⇒\n"+
				"任何去拉 JWKS 的组件都会 fatal 退出 ⇒ 整个 cube app 跟着死。\n"+
				"⇒ 要么把 URL 改对,要么(按 2026-10-12 的结论)把整个组件移出 components 目录。",
				componentsDir, name, deadAuthPort)
		}
	}
}

// TestBearerAuthTombstoneIsNotAComponent —— bearer-auth.yaml 必须是墓碑。
//
// 单独再钉一次:上面两条是"扫目录找坏组件",这条是"钉住那个**具体文件**"。
// 理由:它是唯一被移除过的组件,也是两次事故的主角 ——
// 泛化规则漏了它,最坏的情况就是它被原样复活。
func TestBearerAuthTombstoneIsNotAComponent(t *testing.T) {
	path := filepath.Join(componentsDir, "bearer-auth.yaml")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// 直接删掉也行(那就更不会复活)。
		return
	}
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	body := string(raw)
	if strings.Contains(body, "apiVersion:") || strings.Contains(body, "kind:") {
		t.Errorf("%s 是**活的** Dapr Component(有 apiVersion/kind)。\n\n"+
			"它会被 daprd 自动加载 ⇒ 拉 JWKS 拉不到 ⇒ fatal 退出 ⇒ 整个 cube app 死。\n"+
			"⇒ 只能留墓碑:纯注释、没有 apiVersion/kind。", path)
	}
}
