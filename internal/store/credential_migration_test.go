package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 在独立 schema 里从空库迁到 034，写入旧版明文，再模拟升级到 035。
// 被测对象是迁移本身；数据都是合成标记，不是任何真实凭证。
func TestPlaintextCredentialsMigrateToCiphertext(t *testing.T) {
	ctx, conn := migrationSchema(t)
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	cut := len(ms)
	for i, m := range ms {
		if m.Name == "035_drop_plaintext_credentials.sql" {
			cut = i
		}
	}
	if cut == len(ms) {
		t.Fatal("035 迁移不存在")
	}
	if err := applyMigrations(ctx, conn, nil, ms[:cut]); err != nil {
		t.Fatal(err)
	}
	ids := seedPlaintextCredentials(ctx, t, conn)

	// 有明文而缺密钥：035 整体回滚，明文保留，进程应拒绝启动。
	useCredentialKey(t, "")
	err = applyMigrations(ctx, conn, nil, ms)
	if err == nil || !strings.Contains(err.Error(), "SLA_CREDENTIAL_SECRET_KEY") {
		t.Fatalf("migration without key = %v", err)
	}
	var secret string
	if err := conn.QueryRow(ctx, `SELECT secret FROM upstream_keys WHERE id=$1`, ids.key).Scan(&secret); err != nil ||
		secret != "sk-legacy-key-marker" {
		t.Fatalf("failed migration lost plaintext: %q, %v", secret, err)
	}
	var applied bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = '035_drop_plaintext_credentials.sql')`).
		Scan(&applied); err != nil || applied {
		t.Fatalf("failed 035 recorded as applied: %v %v", applied, err)
	}

	useCredentialKey(t, testCredentialKey)
	if err := applyMigrations(ctx, conn, nil, ms); err != nil {
		t.Fatal(err)
	}
	var plaintextColumns int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema = current_schema()
   AND (table_name, column_name) IN (('upstream_keys','secret'),
        ('collector_credentials','access_token'), ('collector_credentials','refresh_token'),
        ('hub_sync_config','webdav_url'), ('hub_sync_config','webdav_password'),
        ('hub_sync_config','backup_password'))`).Scan(&plaintextColumns); err != nil || plaintextColumns != 0 {
		t.Fatalf("plaintext columns remain: %d %v", plaintextColumns, err)
	}

	var keyCipher []byte
	var prefix string
	if err := conn.QueryRow(ctx, `SELECT secret_ciphertext, secret_prefix FROM upstream_keys WHERE id=$1`,
		ids.key).Scan(&keyCipher, &prefix); err != nil {
		t.Fatal(err)
	}
	if got, err := openCredential("upstream_keys", ids.key, "secret", keyCipher); err != nil ||
		got != "sk-legacy-key-marker" || prefix != "sk-legac" {
		t.Fatalf("key = %q prefix=%q err=%v", got, prefix, err)
	}
	store := &CredentialStore{}
	for _, tc := range []struct {
		account int64
		token   string
	}{{ids.sub2api, "legacy-access-marker"}, {ids.newapi, "legacy-newapi-marker"}} {
		if differs, exists, err := CredentialTokenDiffers(ctx, conn, tc.account, tc.token); err != nil || !exists || differs {
			t.Fatalf("account %d token lost: differs=%v exists=%v err=%v", tc.account, differs, exists, err)
		}
	}
	cred, err := store.loadByAccount(ctx, conn, ids.sub2api, false)
	if err != nil || cred.RefreshToken != "legacy-refresh-marker" {
		t.Fatalf("refresh token = %q, %v", cred.RefreshToken, err)
	}
	hub, err := LoadHubSyncConfig(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if hub.WebDAVURL != legacyWebDAVURL || hub.WebDAVPassword != "webdav-pass-marker" || hub.BackupPassword != "" ||
		hub.WebDAVURLDisplay != "https://dav.example.invalid/backup.json" {
		t.Fatalf("hub config = %+v", hub)
	}

	// 库里任何一列都不再有明文标记（含 bytea 的十六进制文本以外的形态）。
	var leaked bool
	if err := conn.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM upstream_keys k WHERE position($1::bytea in k.secret_ciphertext) > 0)
    OR EXISTS (SELECT 1 FROM collector_credentials c
                WHERE position($2::bytea in coalesce(c.access_token_ciphertext,'')) > 0)
    OR EXISTS (SELECT 1 FROM hub_sync_config h WHERE h::text LIKE '%marker%')`,
		[]byte("legacy-key-marker"), []byte("legacy-access-marker")).Scan(&leaked); err != nil || leaked {
		t.Fatalf("plaintext marker still stored: %v %v", leaked, err)
	}

	// 把一行的密文挪给另一行：AAD 绑定 account_id，读取必须明确失败。
	if _, err := conn.Exec(ctx, `
UPDATE collector_credentials SET access_token_ciphertext =
       (SELECT access_token_ciphertext FROM collector_credentials WHERE account_id=$1)
 WHERE account_id=$2`, ids.sub2api, ids.newapi); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CredentialTokenDiffers(ctx, conn, ids.newapi, "legacy-access-marker"); !errors.Is(err, ErrCredentialDecrypt) {
		t.Fatalf("swapped ciphertext accepted: %v", err)
	}
}

const legacyWebDAVURL = "https://user:url-pass-marker@dav.example.invalid/backup.json?token=url-token-marker"

type legacyIDs struct{ key, sub2api, newapi int64 }

func seedPlaintextCredentials(ctx context.Context, t *testing.T, conn *pgx.Conn) legacyIDs {
	t.Helper()
	var ids legacyIDs
	var channel int64
	if err := conn.QueryRow(ctx, `INSERT INTO channels (name, site_family, base_url)
VALUES ('legacy', 'sub2api', 'https://legacy.example.invalid') RETURNING id`).Scan(&channel); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	for _, dest := range []*int64{&ids.sub2api, &ids.newapi} {
		if err := conn.QueryRow(ctx, `INSERT INTO upstream_accounts (channel_id) VALUES ($1) RETURNING id`,
			channel).Scan(dest); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	if err := conn.QueryRow(ctx, `
INSERT INTO upstream_keys (account_id, secret, status) VALUES ($1, 'sk-legacy-key-marker', 'active') RETURNING id`,
		ids.sub2api).Scan(&ids.key); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO collector_credentials (account_id, channel_id, site_family, cred_type, access_token,
		  refresh_token, token_expires_at, refresh_lock_key)
		  VALUES ($1, $2, 'sub2api', 'sub2api_jwt', 'legacy-access-marker', 'legacy-refresh-marker', $3, 'lock')`,
			[]any{ids.sub2api, channel, time.Now().Add(time.Hour)}},
		{`INSERT INTO collector_credentials (account_id, channel_id, site_family, cred_type, access_token)
		  VALUES ($1, $2, 'newapi', 'newapi_access_token', 'legacy-newapi-marker')`,
			[]any{ids.newapi, channel}},
		{`UPDATE hub_sync_config SET webdav_url = $1, webdav_password = 'webdav-pass-marker' WHERE id = 1`,
			[]any{legacyWebDAVURL}},
	} {
		if _, err := conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed plaintext: %v", err)
		}
	}
	return ids
}

// migrationSchema 给用例一个独立 schema（search_path 指向它），结束时整个删掉。
func migrationSchema(t *testing.T) (context.Context, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连库: %v", err)
	}
	schema := fmt.Sprintf("migration_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema+"; SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(context.Background())
	})
	return ctx, conn
}
