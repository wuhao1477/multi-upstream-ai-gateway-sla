package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func hubCookieAccount(t *testing.T, base, userID, cookie string) collector.HubAccount {
	t.Helper()
	raw := fmt.Sprintf(`{"accounts":{"accounts":[{"site_url":%q,"authType":"cookie",
"cookieAuth":{"sessionCookie":%q},"account_info":{"id":%q}}]}}`, base, cookie, userID)
	b, err := collector.ParseHubBackup(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return b.Accounts.Accounts[0]
}

func cookieImportDB(t *testing.T, base string) (*Server, *pgx.Conn, context.Context) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	wipe(ctx, t, conn, base)
	t.Cleanup(func() { wipe(ctx, t, conn, base) })
	s := testServer()
	s.CookieCredentials = adminCookieStore(t)
	s.SaveCredential = (&store.CredentialStore{}).SaveTx
	s.SaveDetected = (&store.CredentialStore{}).SaveDetected
	return s, conn, ctx
}

// 只验证真实 PG 的事务和凭据选择，不用给定探测结果举证站点协议。
func TestImportCookieLifecycle(t *testing.T) {
	const base = "https://cookie-import.example.invalid"
	s, conn, ctx := cookieImportDB(t, base)
	a := hubCookieAccount(t, base, "42", "session=first-test-cookie==")
	var first collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &first); err != nil {
		t.Fatal(err)
	}
	cred, err := s.CookieCredentials.Load(ctx, conn, first.AccountID)
	if err != nil || cred.CookieHeader != "session=first-test-cookie==" {
		t.Fatal("import did not save Cookie authentication")
	}
	if err := s.CookieCredentials.MarkState(ctx, conn, cred, "expired"); err != nil {
		t.Fatal(err)
	}
	var same collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &same); err != nil {
		t.Fatal(err)
	}
	retained, err := s.CookieCredentials.Load(ctx, conn, first.AccountID)
	if err != nil || same.Status != "unchanged" || retained.State != "expired" || !retained.UpdatedAt.Equal(cred.UpdatedAt) {
		t.Fatal("same Cookie reset state or revision")
	}
	a = hubCookieAccount(t, base, "42", "session=second-test-cookie")
	var updated collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status != "updated" {
		t.Fatal("changed Cookie was not reported")
	}
	a = hubCookieAccount(t, base, "42", "")
	var missing collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &missing); err != nil {
		t.Fatal(err)
	}
	retained, err = s.CookieCredentials.Load(ctx, conn, first.AccountID)
	if err != nil || retained.CookieHeader != "session=second-test-cookie" {
		t.Fatal("absent Cookie cleared the existing value")
	}
	public, _ := json.Marshal([]collector.HubImportItem{first, same, updated, missing})
	if strings.Contains(string(public), "test-cookie") {
		t.Fatal("Cookie exposed in import result")
	}
}

func TestImportCookieFailureIsAtomic(t *testing.T) {
	const base = "https://cookie-import-failure.example.invalid"
	s, conn, ctx := cookieImportDB(t, base)
	s.CookieCredentials = nil
	a := hubCookieAccount(t, base, "42", "session=test-only")
	var item collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &item); err == nil {
		t.Fatal("missing encryption key accepted")
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM channels WHERE base_url=$1`, base).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed Cookie import left a partial channel")
	}
}

func TestImportCookieAccountIsolationAndDisable(t *testing.T) {
	const base = "https://cookie-import-accounts.example.invalid"
	s, conn, ctx := cookieImportDB(t, base)
	a := hubCookieAccount(t, base, "42", "session=first-account")
	a.Disabled = true
	var first, second collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &first); err != nil {
		t.Fatal(err)
	}
	b := hubCookieAccount(t, base, "43", "session=second-account")
	if err := s.importOne(ctx, conn, b, detected(), &second); err != nil {
		t.Fatal(err)
	}
	if first.ChannelID != second.ChannelID || first.AccountID == second.AccountID {
		t.Fatal("same-site accounts were combined")
	}
	if err := s.SaveCredential(ctx, conn, collector.Credential{
		AccountID: first.AccountID, ChannelID: first.ChannelID, Family: collector.FamilyNewAPI,
		CredType: "newapi_access_token", AccessToken: "preserved-test-token", ExternalUserID: "42",
	}); err != nil {
		t.Fatal(err)
	}
	a = hubCookieAccount(t, base, "42", "session=replacement-cookie")
	var updated collector.HubImportItem
	if err := s.importOne(ctx, conn, a, detected(), &updated); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CookieCredentials.Load(ctx, conn, first.AccountID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("reimport enabled a disabled Cookie")
	}
	other, err := s.CookieCredentials.Load(ctx, conn, second.AccountID)
	if err != nil || other.CookieHeader != "session=second-account" {
		t.Fatal("another account's Cookie was changed")
	}
	var token string
	if err := conn.QueryRow(ctx, `SELECT access_token FROM collector_credentials WHERE account_id=$1`, first.AccountID).Scan(&token); err != nil || token != "preserved-test-token" {
		t.Fatal("Cookie import overwrote the token")
	}
	a = hubCookieAccount(t, base, "", "session=missing-identity")
	if err := s.importOne(ctx, conn, a, detected(), &collector.HubImportItem{}); err == nil {
		t.Fatal("missing ID bound Cookie to an arbitrary account")
	}
	// 导出里没有 Cookie 的条目与没令牌的条目同样处理：账号照建，不凭空登记。
	c := hubCookieAccount(t, base, "44", "")
	var noCookie collector.HubImportItem
	if err := s.importOne(ctx, conn, c, detected(), &noCookie); err != nil || noCookie.AccountID == 0 {
		t.Fatalf("Cookie-less entry must still import its account: %v", err)
	}
	if _, err := s.CookieCredentials.Load(ctx, conn, noCookie.AccountID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("Cookie-less entry registered a Cookie")
	}
}

func TestImportCookieRepairAndPreview(t *testing.T) {
	const base = "https://cookie-import-repair.example.invalid"
	s, conn, ctx := cookieImportDB(t, base)
	channel, err := store.CreateChannel(ctx, conn, store.Channel{Name: "cookie repair", BaseURL: base, SiteFamily: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channel, ExternalUserID: "42"}); err != nil {
		t.Fatal(err)
	}
	a := hubCookieAccount(t, base, "42", "session=repair-cookie")
	var preview, imported collector.HubImportItem
	if err := s.previewCookieImport(ctx, conn, a, detected(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Status != "would_import" {
		t.Fatal("preview did not identify incomplete account")
	}
	if err := s.importOne(ctx, conn, a, detected(), &imported); err != nil {
		t.Fatal(err)
	}
	cred, err := s.CookieCredentials.Load(ctx, conn, imported.AccountID)
	if err != nil || cred.CookieHeader != "session=repair-cookie" {
		t.Fatal("family repair lost the Cookie")
	}
	if err := s.CookieCredentials.MarkState(ctx, conn, cred, "expired"); err != nil {
		t.Fatal(err)
	}
	preview = collector.HubImportItem{}
	if err := s.previewCookieImport(ctx, conn, a, detected(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Status != "unchanged" {
		t.Fatal("same Cookie preview was not unchanged")
	}
	a = hubCookieAccount(t, base, "42", "session=changed-cookie")
	preview = collector.HubImportItem{}
	if err := s.previewCookieImport(ctx, conn, a, detected(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Status != "updated" {
		t.Fatal("changed Cookie was not detected")
	}
	retained, err := s.CookieCredentials.Load(ctx, conn, imported.AccountID)
	if err != nil || retained.CookieHeader != "session=repair-cookie" || retained.State != "expired" || !retained.UpdatedAt.Equal(cred.UpdatedAt) {
		t.Fatal("preview changed stored Cookie, state or revision")
	}
}

// 探测结果作为受控输入，只验 dry_run 的副作用边界；不举证上游协议。
func TestImportCookieDryRunDoesNotWriteOrReadKeys(t *testing.T) {
	const base = "https://cookie-import-preview.example.invalid"
	s, conn, ctx := cookieImportDB(t, base)
	pool, err := store.NewPool(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s.DB = pool
	s.Detect = func(context.Context, string) (collector.DetectResult, error) { return detected(), nil }
	s.ImportKeys = func(context.Context, *pgx.Conn, int64, int64, collector.KeyImportRequest) (collector.KeyImportResult, error) {
		t.Error("preview attempted authenticated Key access")
		return collector.KeyImportResult{}, errors.New("preview must not import keys")
	}
	a := hubCookieAccount(t, base, "42", "session=preview-secret")
	var backup collector.HubBackup
	backup.Accounts.Accounts = []collector.HubAccount{a}
	result, err := s.ImportHub(ctx, &backup, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || result.Failed != 0 {
		t.Fatal("Cookie preview failed")
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM channels WHERE base_url=$1`, base).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("preview created a channel")
	}
	public, _ := json.Marshal(result)
	if strings.Contains(string(public), "preview-secret") {
		t.Fatal("preview leaked Cookie")
	}
}
