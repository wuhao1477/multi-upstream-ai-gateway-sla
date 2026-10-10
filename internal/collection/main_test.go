package collection

import (
	"encoding/base64"
	"os"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 本包的上游是本机 httptest 服务。走生产同一入口显式授权回环，而不是关闭出站校验；
// 未授权时的拒绝行为由 collector 的 outbound_test.go 用独立的策略实例验证。
func TestMain(m *testing.M) {
	if err := collector.ConfigureOutbound(`{"127.0.0.1": ["127.0.0.1/32"]}`); err != nil {
		panic(err)
	}
	// 各测试包共用一个测试库（hub_sync_config 只有一行），密钥必须一致才能互读密文。
	// 只用于测试数据；生产密钥来自部署环境，从不入库。
	testKey := base64.StdEncoding.EncodeToString([]byte("sla-test-credential-key-32bytes!"))
	if err := store.ConfigureCredentialKey(testKey); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
