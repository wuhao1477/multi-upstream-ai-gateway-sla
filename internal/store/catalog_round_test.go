package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

func TestCatalogPresenceReliable(t *testing.T) {
	cases := []struct {
		name   string
		models []collector.CatalogModel
		want   bool
	}{
		{name: "empty", want: false},
		{name: "blank model", models: []collector.CatalogModel{{}}, want: false},
		{name: "complete", models: []collector.CatalogModel{{ModelName: "m"}}, want: true},
		{name: "price degraded is still presence reliable", models: []collector.CatalogModel{{
			ModelName: "m", Meta: collector.SourceMeta{
				Degraded: true, MissingFields: []string{"input_price", "output_price"},
			},
		}}, want: true},
		{name: "partial account result is unreliable", models: []collector.CatalogModel{{
			ModelName: "m", Meta: collector.SourceMeta{Partial: true},
		}}, want: false},
		{name: "missing model list is unreliable", models: []collector.CatalogModel{{
			ModelName: "m", Meta: collector.SourceMeta{
				Degraded: true, MissingFields: []string{"available_models"},
			},
		}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := catalogPresenceReliable(tc.models); got != tc.want {
				t.Fatalf("catalogPresenceReliable() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestCatalogStaleAfterReliableMissingRounds(t *testing.T) {
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

	const base = "https://catalog-round-test.example.invalid"
	wipeCatalogRoundTest(ctx, t, conn, base)
	defer wipeCatalogRoundTest(context.Background(), t, conn, base)

	var channelID int64
	err = conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url)
VALUES ('catalog-round-test', 'unknown', $1)
RETURNING id`, base).Scan(&channelID)
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}

	sink := NewCollectorSink(pool)
	save := func(name string) {
		t.Helper()
		_, err := sink.SaveCatalog(ctx, channelID, []collector.CatalogModel{{
			ModelName: name,
			Meta:      collector.SourceMeta{FetchedAt: time.Now()},
		}})
		if err != nil {
			t.Fatalf("保存目录 %s: %v", name, err)
		}
	}

	save("target")
	save("other-1")
	save("other-2")
	save("other-3")

	entries, err := ListCatalog(ctx, conn, channelID, false, 3)
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	if !catalogEntry(entries, "target").Stale {
		t.Fatalf("target 连续 3 个可靠轮次缺席后应 stale，得到 %+v", entries)
	}

	save("target")
	entries, err = ListCatalog(ctx, conn, channelID, false, 3)
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	if catalogEntry(entries, "target").Stale {
		t.Fatalf("target 再次出现后应清除 stale，得到 %+v", entries)
	}
}

func catalogEntry(entries []CatalogEntry, name string) CatalogEntry {
	for _, entry := range entries {
		if entry.ModelName == name {
			return entry
		}
	}
	return CatalogEntry{}
}

func wipeCatalogRoundTest(ctx context.Context, t *testing.T, conn *pgx.Conn, base string) {
	t.Helper()
	if _, err := conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base); err != nil {
		t.Fatalf("清理目录轮次测试渠道: %v", err)
	}
}
