package collector

import (
	"os"
	"testing"
)

// 本包的上游是本机 httptest 服务。走生产同一入口显式授权回环，而不是关闭出站校验；
// 未授权时的拒绝行为由 outbound_test.go 用独立的策略实例验证。
func TestMain(m *testing.M) {
	if err := ConfigureOutbound(`{"127.0.0.1": ["127.0.0.1/32"]}`); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
