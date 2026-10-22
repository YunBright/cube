// Package dapr 收录本仓随二进制分发给生产机的 Dapr 资产。
//
// # 为什么这个包只有一个 doc.go
//
// 因为本包下面挂着一条**回归锁**(components_test.go):components 目录里的
// 任何 Component YAML 都会被 daprd **自动加载**,不需要 unit 显式引用 ——
// 所以一份放错/过期的组件文件能在**部署时**把整个 app 打死,而且**零报错**
// (症状只是"服务起不来 / 502")。
//
// 而 goreleaser 的 gateway 归档里**就带着** dapr/components
// (见 .goreleaser.yaml:`src: dapr/components` → `dst: components`)
// ⇒ 每次部署 cube 都会把仓里这份覆盖到服务器。
//
// ⇒ 锁必须放在**本仓**(仓里的那份才是被打包的那份),
// 放在 deployer 仓锁不住 —— 2026-10-20 就因此又炸了一次(见 components_test.go 的注释)。
package dapr
