package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

func TestSaveGroupsMissingModelFieldPreservesPreviousModels(t *testing.T) {
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

	const base = "https://group-models-test.example.invalid"
	_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base)
	defer func() {
		_, _ = conn.Exec(context.Background(), `DELETE FROM channels WHERE base_url=$1`, base)
	}()

	var channelID int64
	err = conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url)
VALUES ('group-models-test', 'sub2api', $1)
RETURNING id`, base).Scan(&channelID)
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}

	sink := NewCollectorSink(pool)
	_, err = sink.SaveGroups(ctx, channelID, []collector.Group{{
		GroupRef: "paid", AvailableModels: []string{"model-a", "model-b"},
		Meta: collector.SourceMeta{FetchedAt: time.Now()},
	}})
	if err != nil {
		t.Fatalf("写初始分组模型: %v", err)
	}
	_, err = sink.SaveGroups(ctx, channelID, []collector.Group{{
		GroupRef: "paid",
		Meta: collector.SourceMeta{
			FetchedAt: time.Now(), Degraded: true,
			MissingFields: []string{"available_models"},
		},
	}})
	if err != nil {
		t.Fatalf("写降级分组: %v", err)
	}

	var count int
	err = conn.QueryRow(ctx, `
SELECT count(*) FROM group_models gm
JOIN channel_groups g ON g.id=gm.channel_group_id
WHERE g.channel_id=$1 AND g.group_ref='paid'`, channelID).Scan(&count)
	if err != nil {
		t.Fatalf("查分组模型: %v", err)
	}
	if count != 2 {
		t.Fatalf("缺少 available_models 的降级响应不应清空旧清单，实际剩 %d 条", count)
	}
}
