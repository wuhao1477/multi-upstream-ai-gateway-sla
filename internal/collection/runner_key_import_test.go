package collection

import (
	"context"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 直接传入标准化后的采集结果，验证真实 PostgreSQL 写入，不构造上游协议。
func TestImportFetchedKeysSavesInitialMetadata(t *testing.T) {
	access, session := cookieAccessFixture(t)
	ctx := context.Background()
	conn, release, err := access.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() {
		if _, err := conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id=$1`, session.AccountID); err != nil {
			t.Error(err)
		}
	}()
	remain, used := 12.5, 3.25
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	fetched := time.Now().UTC().Truncate(time.Microsecond)
	keys := []collector.Key{
		{KeyRef: "limited", Secret: "test-only-limited", RemainQuotaUSD: &remain, UsedQuotaUSD: &used,
			ExpiredAt: &expires, RateLimit: collector.RateLimit{RPM: 60, Concurrency: 3},
			Meta: collector.SourceMeta{FetchedAt: fetched}},
		{KeyRef: "unlimited", Secret: "test-only-unlimited", Unlimited: true, UsedQuotaUSD: &used,
			Meta: collector.SourceMeta{FetchedAt: fetched}},
	}
	runner := &Runner{}
	result, err := runner.importFetchedKeys(ctx, conn, store.Channel{ID: session.ChannelID},
		collector.Credential{AccountID: session.AccountID}, session, fixedKeyResolver{}, keys, nil)
	if err != nil || result.Imported != 2 || result.Failed != 0 || result.SecretResolves != 0 {
		t.Fatalf("first import result=%+v err=%v", result, err)
	}
	var complete int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM upstream_keys
		WHERE account_id=$1 AND used_quota_usd=3.25 AND quota_synced_at=$2
		AND ((external_ref='limited' AND remain_quota_usd=12.5 AND NOT unlimited_quota
		      AND expired_time=$3 AND rpm_limit=60 AND concurrency_limit=3)
		  OR (external_ref='unlimited' AND unlimited_quota AND remain_quota_usd IS NULL))`,
		session.AccountID, fetched, expires).Scan(&complete)
	if err != nil {
		t.Fatal(err)
	}
	if complete != 2 {
		t.Fatalf("first import must persist quota, limits, expiry and collection time: complete=%d/2", complete)
	}
}

// 畸形限流值让真数据库参数编码失败；验证写入失败不能报作完整导入。
func TestImportFetchedKeysReportsMetadataWriteFailure(t *testing.T) {
	access, session := cookieAccessFixture(t)
	ctx := context.Background()
	conn, release, err := access.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() {
		if _, err := conn.Exec(ctx, `DELETE FROM upstream_keys WHERE account_id=$1`, session.AccountID); err != nil {
			t.Error(err)
		}
	}()
	key := collector.Key{KeyRef: "metadata-failure", Secret: "test-only-metadata-failure",
		RateLimit: collector.RateLimit{RPM: 1 << 40}, Meta: collector.SourceMeta{FetchedAt: time.Now()}}
	runner := &Runner{}
	result, err := runner.importFetchedKeys(ctx, conn, store.Channel{ID: session.ChannelID},
		collector.Credential{AccountID: session.AccountID}, session, fixedKeyResolver{}, []collector.Key{key}, nil)
	if err != nil || result.Imported != 0 || result.Failed != 1 {
		t.Fatalf("metadata failure must not report a complete import: result=%+v err=%v", result, err)
	}
}
