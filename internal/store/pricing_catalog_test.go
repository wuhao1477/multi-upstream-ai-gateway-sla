package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

func TestSavePricingUpdatesExistingCatalogPriceOnly(t *testing.T) {
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

	const base = "https://pricing-catalog-update.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)

	ch := Channel{Name: "pricing-catalog-update", SiteFamily: "newapi", BaseURL: base}
	ch.ID, err = CreateChannel(ctx, conn, ch)
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}

	sink := NewCollectorSink(pool)
	firstSeen := time.Now().Add(-time.Hour).UTC()
	if _, err := sink.SaveCatalog(ctx, ch.ID, []collector.CatalogModel{{
		ModelName: "m", InputPrice: 1, OutputPrice: 1,
		BillingUnit: "per_call", Meta: collector.SourceMeta{FetchedAt: firstSeen},
	}}); err != nil {
		t.Fatalf("写初始目录: %v", err)
	}
	beforeSeen, beforeSeq := catalogSeenVersion(t, ctx, conn, ch.ID, "m")

	if _, err := sink.SavePricing(ctx, ch.ID, collector.Pricing{
		Models: []collector.ModelPrice{{
			ModelName: "m", InputPrice: 2, OutputPrice: 3, BillingUnit: "per_1m_token",
		}},
		Meta: collector.SourceMeta{FetchedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("写价格: %v", err)
	}

	entries, err := ListCatalog(ctx, conn, ch.ID, false, 3)
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	got := catalogEntry(entries, "m")
	if got.InputPrice == nil || *got.InputPrice != 2 ||
		got.OutputPrice == nil || *got.OutputPrice != 3 ||
		got.BillingUnit == nil || *got.BillingUnit != "per_1m_token" {
		t.Fatalf("价格周期未更新目录价格，得到 %+v", got)
	}
	afterSeen, afterSeq := catalogSeenVersion(t, ctx, conn, ch.ID, "m")
	if !afterSeen.Equal(beforeSeen) || afterSeq != beforeSeq {
		t.Fatalf("价格周期不应推进目录可见轮次，before=%s/%d after=%s/%d",
			beforeSeen, beforeSeq, afterSeen, afterSeq)
	}
}

func TestSavePricingStoresSnapshotBeforeCountingRow(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
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

	const base = "https://pricing-snapshot-atomic.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)
	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "pricing-snapshot-atomic", SiteFamily: "newapi", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO models (canonical_name) VALUES ('pricing-model')`); err != nil {
		t.Fatalf("建模型: %v", err)
	}
	// models 没有 channel_id，wipeInventoryTest 按 base_url 清不到这行；
	// 不删则同一个库第二次跑本用例必撞 models_canonical_name_key。
	defer func() {
		ctx := context.Background()
		if _, err := conn.Exec(ctx, `
DELETE FROM price_versions WHERE model_id IN (
	SELECT id FROM models WHERE canonical_name='pricing-model')`); err != nil {
			t.Errorf("清理价格版本: %v", err)
		}
		if _, err := conn.Exec(ctx,
			`DELETE FROM models WHERE canonical_name='pricing-model'`); err != nil {
			t.Errorf("清理模型: %v", err)
		}
	}()
	if _, err := NewCollectorSink(pool).SaveCatalog(ctx, channelID, []collector.CatalogModel{{
		ModelName: "pricing-model", InputPrice: 1, OutputPrice: 1,
		BillingUnit: "per_call", Meta: collector.SourceMeta{FetchedAt: time.Now()},
	}}); err != nil {
		t.Fatalf("写目录: %v", err)
	}
	cleanupTrigger := installSnapshotFailureTrigger(t, ctx, conn, "pricing_snapshot_fail")
	defer cleanupTrigger()
	result, err := NewCollectorSink(pool).SavePricing(ctx, channelID, collector.Pricing{
		Models: []collector.ModelPrice{{ModelName: "pricing-model", InputPrice: 2, OutputPrice: 3,
			BillingUnit: "per_call"}}, Meta: collector.SourceMeta{FetchedAt: time.Now()},
	})
	if err == nil {
		t.Fatal("快照写入失败时 SavePricing 应返回错误")
	}
	if result.Snapshots != 0 {
		t.Fatalf("快照失败时不应报告已持久化行数，得到 %d", result.Snapshots)
	}
	var input float64
	if err := conn.QueryRow(ctx, `
SELECT input_price FROM channel_model_catalog
 WHERE channel_id=$1 AND model_name='pricing-model'`, channelID).Scan(&input); err != nil {
		t.Fatalf("读目录价格: %v", err)
	}
	if input != 1 {
		t.Fatalf("快照失败后目录价格未回滚，得到 %v", input)
	}
	var versions, snapshots int
	if err := conn.QueryRow(ctx, `
SELECT (SELECT count(*) FROM price_versions pv JOIN channels c ON c.id=pv.channel_id WHERE c.id=$1),
       (SELECT count(*) FROM collector_snapshots WHERE channel_id=$1 AND scope_type='pricing')`, channelID).
		Scan(&versions, &snapshots); err != nil {
		t.Fatalf("数价格与快照: %v", err)
	}
	if versions != 0 || snapshots != 0 {
		t.Fatalf("快照失败后价格版本/快照未回滚: versions=%d snapshots=%d", versions, snapshots)
	}
}

func catalogSeenVersion(
	t *testing.T, ctx context.Context, conn *pgx.Conn, channelID int64, model string,
) (time.Time, int64) {
	t.Helper()
	var seen time.Time
	var seq int64
	if err := conn.QueryRow(ctx, `
SELECT last_seen_at, last_seen_seq
  FROM channel_model_catalog
 WHERE channel_id=$1 AND model_name=$2`, channelID, model).Scan(&seen, &seq); err != nil {
		t.Fatalf("读目录可见版本: %v", err)
	}
	return seen, seq
}
