package main

import (
	"context"
	"errors"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 装配阶段的失败必须能被 errors.Is 认出是 ErrPrecondition。
//
// 这条守的是 %w 这个字符：一旦有人改成 %v，admin 的 sync 处理器就认不出
// "没打上游"，本地失败会重新开始消耗 60 秒限流窗口，而且状态码从 422 退回
// 502（把配置问题说成上游故障）。两个回归都不会让别的测试变红，故单独守。
func TestRunChannelSyncUnknownFamilyIsPrecondition(t *testing.T) {
	// 站型未知在 adapterFor 就返回，不会碰 pool —— 故可传 nil。
	_, err := runChannelSync(context.Background(), nil, nil, nil, nil,
		store.Channel{ID: 1, SiteFamily: "definitely-not-a-family"})
	if err == nil {
		t.Fatal("未知站型应报错")
	}
	if !errors.Is(err, collector.ErrPrecondition) {
		t.Fatalf("应裹 ErrPrecondition，得到 %v", err)
	}
	// 原因要保留下来，否则用户只看到"前置条件不满足"无从下手。
	if !contains(err.Error(), "definitely-not-a-family") {
		t.Fatalf("错误应保留原因，得到 %q", err.Error())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
