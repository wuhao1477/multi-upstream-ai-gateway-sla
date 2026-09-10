package store

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/collector"
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

func TestSaveGroupsPartialResultPreservesAndAddsModels(t *testing.T) {
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

	const base = "https://group-models-partial.example.invalid"
	_, _ = conn.Exec(ctx, `DELETE FROM channels WHERE base_url=$1`, base)
	defer func() {
		_, _ = conn.Exec(context.Background(), `DELETE FROM channels WHERE base_url=$1`, base)
	}()

	var channelID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url)
VALUES ('group-models-partial', 'sub2api', $1)
RETURNING id`, base).Scan(&channelID); err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	sink := NewCollectorSink(pool)
	if _, err := sink.SaveGroups(ctx, channelID, []collector.Group{{
		GroupRef: "paid", AvailableModels: []string{"model-old", "model-stale"},
		Meta: collector.SourceMeta{FetchedAt: time.Now()},
	}}); err != nil {
		t.Fatalf("写初始分组模型: %v", err)
	}
	if _, err := sink.SaveGroups(ctx, channelID, []collector.Group{{
		GroupRef: "paid", AvailableModels: []string{"model-new"},
		Meta: collector.SourceMeta{FetchedAt: time.Now(), Partial: true},
	}}); err != nil {
		t.Fatalf("写部分分组模型: %v", err)
	}

	rows, err := conn.Query(ctx, `
SELECT gm.model_name
  FROM group_models gm
  JOIN channel_groups g ON g.id=gm.channel_group_id
 WHERE g.channel_id=$1 AND g.group_ref='paid'
 ORDER BY gm.model_name`, channelID)
	if err != nil {
		t.Fatalf("查分组模型: %v", err)
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			t.Fatalf("读分组模型: %v", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读分组模型: %v", err)
	}
	want := []string{"model-new", "model-old", "model-stale"}
	if !slices.Equal(models, want) {
		t.Fatalf("部分分组结果必须保留旧模型并追加新模型，得到 %v，期望 %v", models, want)
	}
}

func TestSaveGroupsRollsBackBusinessRowsWhenSnapshotFails(t *testing.T) {
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

	const base = "https://groups-snapshot-atomic.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	defer wipeInventoryTest(context.Background(), t, conn, base)
	var channelID int64
	if err := conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url)
VALUES ('groups-snapshot-atomic', 'newapi', $1) RETURNING id`, base).Scan(&channelID); err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	cleanupTrigger := installSnapshotFailureTrigger(t, ctx, conn, "groups_snapshot_fail")
	defer cleanupTrigger()
	_, err = NewCollectorSink(pool).SaveGroups(ctx, channelID, []collector.Group{{
		GroupRef: "paid", RateMultiplier: 0.5,
		Meta: collector.SourceMeta{FetchedAt: time.Now()},
	}})
	if err == nil {
		t.Fatal("快照写入失败时 SaveGroups 应返回错误")
	}
	var count int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM channel_groups WHERE channel_id=$1`, channelID).Scan(&count); err != nil {
		t.Fatalf("查分组: %v", err)
	}
	if count != 0 {
		t.Fatalf("快照失败后分组业务行未回滚，剩余 %d 行", count)
	}
}

func installSnapshotFailureTrigger(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) func() {
	t.Helper()
	if _, err := conn.Exec(ctx, `
CREATE OR REPLACE FUNCTION test_fail_collector_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'snapshot failure'; END $$;
CREATE TRIGGER `+name+` BEFORE INSERT ON collector_snapshots
FOR EACH ROW EXECUTE FUNCTION test_fail_collector_snapshot();`); err != nil {
		t.Fatalf("安装快照失败触发器: %v", err)
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON collector_snapshots`)
		_, _ = conn.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fail_collector_snapshot()`)
	}
}
