// Package slagateway 只做一件事：把仓库根目录下的 migrations/ 嵌入二进制。
//
// 为什么需要一个根包：`go:embed` 不能引用包目录之外的路径，而 [10 §1] 把
// migrations/ 定在仓库根。两个选择——把迁移挪进 internal/store/（改变文档
// 已定的布局），或加一个只负责嵌入的根包。选后者：布局是文档契约，
// 而这个包没有任何逻辑，只有一行嵌入声明。
//
// 嵌入而非运行时读盘的理由：二进制自带迁移，部署时不必挂载文件，
// 也不会出现"镜像里的代码与挂进去的迁移版本不匹配"（06 §2.2 幂等 bootstrap）。
package slagateway

import "embed"

// MigrationFS 是 migrations/ 的只读快照。
//
//go:embed all:migrations
var MigrationFS embed.FS
