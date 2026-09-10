package store

import "testing"

func TestPoolConfigReadOnlyMode(t *testing.T) {
	const dsn = "postgres://user:pass@127.0.0.1:5432/db?sslmode=disable"
	readOnly, err := poolConfig(dsn, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := readOnly.ConnConfig.RuntimeParams["default_transaction_read_only"]; got != "on" {
		t.Fatalf("只读连接参数 = %q，期望 on", got)
	}
	readWrite, err := poolConfig(dsn, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readWrite.ConnConfig.RuntimeParams["default_transaction_read_only"]; ok {
		t.Fatal("普通连接池不应强制只读")
	}
}

// 采集 worker 每人峰值占两条连接（渠道锁 + 临时连接），pgx 在 ≤4 核机器上
// 默认只给 4 条，4 个 worker 会把池占满并阻塞到整渠道 120s 超时。
// parsed 直接传入，不依赖跑测试这台机器的核数 —— 否则 16 核 runner 上恒绿。
func TestMaxConnsRaisesSmallDefault(t *testing.T) {
	const dsn = "postgres://user:pass@127.0.0.1:5432/db?sslmode=disable"
	if got := maxConns(dsn, 4); got != MinPoolConns {
		t.Fatalf("4 核默认值被抬到 %d，期望 %d", got, MinPoolConns)
	}
	if got := maxConns(dsn, 32); got != 32 {
		t.Fatalf("已经够大的默认值被改成了 %d，期望保持 32", got)
	}
}

// 显式配置说了算：抬下限不能覆盖运维刻意调小的值。
func TestMaxConnsKeepsExplicitDSNValue(t *testing.T) {
	const dsn = "postgres://user:pass@127.0.0.1:5432/db?sslmode=disable&pool_max_conns=3"
	if got := maxConns(dsn, 3); got != 3 {
		t.Fatalf("MaxConns = %d，期望保留 DSN 显式写的 3", got)
	}
	cfg, err := poolConfig(dsn, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 3 {
		t.Fatalf("poolConfig 后 MaxConns = %d，期望 3", cfg.MaxConns)
	}
}
