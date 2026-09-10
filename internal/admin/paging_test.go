package admin

import (
	"net/url"
	"testing"
)

// TestParsePagingClampsInsteadOfFallingBack 锁住"超上限截到上限"这个语义。
//
// 为什么值得一个专门的测试：这里原来写的是 `v > 0 && v <= 1000`，
// 于是 `?limit=1369`（真实渠道的目录规模）**静默退回 100**。
// 请求方拿到的是一个语法完整、看起来正常的响应，只是少了 1269 行 ——
// 运维据此认为"这渠道只有 100 个模型"。
// 这类缺陷不会报错、不会进日志，只能靠断言"边界外的输入怎么处理"守住。
func TestParsePagingClampsInsteadOfFallingBack(t *testing.T) {
	cases := []struct {
		name          string
		query         string
		limit, offset int
	}{
		{"缺省", "", pagingDefault, 0},
		{"正常值原样生效", "limit=50&offset=10", 50, 10},
		{"恰好等于上限", "limit=1000", pagingMax, 0},
		// 核心用例：超上限必须截到 1000，**不得**退回 100
		{"超上限截到上限", "limit=1369", pagingMax, 0},
		{"远超上限也截到上限", "limit=999999", pagingMax, 0},
		// 非法值走默认，不报错：分页参数不承载业务语义
		{"零与负数走默认", "limit=0&offset=-5", pagingDefault, 0},
		{"非数字走默认", "limit=abc", pagingDefault, 0},
		{"offset 可超出总数（由调用方截断）", "offset=100000", pagingDefault, 100000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatalf("构造查询串失败: %v", err)
			}
			gotLimit, gotOffset := parsePaging(q)
			if gotLimit != c.limit || gotOffset != c.offset {
				t.Fatalf("parsePaging(%q) = (%d,%d)，期望 (%d,%d)",
					c.query, gotLimit, gotOffset, c.limit, c.offset)
			}
		})
	}
}
