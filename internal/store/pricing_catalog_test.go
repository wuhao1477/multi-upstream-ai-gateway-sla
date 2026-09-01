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
