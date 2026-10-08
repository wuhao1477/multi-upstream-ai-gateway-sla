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

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 被测对象是本项目的管理路由，不是模拟上游站点。
func TestBrowserCredentialsRequireAdmin(t *testing.T) {
	s := NewServer(nil, "test-admin", nil, nil)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/admin/accounts/1/browser-credentials", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", method, rec.Code)
		}
	}
}

func TestBrowserCredentialAPI(t *testing.T) {
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
	const base = "https://browser-api.example.invalid"
	wipe(ctx, t, conn, base)
	defer wipe(ctx, t, conn, base)
	channel, err := store.CreateChannel(ctx, conn, store.Channel{Name: "browser-api-test", BaseURL: base, SiteFamily: "newapi"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channel, ExternalUserID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s := NewServer(pool, "test-admin", nil, nil)
	s.BrowserCredentials, err = store.NewBrowserCredentialStore(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	path := fmt.Sprintf("/admin/accounts/%d/browser-credentials", id)
	request := func(method, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s: want %d, got %d", method, want, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "test-password-unique") {
			t.Fatal("password exposed")
		}
	}
	request(http.MethodPut, `{"enabled":true,"username":"alice"}`, 400)
	request(http.MethodPut, `{"enabled":true,"username":"alice","password":"test-password-unique"}`, 200)
	request(http.MethodPut, `{"enabled":true,"username":"bob"}`, 400)
	request(http.MethodPut, `{"enabled":false,"username":"alice"}`, 200)
	request(http.MethodPut, `{"password":"test-password-unique","unexpected":true}`, 400)
	request(http.MethodPut, `{} {}`, 400)
	accounts, err := store.ListAccounts(ctx, conn, channel)
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(accounts)
	if strings.Contains(string(serialized), "test-password-unique") {
		t.Fatal("password exposed in account list")
	}
	s.ReadOnly = true
	request(http.MethodDelete, "", 409)
	s.ReadOnly = false
	s.BrowserCredentials = nil
	request(http.MethodDelete, "", 200)
	request(http.MethodDelete, "", 200) // 清除幂等，并且无需部署密钥。
}

func TestBrowserCredentialsRejectUnconfiguredEncryption(t *testing.T) {
	s := NewServer(nil, "test-admin", nil, nil)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	req := httptest.NewRequest(http.MethodPut, "/admin/accounts/1/browser-credentials",
		strings.NewReader(`{"enabled":true,"username":"test","password":"test-only-secret"}`))
	req.Header.Set("Authorization", "Bearer test-admin")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing encryption key: want 503, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "test-only-secret") {
		t.Fatal("password exposed in response")
	}
}
