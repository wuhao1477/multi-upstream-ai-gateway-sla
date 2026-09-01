package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

func TestUnregisteredKeySnapshotVisibleInInventory(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("建连接池: %v", err)
	}
	defer pool.Close()

	const base = "https://unregistered-key-inventory.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)

	ch := Channel{Name: "unregistered-key-inventory", SiteFamily: "newapi", BaseURL: base}
	ch.ID, err = CreateChannel(ctx, conn, ch)
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}

	err = NewCollectorSink(pool).SaveKey(ctx, ch.ID, 0, collector.Key{
		KeyRef:         "remote-only-key",
		RemainQuotaUSD: quotaPtr(3),
		Meta:           collector.SourceMeta{FetchedAt: time.Now()},
	})
	if !errors.Is(err, collector.ErrKeyNotRegistered) {
		t.Fatalf("应返回未登记 Key，得到 %v", err)
	}

	inv, err := BuildInventory(ctx, conn, ch, 3)
	if err != nil {
		t.Fatalf("构建资产总览: %v", err)
	}
	if !hasAnomalyItem(inv.Anomalies, "unregistered_key", "remote-only-key") {
		t.Fatalf("未登记 Key 应出现在资产异常，得到 %+v", inv.Anomalies)
	}

	accountID, err := CreateAccount(ctx, conn, Account{ChannelID: ch.ID})
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if _, err := CreateKey(ctx, conn, accountID, "sk-test", "remote-only-key", nil); err != nil {
		t.Fatalf("补登记 Key: %v", err)
	}
	inv, err = BuildInventory(ctx, conn, ch, 3)
	if err != nil {
		t.Fatalf("重新构建资产总览: %v", err)
	}
	if hasAnomalyItem(inv.Anomalies, "unregistered_key", "remote-only-key") {
		t.Fatalf("补登记后未登记异常应消失，得到 %+v", inv.Anomalies)
	}
}

func TestSaveKeyPersistsZeroQuota(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("建连接池: %v", err)
	}
	defer pool.Close()

	const base = "https://zero-key-quota.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)

	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "zero-key-quota", SiteFamily: "newapi", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	accountID, err := CreateAccount(ctx, conn, Account{ChannelID: channelID})
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}
	if _, err := CreateKey(ctx, conn, accountID, "sk-zero", "zero-key", nil); err != nil {
		t.Fatalf("建 Key: %v", err)
	}

	sink := NewCollectorSink(pool)
	for _, quota := range []float64{3, 0} {
		if err := sink.SaveKey(ctx, channelID, accountID, collector.Key{
			KeyRef: "zero-key", RemainQuotaUSD: quotaPtr(quota), UsedQuotaUSD: quotaPtr(quota),
			Meta: collector.SourceMeta{FetchedAt: time.Now()},
		}); err != nil {
			t.Fatalf("保存额度 %v: %v", quota, err)
		}
	}

	var remain, used float64
	if err := conn.QueryRow(ctx, `
SELECT remain_quota_usd, used_quota_usd
  FROM upstream_keys WHERE account_id=$1`, accountID).Scan(&remain, &used); err != nil {
		t.Fatalf("读 Key 额度: %v", err)
	}
	if remain != 0 || used != 0 {
		t.Fatalf("明确采到 0 额度必须覆盖旧值，实际 remain=%v used=%v", remain, used)
	}
}

func quotaPtr(v float64) *float64 { return &v }

func hasAnomalyItem(items []Anomaly, kind, item string) bool {
	for _, anomaly := range items {
		if anomaly.Kind != kind {
			continue
		}
		for _, got := range anomaly.Items {
			if got == item {
				return true
			}
		}
	}
	return false
}

func wipeInventoryTest(ctx context.Context, t *testing.T, conn *pgx.Conn, base string) {
	t.Helper()
	stmts := []string{
		`DELETE FROM collector_snapshots WHERE channel_id IN (
			SELECT id FROM channels WHERE base_url=$1)`,
		`DELETE FROM upstream_keys WHERE account_id IN (
			SELECT a.id FROM upstream_accounts a JOIN channels ch ON ch.id=a.channel_id
			WHERE ch.base_url=$1)`,
		`DELETE FROM upstream_accounts WHERE channel_id IN (
			SELECT id FROM channels WHERE base_url=$1)`,
		`DELETE FROM group_models WHERE channel_group_id IN (
			SELECT g.id FROM channel_groups g JOIN channels ch ON ch.id=g.channel_id
			WHERE ch.base_url=$1)`,
		`DELETE FROM channel_groups WHERE channel_id IN (
			SELECT id FROM channels WHERE base_url=$1)`,
		`DELETE FROM channel_model_catalog WHERE channel_id IN (
			SELECT id FROM channels WHERE base_url=$1)`,
		`DELETE FROM channels WHERE base_url=$1`,
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt, base); err != nil {
			t.Fatalf("清理资产总览测试渠道: %v", err)
		}
	}
}
