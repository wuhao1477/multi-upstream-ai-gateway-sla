package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

func browserTestStore(t *testing.T) *BrowserCredentialStore {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := NewBrowserCredentialStore(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func browserTestAccount(t *testing.T) (*pgx.Conn, context.Context, int64, int64) {
	t.Helper()
	conn, ctx := hubTestConn(t)
	id := mkChannel(t, ctx, conn, "browser-credentials", "https://browser-credentials.example.invalid", "manual")
	account, err := CreateAccount(ctx, conn, Account{ChannelID: id, ExternalUserID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	return conn, ctx, id, account
}

func saveBrowserTest(t *testing.T, s *BrowserCredentialStore, conn *pgx.Conn, id int64, in SaveBrowserCredentialInput) error {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.Save(ctx, tx, id, in); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// 真 PG 验证保存/保留/清除；没有伪造站点协议或对外发请求。
func TestBrowserCredentialLifecycle(t *testing.T) {
	conn, ctx, channel, id := browserTestAccount(t)
	s := browserTestStore(t)
	in := SaveBrowserCredentialInput{Enabled: true, Username: "alice", Password: " test-only-password "}
	if err := (&CredentialStore{}).SaveTx(ctx, conn, collector.Credential{
		AccountID: id, ChannelID: channel, Family: collector.FamilyNewAPI,
		CredType: "newapi_access_token", AccessToken: "test-only-token", ExternalUserID: "42",
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveBrowserTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	cred, err := s.Load(ctx, conn, id)
	if err != nil || cred.Password != in.Password {
		t.Fatal("password did not survive encryption")
	}
	var encrypted []byte
	if err := conn.QueryRow(ctx, `SELECT password_ciphertext FROM collector_browser_credentials WHERE account_id=$1`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(in.Password)) {
		t.Fatal("plaintext password stored")
	}
	accounts, err := ListAccounts(ctx, conn, channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !accounts[0].BrowserConfigured || !accounts[0].BrowserEnabled || accounts[0].BrowserState != "unverified" {
		t.Fatal("saved configuration must not be reported as a verified login")
	}
	public, _ := json.Marshal(accounts)
	if bytes.Contains(public, []byte(in.Password)) || bytes.Contains(public, []byte("ciphertext")) {
		t.Fatal("secrets exposed")
	}
	in.Password = ""
	if err := saveBrowserTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	retained, err := s.Load(ctx, conn, id)
	if err != nil || retained.Password != cred.Password {
		t.Fatal("empty password must retain previous value")
	}
	in.Username = "bob"
	if err := saveBrowserTest(t, s, conn, id, in); !errors.Is(err, ErrBrowserCredentialInput) {
		t.Fatal("changing username must require a password")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := DeleteBrowserCredential(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, conn, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted credentials still usable")
	}
	var token string
	if err := conn.QueryRow(ctx, `SELECT access_token FROM collector_credentials WHERE account_id=$1`, id).Scan(&token); err != nil || token != "test-only-token" {
		t.Fatal("browser credential changes modified the original token")
	}
}

func TestBrowserSessionRevisionAndDisable(t *testing.T) {
	conn, ctx, _, id := browserTestAccount(t)
	s := browserTestStore(t)
	in := SaveBrowserCredentialInput{Enabled: true, Username: "alice", Password: "test-only-password"}
	if err := saveBrowserTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	old, err := s.Load(ctx, conn, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(ctx, conn, old, []byte(`{"test":"session"}`), "ready"); err != nil {
		t.Fatal(err)
	}
	restored, err := s.Load(ctx, conn, id)
	if err != nil || !bytes.Equal(restored.Session, []byte(`{"test":"session"}`)) {
		t.Fatal("encrypted session did not survive storage")
	}
	in.Enabled, in.Password = false, ""
	if err := saveBrowserTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(ctx, conn, old, []byte("old-session"), "ready"); !errors.Is(err, ErrBrowserCredentialChanged) {
		t.Fatal("old task restored a revoked session")
	}
	if _, err := s.Load(ctx, conn, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled credentials remain usable")
	}
	in.Enabled = true
	if err := saveBrowserTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Load(ctx, conn, id)
	if err != nil || len(fresh.Session) != 0 {
		t.Fatal("disabled session was restored")
	}
	if err := s.SaveSession(ctx, conn, old, []byte("old-session"), "ready"); !errors.Is(err, ErrBrowserCredentialChanged) {
		t.Fatal("revision was not invalidated")
	}
}

func TestBrowserCredentialsInvalidatedWithAccountOrSite(t *testing.T) {
	conn, ctx, channel, id := browserTestAccount(t)
	s := browserTestStore(t)
	in := SaveBrowserCredentialInput{Enabled: true, Username: "alice", Password: "test-only-password"}
	if err := saveBrowserTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	old, err := s.Load(ctx, conn, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE upstream_accounts SET status='disabled' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE upstream_accounts SET status='active' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(ctx, conn, old, []byte("revoked"), "ready"); !errors.Is(err, ErrBrowserCredentialChanged) {
		t.Fatal("disable/reactivate allowed stale session write")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := UpdateChannel(ctx, tx, Channel{ID: channel, BaseURL: "https://changed.example.invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, tx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("old-site password remains usable on a different site")
	}
}

// 密文篡改与跨账号重放是安全边界输入，不是伪造上游依赖。
func TestBrowserCipherRejectsTamperingAndCrossAccount(t *testing.T) {
	s := browserTestStore(t)
	ciphertext, err := s.seal(1, "https://example.com", "password", []byte("test-only-secret"))
	if err != nil {
		t.Fatal(err)
	}
	for _, aad := range []struct {
		id              int64
		origin, purpose string
	}{
		{2, "https://example.com", "password"}, {1, "https://other.example", "password"}, {1, "https://example.com", "session"},
	} {
		if _, err := s.open(aad.id, aad.origin, aad.purpose, ciphertext); err == nil {
			t.Fatal("wrong binding accepted")
		}
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := s.open(1, "https://example.com", "password", ciphertext); err == nil {
		t.Fatal("modified ciphertext accepted")
	}
}
