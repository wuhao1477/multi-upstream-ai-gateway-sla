package store

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func credentialTestConn(t *testing.T) (context.Context, *pgx.Conn) {
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
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return ctx, conn
}

func TestKeySecretEncryptedAtRest(t *testing.T) {
	ctx, conn := credentialTestConn(t)
	const base = "https://key-secret-at-rest.example.invalid"
	wipeInventoryTest(ctx, t, conn, base)
	t.Cleanup(func() { wipeInventoryTest(context.Background(), t, conn, base) })
	channelID, err := CreateChannel(ctx, conn, Channel{Name: "key-at-rest", SiteFamily: "newapi", BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := CreateAccount(ctx, conn, Account{ChannelID: channelID})
	if err != nil {
		t.Fatal(err)
	}

	read := func(id int64) (string, string) {
		t.Helper()
		var sealed []byte
		var prefix string
		if err := conn.QueryRow(ctx, `SELECT secret_ciphertext, secret_prefix FROM upstream_keys WHERE id=$1`,
			id).Scan(&sealed, &prefix); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(sealed, []byte("key-at-rest-marker")) {
			t.Fatal("key ciphertext contains plaintext")
		}
		plain, err := openCredential("upstream_keys", id, "secret", sealed)
		if err != nil {
			t.Fatal(err)
		}
		return plain, prefix
	}
	id, err := CreateKey(ctx, conn, accountID, "sk-key-at-rest-marker-1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain, prefix := read(id); plain != "sk-key-at-rest-marker-1" || prefix != "sk-key-a" {
		t.Fatalf("created key = %q / %q", plain, prefix)
	}
	keys, err := ListKeys(ctx, conn, channelID)
	if err != nil || len(keys) != 1 || keys[0].SecretPrefix != "sk-key-a…" {
		t.Fatalf("list = %+v, %v", keys, err)
	}

	next := "rk-key-at-rest-marker-2"
	if err := UpdateKey(ctx, conn, KeyPatch{ID: id, Secret: &next}); err != nil {
		t.Fatal(err)
	}
	if plain, prefix := read(id); plain != next || prefix != "rk-key-a" {
		t.Fatalf("updated key = %q / %q", plain, prefix)
	}
	status := "revoked"
	if err := UpdateKey(ctx, conn, KeyPatch{ID: id, Status: &status}); err != nil {
		t.Fatal(err)
	}
	if plain, _ := read(id); plain != next {
		t.Fatalf("non-secret update changed the secret: %q", plain)
	}
}

// 界面只拿到去掉认证信息的展示值并原样提交：不能用它顶掉真实地址。
func TestHubSyncURLDisplayRoundTripKeepsRealURL(t *testing.T) {
	ctx, conn := credentialTestConn(t)
	original, err := LoadHubSyncConfig(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = SaveHubSyncConfig(context.Background(), conn, original)
		_, _ = conn.Exec(context.Background(), `UPDATE hub_sync_config
   SET webdav_password_ciphertext = CASE WHEN $1 THEN NULL ELSE webdav_password_ciphertext END,
       backup_password_ciphertext = CASE WHEN $2 THEN NULL ELSE backup_password_ciphertext END
 WHERE id=1`, original.WebDAVPassword == "", original.BackupPassword == "")
	})

	const real = "https://dav.example.invalid/dav/?token=hub-url-marker"
	save := func(url, password string, interval int) HubSyncConfig {
		t.Helper()
		if err := SaveHubSyncConfig(ctx, conn, HubSyncConfig{
			WebDAVURL: url, WebDAVPassword: password, IntervalMinutes: interval, ApplyMode: HubSyncModeReport,
		}); err != nil {
			t.Fatal(err)
		}
		got, err := LoadHubSyncConfig(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := save(real, "hub-password-marker", 30)
	if got.WebDAVURL != real || got.WebDAVURLDisplay != "https://dav.example.invalid/dav/" ||
		got.WebDAVPassword != "hub-password-marker" {
		t.Fatalf("saved = %+v", got)
	}
	got = save(got.WebDAVURLDisplay, "", 60)
	if got.WebDAVURL != real || got.WebDAVPassword != "hub-password-marker" || got.IntervalMinutes != 60 {
		t.Fatalf("display round trip changed secrets: %+v", got)
	}
	got = save("https://dav2.example.invalid/backup.json", "", 60)
	if got.WebDAVURL != "https://dav2.example.invalid/backup.json" ||
		got.WebDAVURLDisplay != got.WebDAVURL {
		t.Fatalf("new URL not saved: %+v", got)
	}
	if got = save("", "", 60); got.WebDAVURL != "" || got.WebDAVURLDisplay != "" {
		t.Fatalf("empty URL not cleared: %+v", got)
	}
	var leaked bool
	if err := conn.QueryRow(ctx, `SELECT h::text LIKE '%marker%' FROM hub_sync_config h WHERE id=1`).
		Scan(&leaked); err != nil || leaked {
		t.Fatalf("plaintext marker stored: %v %v", leaked, err)
	}
}
