// Package appcfg 决定 cube app 与本地验证工具**读哪一份实例配置**。
//
// # 两份文件的分工
//
//	config.yaml         实例配置本体(DSN + 源表名)。**不在版本库**(根 .gitignore
//	                    第 26 行忽略它),因为里面有源库口令。它同时是 goreleaser
//	                    打包进归档的那份 —— 部署会用**打包机上本机工作区的那份**
//	                    覆盖远端。
//	config.local.yaml   本地验证用的覆盖文件,**也不在版本库**。
//	                    典型用途:连真实源库跑 sqlcheck / sqlq 时,把 DSN 指向
//	                    本机 ssh 隧道端口,或者指向一个测试库,而不必动那份会被
//	                    部署出去的 config.yaml。
//
// # 优先级:config.local.yaml > config.yaml
//
// 存在即生效,不读环境变量、不看命令行 —— 这个项目已经被"路径靠猜"的
// 静默失败坑过(路径对不上 → 模型一个没加载 → 启动不报错 → 查询时才炸)。
//
// # 这个优先级有一个真实风险,处理方式见下
//
// config.local.yaml 一旦留在**服务器**上,就会盖住部署刚推下去的 config.yaml,
// 于是"部署改了配置但没生效",而且没有任何报错。
// 两个措施把这个风险压到可接受:
//
//  1. app 启动日志里 `config loaded` 一定带 `file=<实际用的文件名>`,
//     一眼能看出用的是哪份。
//  2. `deploy-cube.ps1 -Step prune` 会删掉远端的 config.local.yaml ——
//     本地验证的临时文件不该跨部署存活。
//
// 不加第三个措施(比如"服务器上存在就拒启动"),因为那会让"在服务器上临时
// 覆盖配置"这个正当用法变得不可用,而它本来就没什么正当理由 —— 真要临时换配置,
// 改 config.yaml 本身更可控。
package appcfg

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	// LocalFile 是本地验证用的覆盖文件,优先于 SharedFile。
	LocalFile = "config.local.yaml"
	// SharedFile 是实例配置本体,也是打包进归档的那份。
	SharedFile = "config.yaml"
)

// Resolve 返回该目录下的"生效配置文件"路径。
//
// 注意:返回的是 SharedFile 时**不代表它存在** —— 调用方仍要处理读不到的错误。
// 这里故意不因为缺失就报错,因为 sqlcheck / sqlq 想要的是"我要读哪个路径",
// 缺失应该由真正的读取动作给出比"文件不存在"更具体的错误。
func Resolve(dir string) string {
	local := filepath.Join(dir, LocalFile)
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local
	}
	return filepath.Join(dir, SharedFile)
}

// Load 读 dir 下的生效配置(config.local.yaml 优先)。
func Load[T any](dir string) (T, string, error) {
	return LoadFrom[T](Resolve(dir))
}

// LoadFrom 解析指定路径的配置,返回配置值与**实际使用的文件路径**。
//
// 泛型是为了让四个入口(sixun-ysx、sixun-hbposv7、sqlcheck、sqlq)共用这一个实现:
// 它们的 config 结构体字段不同,但"怎么解析 + 报什么错"不该各写一遍。
// sqlcheck / sqlq 走这个(因为它们支持 -config 指定任意路径),app 走 Load。
//
// 调用方**必须**把 usedPath 打进启动日志 —— 那是"我到底读了哪份配置"的唯一
// 可见信号,漏了就等于上面那条静默覆盖风险重新变成不可见。
func LoadFrom[T any](path string) (cfg T, usedPath string, err error) {
	var zero T

	data, err := os.ReadFile(path)
	if err != nil {
		return zero, path, fmt.Errorf("appcfg: 读配置 %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return zero, path, fmt.Errorf("appcfg: 解析 %s: %w", path, err)
	}
	return cfg, path, nil
}
