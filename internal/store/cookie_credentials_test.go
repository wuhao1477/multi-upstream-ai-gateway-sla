package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// 真 PG 验证部署后的存储边界，不读取任何已存凭据。
func TestCookieSchemaReplacesPasswords(t *testing.T) {
	conn, ctx := hubTestConn(t)
	var replaced bool
	err := conn.QueryRow(ctx, `SELECT to_regclass('collector_browser_credentials') IS NULL
AND to_regclass('collector_cookie_credentials') IS NOT NULL`).Scan(&replaced)
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("Cookie storage must replace the password table")
	}
}

func cookieTestStore(t *testing.T) *CookieCredentialStore {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := NewCookieCredentialStore(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Cookie-only 账号必须进入实际采集入口，而不能只在管理页显示已登记。
func TestCookieOnlyCredentialIsCollectable(t *testing.T) {
	conn, ctx, channel, id := cookieTestAccount(t)
	s := cookieTestStore(t)
	if _, err := saveCookieTest(t, s, conn, id, SaveCookieCredentialInput{
		Enabled: true, CookieHeader: "session=test-only",
	}); err != nil {
		t.Fatal(err)
	}
	ch := Channel{ID: channel}
	creds, err := (&CredentialStore{}).ListByChannel(ctx, conn, ch)
	if err != nil || len(creds) != 1 {
		t.Fatalf("Cookie-only account must be loaded: count=%d err=%v", len(creds), err)
	}
	if creds[0].AccountID != id || creds[0].ExternalUserID != "42" || creds[0].AccessToken != "" {
		t.Fatal("Cookie account identity or empty token changed")
	}
	if _, err := conn.Exec(ctx, `UPDATE collector_cookie_credentials SET enabled=false WHERE account_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := (&CredentialStore{}).ListByChannel(ctx, conn, ch); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled Cookie-only account must not be loaded")
	}
}

func cookieTestAccount(t *testing.T) (*pgx.Conn, context.Context, int64, int64) {
	t.Helper()
	conn, ctx := hubTestConn(t)
	channel := mkChannel(t, ctx, conn, "cookie-credentials", "https://cookie-credentials.example.invalid", "manual")
	id, err := CreateAccount(ctx, conn, Account{ChannelID: channel, ExternalUserID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	return conn, ctx, channel, id
}

func saveCookieTest(t *testing.T, s *CookieCredentialStore, conn *pgx.Conn, id int64, in SaveCookieCredentialInput) (bool, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	changed, err := s.Save(ctx, tx, id, in)
	if err != nil {
		return false, err
	}
	return changed, tx.Commit(ctx)
}

// Cookie 是安全边界输入；这些畸形输入不需要伪造上游协议。
func TestCookieHeaderValidation(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Cookie: session=test-only==; pref=light", "session=test-only==; pref=light"},
		{" session=first;session=second ", "session=first; session=second"},
		{"", ""}, {"session=x\r\nX-Test: 1", ""}, {"session=x\t", ""},
		{"not-a-pair", ""}, {"session=" + strings.Repeat("x", 16<<10), ""},
	} {
		got, err := normalizeCookieHeader(tc.input)
		if tc.want == "" {
			if err == nil {
				t.Error("invalid cookie header accepted")
			}
		} else if err != nil || got != tc.want {
			t.Error("cookie pairs were not preserved")
		}
	}
}

func TestCookieCredentialLifecycle(t *testing.T) {
	conn, ctx, channel, id := cookieTestAccount(t)
	s := cookieTestStore(t)
	if _, err := saveCookieTest(t, s, conn, id, SaveCookieCredentialInput{}); !errors.Is(err, ErrCookieCredentialInput) {
		t.Fatal("first save must require a cookie")
	}
	in := SaveCookieCredentialInput{Enabled: true, CookieHeader: "session=test-only-cookie==; pref=light"}
	if err := (&CredentialStore{}).SaveTx(ctx, conn, collector.Credential{
		AccountID: id, ChannelID: channel, Family: collector.FamilyNewAPI,
		CredType: "newapi_access_token", AccessToken: "test-only-token", ExternalUserID: "42",
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := saveCookieTest(t, s, conn, id, in); err != nil || !changed {
		t.Fatalf("first save: changed=%v err=%v", changed, err)
	}
	cred, err := s.Load(ctx, conn, id)
	if err != nil || cred.CookieHeader != in.CookieHeader {
		t.Fatal("cookie did not survive encryption")
	}
	var encrypted []byte
	if err := conn.QueryRow(ctx, `SELECT cookie_ciphertext FROM collector_cookie_credentials WHERE account_id=$1`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(in.CookieHeader)) {
		t.Fatal("plaintext cookie stored")
	}
	accounts, err := ListAccounts(ctx, conn, channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !accounts[0].CookieConfigured || !accounts[0].CookieEnabled || accounts[0].CookieState != "unverified" {
		t.Fatal("saving a cookie must not report it as verified")
	}
	public, _ := json.Marshal([]any{accounts, cred})
	if bytes.Contains(public, []byte(in.CookieHeader)) || bytes.Contains(public, []byte("ciphertext")) {
		t.Fatal("secret exposed")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := DeleteCookieCredential(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, conn, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted cookie remains usable")
	}
	if err := s.MarkState(ctx, conn, cred, "ready"); !errors.Is(err, ErrCookieCredentialChanged) {
		t.Fatal("deleted cookie was restored")
	}
	if differs, exists, err := CredentialTokenDiffers(ctx, conn, id, "test-only-token"); err != nil || !exists || differs {
		t.Fatal("cookie changes modified the original token")
	}
}

func TestCookieRevisionAndDisable(t *testing.T) {
	conn, ctx, _, id := cookieTestAccount(t)
	s := cookieTestStore(t)
	in := SaveCookieCredentialInput{Enabled: true, CookieHeader: "session=test-only-cookie"}
	if _, err := saveCookieTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	old, err := s.Load(ctx, conn, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkState(ctx, conn, old, "expired"); err != nil {
		t.Fatal(err)
	}
	if changed, err := saveCookieTest(t, s, conn, id, in); err != nil || changed {
		t.Fatal("same cookie changed revision")
	}
	same, err := s.Load(ctx, conn, id)
	if err != nil || same.State != "expired" || !same.UpdatedAt.Equal(old.UpdatedAt) {
		t.Fatal("same cookie reset expired state")
	}
	in.CookieHeader = ""
	if changed, err := saveCookieTest(t, s, conn, id, in); err != nil || changed {
		t.Fatal("empty cookie did not retain previous value")
	}
	in.Enabled = false
	if changed, err := saveCookieTest(t, s, conn, id, in); err != nil || !changed {
		t.Fatal("disable was not saved")
	}
	if _, err := s.Load(ctx, conn, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled cookie remains usable")
	}
	if err := s.MarkState(ctx, conn, old, "ready"); !errors.Is(err, ErrCookieCredentialChanged) {
		t.Fatal("stale task restored disabled cookie")
	}
	in.Enabled, in.CookieHeader = true, "session=test-only-new-cookie"
	if _, err := saveCookieTest(t, s, conn, id, in); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Load(ctx, conn, id)
	if err != nil || fresh.State != "unverified" || fresh.CookieHeader != in.CookieHeader {
		t.Fatal("replacement was not reset for validation")
	}
	if err := s.MarkState(ctx, conn, old, "ready"); !errors.Is(err, ErrCookieCredentialChanged) {
		t.Fatal("stale task accepted after replacement")
	}
}

func TestCookieCredentialsInvalidatedWithAccountOrSite(t *testing.T) {
	conn, ctx, channel, id := cookieTestAccount(t)
	s := cookieTestStore(t)
	if _, err := saveCookieTest(t, s, conn, id, SaveCookieCredentialInput{Enabled: true, CookieHeader: "session=test-only"}); err != nil {
		t.Fatal(err)
	}
	old, err := s.Load(ctx, conn, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE upstream_accounts SET external_user_id='43' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkState(ctx, conn, old, "ready"); !errors.Is(err, ErrCookieCredentialChanged) {
		t.Fatal("identity change accepted stale result")
	}
	changedURL := fmt.Sprintf("https://cookie-credentials-changed-%d.example.invalid", channel)
	t.Cleanup(func() { wipeChannelTree(context.Background(), conn, changedURL) })
	if _, err := conn.Exec(ctx, `UPDATE channels SET base_url=$2 WHERE id=$1`, channel, changedURL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, conn, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("old-site cookie remains usable")
	}
}

func TestCookieCipherRejectsTamperingAndCrossAccount(t *testing.T) {
	s := cookieTestStore(t)
	encrypted, err := s.seal(1, "https://example.com", []byte("test-only-cookie"))
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []struct {
		id     int64
		origin string
	}{{2, "https://example.com"}, {1, "https://other.example"}} {
		if _, err := s.open(binding.id, binding.origin, encrypted); err == nil {
			t.Fatal("wrong binding accepted")
		}
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := s.open(1, "https://example.com", encrypted); err == nil {
		t.Fatal("modified ciphertext accepted")
	}
}
