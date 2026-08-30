// registry-dump 把站型注册表按行打印，供 shell 侧的验收断言消费。
//
// 为什么要有这个命令：verify/test-migrate.sh 要断言"库里 site_family / cred_type
// 的 CHECK 取值 == 注册表推导出来的取值"。若在 shell 里写一份家族清单，那份清单
// 就是第二份真相源，而"加了一族忘了同步"正是那条断言要查的事 —— 清单漏一项与
// 迁移漏一条一样静默（同 registry_test.go 扫源码而不手写清单的理由）。
//
// 输出：每族一行，制表符分隔的三列
//
//	<family>\t<cred_type>\t<passwd_cred_type>
//
// 第三列为空表示该族不允许账密登记。选制表符是因为 cut -f 好取，
// 且这三个字段都不可能含制表符（它们是 Go 源码里的字面量标识符）。
package main

import (
	"fmt"
	"os"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

func main() {
	regs := collector.All()
	if len(regs) == 0 {
		// 空注册表意味着 registrations 切片被清空或链接错了包。
		// 静默打印零行会让调用方的断言拿到空集去比对，那是"绿得毫无意义"。
		fmt.Fprintln(os.Stderr, "registry-dump: 注册表为空")
		os.Exit(1)
	}
	for _, r := range regs {
		fmt.Printf("%s\t%s\t%s\n", r.Family, r.CredType, r.PasswdCredType)
	}
}
