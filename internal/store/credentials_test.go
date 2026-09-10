package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

type rotatingRefresher struct{ calls atomic.Int32 }

func (r *rotatingRefresher) Refresh(
	_ context.Context, cred collector.Credential,
) (collector.Credential, error) {
	if r.calls.Add(1) != 1 {
		return cred, errors.New("旧 refresh_token 已被第一次刷新作废")
	}
	time.Sleep(50 * time.Millisecond)
	cred.AccessToken = "new-access"
	cred.RefreshToken = "new-refresh"
	cred.TokenExpiresAt = time.Now().Add(time.Hour)
	return cred, nil
}

func TestSub2APIRefreshIsSerializedAcrossInstancesByRefreshLockKey(t *testing.T) {
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

	const (
		base1   = "https://refresh-lock-1.example.invalid"
		base2   = "https://refresh-lock-2.example.invalid"
		lockKey = "refresh:sub2api:shared-account"
	)
	wipeRefreshLockTest(ctx, t, conn, base1, base2)
	defer wipeRefreshLockTest(context.Background(), t, conn, base1, base2)

	channels := make([]Channel, 2)
	accountIDs := make([]int64, 2)
	for i, base := range []string{base1, base2} {
		channels[i] = Channel{Name: "refresh-lock", SiteFamily: "sub2api", BaseURL: base}
		channels[i].ID, err = CreateChannel(ctx, conn, channels[i])
		if err != nil {
			t.Fatalf("建渠道: %v", err)
		}
		accountIDs[i], err = CreateAccount(ctx, conn, Account{
			ChannelID: channels[i].ID, ExternalUserID: "shared-account",
		})
		if err != nil {
			t.Fatalf("建账号: %v", err)
		}
		_, err = conn.Exec(ctx, `
INSERT INTO collector_credentials (
       account_id, channel_id, site_family, cred_type, access_token, refresh_token,
       token_expires_at, refresh_lock_key)
VALUES ($1,$2,'sub2api','sub2api_jwt','old-access','old-refresh',$3,$4)`,
			accountIDs[i], channels[i].ID, time.Now().Add(10*time.Second), lockKey)
		if err != nil {
			t.Fatalf("写凭证: %v", err)
		}
	}

	stores := []*CredentialStore{NewCredentialStore(pool), NewCredentialStore(pool)}
	auths := []*collector.Authenticator{
		collector.NewAuthenticator(stores[0]),
		collector.NewAuthenticator(stores[1]),
	}
	creds := make([]collector.Credential, 2)
	for i := range channels {
		listed, listErr := stores[i].ListByChannel(ctx, conn, channels[i])
		err = listErr
		if err != nil {
			t.Fatalf("读凭证: %v", err)
		}
		creds[i] = listed[0]
	}

	refresher := &rotatingRefresher{}
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range auths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := auths[i].EnsureFresh(ctx, creds[i], refresher, time.Now())
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("并发刷新失败: %v", err)
		}
	}
	if got := refresher.calls.Load(); got != 1 {
		t.Errorf("两个实例刷新了 %d 次，期望共享 refresh_lock_key 后只刷新一次", got)
	}

	for _, ch := range channels {
		var access, refresh string
		if err := conn.QueryRow(ctx, `
SELECT access_token, refresh_token
  FROM collector_credentials WHERE channel_id=$1`, ch.ID).Scan(&access, &refresh); err != nil {
			t.Fatalf("读刷新结果: %v", err)
		}
		if access != "new-access" || refresh != "new-refresh" {
			t.Errorf("渠道 %d 仍是旧凭证: access=%q refresh=%q", ch.ID, access, refresh)
		}
	}
}

func TestSaveTxPreservesExistingRefreshLockKeyOnPartialUpdate(t *testing.T) {
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

	const base = "https://save-tx-lock-preserve.example.invalid"
	const wantLockKey = "refresh:sub2api:stable-account"
	wipeRefreshLockTest(ctx, t, conn, base)
	defer wipeRefreshLockTest(context.Background(), t, conn, base)

	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "save-tx-lock-preserve", SiteFamily: "sub2api", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	accountID, err := CreateAccount(ctx, conn, Account{
		ChannelID: channelID, ExternalUserID: "stable-account",
	})
	if err != nil {
		t.Fatalf("建账号: %v", err)
	}
	_, err = conn.Exec(ctx, `
INSERT INTO collector_credentials (
       account_id, channel_id, site_family, cred_type, access_token, refresh_token,
       token_expires_at, refresh_lock_key)
VALUES ($1,$2,'sub2api','sub2api_jwt','old-access','old-refresh',$3,$4)`,
		accountID, channelID, time.Now().Add(time.Hour), wantLockKey)
	if err != nil {
		t.Fatalf("写凭证: %v", err)
	}

	// This sparse write has no account identity or base URL. It must update the
	// token without replacing the stable lock domain with a fallback key.
	if err := (&CredentialStore{}).SaveTx(ctx, conn, collector.Credential{
		AccountID:   accountID,
		ChannelID:   channelID,
		Family:      collector.FamilySub2API,
		CredType:    "sub2api_jwt",
		AccessToken: "new-access",
	}); err != nil {
		t.Fatalf("部分更新凭证: %v", err)
	}

	var gotLockKey, access string
	if err := conn.QueryRow(ctx, `
SELECT refresh_lock_key, access_token
  FROM collector_credentials WHERE channel_id=$1`, channelID).
		Scan(&gotLockKey, &access); err != nil {
		t.Fatalf("读凭证: %v", err)
	}
	if gotLockKey != wantLockKey {
		t.Fatalf("部分更新改写了刷新锁键: got=%q want=%q", gotLockKey, wantLockKey)
	}
	if access != "new-access" {
		t.Fatalf("部分更新未写入 access_token: got=%q", access)
	}
}

func TestCredentialRefreshLockMigrationMatchesRuntimeURLNormalization(t *testing.T) {
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

	if _, err := conn.Exec(ctx, `
CREATE TEMP TABLE channels (id bigint PRIMARY KEY, base_url text NOT NULL);
CREATE TEMP TABLE collector_credentials (
  channel_id bigint PRIMARY KEY,
  site_family text NOT NULL,
  external_user_id text,
  refresh_lock_key text
);`); err != nil {
		t.Fatalf("建临时旧表: %v", err)
	}
	if _, err := conn.Exec(ctx, `
INSERT INTO channels VALUES (1, ' HTTPS://API.EXAMPLE.COM/CasePath/// ');
INSERT INTO collector_credentials VALUES (1, 'sub2api', ' 7500 ', NULL);`); err != nil {
		t.Fatalf("写存量凭证: %v", err)
	}

	var migrationSQL string
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Name == "021_credential_refresh_lock.sql" {
			migrationSQL = migration.SQL
			break
		}
	}
	if _, err := conn.Exec(ctx, migrationSQL); err != nil {
		t.Fatalf("执行 021: %v", err)
	}

	var got string
	if err := conn.QueryRow(ctx,
		`SELECT refresh_lock_key FROM collector_credentials WHERE channel_id=1`).Scan(&got); err != nil {
		t.Fatalf("读回锁键: %v", err)
	}
	const want = "refresh:sub2api:https://api.example.com/CasePath:7500"
	if got != want {
		t.Fatalf("迁移与运行时 URL 规范化不一致: got=%q want=%q", got, want)
	}
}

func TestCredentialsAreUniquePerAccount(t *testing.T) {
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

	const base = "https://credential-per-account.example.invalid"
	wipeCredentialAccountTest(ctx, t, conn, base)
	defer wipeCredentialAccountTest(context.Background(), t, conn, base)

	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "credential-per-account", SiteFamily: "newapi", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	account1, err := CreateAccount(ctx, conn, Account{
		ChannelID: channelID, ExternalUserID: "user-1",
	})
	if err != nil {
		t.Fatalf("建账号 1: %v", err)
	}
	account2, err := CreateAccount(ctx, conn, Account{
		ChannelID: channelID, ExternalUserID: "user-2",
	})
	if err != nil {
		t.Fatalf("建账号 2: %v", err)
	}

	credentials := &CredentialStore{}
	for _, cred := range []collector.Credential{
		{AccountID: account1, ChannelID: channelID, Family: collector.FamilyNewAPI,
			CredType: "newapi_access_token", AccessToken: "token-1", ExternalUserID: "user-1"},
		{AccountID: account2, ChannelID: channelID, Family: collector.FamilyNewAPI,
			CredType: "newapi_access_token", AccessToken: "token-2", ExternalUserID: "user-2"},
	} {
		if err := credentials.SaveTx(ctx, conn, cred); err != nil {
			t.Fatalf("保存账号 %d 凭证: %v", cred.AccountID, err)
		}
	}

	got, err := credentials.ListByChannel(ctx, conn, Channel{ID: channelID, BaseURL: base})
	if err != nil {
		t.Fatalf("列渠道凭证: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("凭证数 = %d，期望 2", len(got))
	}
	if got[0].AccountID != account1 || got[0].AccessToken != "token-1" ||
		got[1].AccountID != account2 || got[1].AccessToken != "token-2" {
		t.Fatalf("凭证未按账号保存: %+v", got)
	}
}

func TestSameExternalKeyRefUpdatesItsOwnAccount(t *testing.T) {
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

	const base = "https://same-key-ref-per-account.example.invalid"
	wipeCredentialAccountTest(ctx, t, conn, base)
	defer wipeCredentialAccountTest(context.Background(), t, conn, base)

	channelID, err := CreateChannel(ctx, conn, Channel{
		Name: "same-key-ref-per-account", SiteFamily: "newapi", BaseURL: base,
	})
	if err != nil {
		t.Fatalf("建渠道: %v", err)
	}
	account1, err := CreateAccount(ctx, conn, Account{ChannelID: channelID, ExternalUserID: "user-1"})
	if err != nil {
		t.Fatalf("建账号 1: %v", err)
	}
	account2, err := CreateAccount(ctx, conn, Account{ChannelID: channelID, ExternalUserID: "user-2"})
	if err != nil {
		t.Fatalf("建账号 2: %v", err)
	}
	key1, err := CreateKey(ctx, conn, account1, "secret-1", "1", nil)
	if err != nil {
		t.Fatalf("建 Key 1: %v", err)
	}
	key2, err := CreateKey(ctx, conn, account2, "secret-2", "1", nil)
	if err != nil {
		t.Fatalf("建 Key 2: %v", err)
	}

	sink := NewCollectorSink(pool)
	for accountID, quota := range map[int64]float64{account1: 11, account2: 22} {
		if err := sink.SaveKey(ctx, channelID, accountID, collector.Key{
			KeyRef: "1", RemainQuotaUSD: quotaPtr(quota),
			Meta: collector.SourceMeta{FetchedAt: time.Now()},
		}); err != nil {
			t.Fatalf("保存账号 %d 的 Key 用量: %v", accountID, err)
		}
	}

	for keyID, want := range map[int64]float64{key1: 11, key2: 22} {
		var got float64
		if err := conn.QueryRow(ctx,
			`SELECT remain_quota_usd FROM upstream_keys WHERE id=$1`, keyID).Scan(&got); err != nil {
			t.Fatalf("读 Key %d: %v", keyID, err)
		}
		if got != want {
			t.Fatalf("Key %d 额度 = %v，期望 %v", keyID, got, want)
		}
	}
}

func wipeCredentialAccountTest(
	ctx context.Context, t *testing.T, conn *pgx.Conn, base string,
) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM collector_snapshots WHERE channel_id IN (SELECT id FROM channels WHERE base_url=$1)`,
		`DELETE FROM collector_credentials WHERE channel_id IN (SELECT id FROM channels WHERE base_url=$1)`,
		`DELETE FROM upstream_keys WHERE account_id IN (
			SELECT a.id FROM upstream_accounts a JOIN channels c ON c.id=a.channel_id WHERE c.base_url=$1)`,
		`DELETE FROM upstream_accounts WHERE channel_id IN (SELECT id FROM channels WHERE base_url=$1)`,
		`DELETE FROM channels WHERE base_url=$1`,
	} {
		if _, err := conn.Exec(ctx, stmt, base); err != nil {
			t.Fatalf("清理账号凭证测试: %v", err)
		}
	}
}

func wipeRefreshLockTest(
	ctx context.Context, t *testing.T, conn *pgx.Conn, bases ...string,
) {
	t.Helper()
	if _, err := conn.Exec(ctx, `
DELETE FROM collector_credentials
 WHERE channel_id IN (SELECT id FROM channels WHERE base_url = ANY($1))`, bases); err != nil {
		t.Fatalf("清理刷新锁测试凭证: %v", err)
	}
	if _, err := conn.Exec(ctx, `
DELETE FROM upstream_accounts
 WHERE channel_id IN (SELECT id FROM channels WHERE base_url = ANY($1))`, bases); err != nil {
		t.Fatalf("清理刷新锁测试账号: %v", err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM channels WHERE base_url = ANY($1)`, bases); err != nil {
		t.Fatalf("清理刷新锁测试渠道: %v", err)
	}
}
