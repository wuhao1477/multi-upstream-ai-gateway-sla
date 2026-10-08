package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 被测对象是管理路由的认证边界，不模拟上游登录。
func TestCookieCredentialsRequireAdmin(t *testing.T) {
	s := NewServer(nil, "test-admin", nil, nil)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/admin/accounts/1/cookie-credentials", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", method, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/1/cookie-credentials/validate", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("validate: want 401, got %d", rec.Code)
	}
}

// 验证路由只接受已存 Cookie 的账号 ID，不接收新的秘密或调用密码登录。
func TestCookieValidationRoute(t *testing.T) {
	s := NewServer(nil, "test-admin", nil, nil)
	s.CookieCredentials = adminCookieStore(t)
	calls := 0
	s.ValidateCookie = func(_ context.Context, id int64) error {
		calls++
		if id != 7 {
			t.Error("wrong account selected")
		}
		return collector.ErrCookieExpired
	}
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	for _, tc := range []struct {
		body     string
		readOnly bool
		want     int
	}{
		{"", false, 422}, {`{"cookie_header":"must-not-read"}`, false, 400}, {"", true, 409},
	} {
		s.ReadOnly = tc.readOnly
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/7/cookie-credentials/validate", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer test-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("want %d got %d", tc.want, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "must-not-read") {
			t.Error("request secret exposed")
		}
	}
	if calls != 1 {
		t.Errorf("only explicit stored-cookie validation may run: calls=%d", calls)
	}
}

func adminCookieStore(t *testing.T) *store.CookieCredentialStore {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := store.NewCookieCredentialStore(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCookieCredentialAPI(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, release, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	const base = "https://cookie-api.example.invalid"
	wipe(ctx, t, conn, base)
	defer wipe(ctx, t, conn, base)
	channel, err := store.CreateChannel(ctx, conn, store.Channel{Name: "cookie-api-test", BaseURL: base, SiteFamily: "newapi"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channel, ExternalUserID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(pool, "test-admin", nil, nil)
	s.CookieCredentials = adminCookieStore(t)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	path := fmt.Sprintf("/admin/accounts/%d/cookie-credentials", id)
	request := func(method, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s: want %d, got %d", method, want, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "test-cookie-unique") {
			t.Fatal("cookie exposed")
		}
	}
	request(http.MethodPut, `{"enabled":true}`, 400)
	request(http.MethodPut, `{"enabled":true,"cookie_header":"session=test-cookie-unique=="}`, 200)
	request(http.MethodPut, `{"enabled":false}`, 200)
	request(http.MethodPut, `{"username":"alice","password":"test-only"}`, 400)
	request(http.MethodPut, `{"cookie_header":"session=x\r\nX-Test:1"}`, 400)
	request(http.MethodPut, `{} {}`, 400)
	accounts, err := store.ListAccounts(ctx, conn, channel)
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(accounts)
	if strings.Contains(string(serialized), "test-cookie-unique") {
		t.Fatal("cookie exposed in account list")
	}
	s.ReadOnly = true
	request(http.MethodDelete, "", 409)
	s.ReadOnly = false
	s.CookieCredentials = nil
	request(http.MethodDelete, "", 200)
	request(http.MethodDelete, "", 200)
	path = fmt.Sprintf("/admin/accounts/%d/browser-credentials", id)
	request(http.MethodPut, `{"username":"alice","password":"test-only"}`, 404)
}

func TestCookieCredentialsRejectUnconfiguredEncryption(t *testing.T) {
	s := NewServer(nil, "test-admin", nil, nil)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	req := httptest.NewRequest(http.MethodPut, "/admin/accounts/1/cookie-credentials",
		strings.NewReader(`{"enabled":true,"cookie_header":"session=test-only-secret"}`))
	req.Header.Set("Authorization", "Bearer test-admin")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "test-only-secret") {
		t.Fatal("cookie exposed in response")
	}
}
