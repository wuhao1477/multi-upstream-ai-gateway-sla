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
