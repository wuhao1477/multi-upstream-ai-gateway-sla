package collection

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 行为测试：真站点不能按需改换用户 ID 头名或在请求间使令牌失效。
// 成功信封复用 cookieAccessSelf 的实测形态，不作为其他二开站接入成功的证据。
func TestCookieValidationDiscoversUserIDHeader(t *testing.T) {
	a, s := cookieIdentityFixture(t)
	ctx := context.Background()
	calls := 0
	a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
		calls++
		cred, err := a.load(ctx, s.AccountID)
		if err != nil || cred.State != "unverified" || cred.ExternalUserID != "" {
			t.Fatal("a rejected header must not expire the Cookie or bind an unverified identity")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") == "" {
			t.Fatal("header discovery must use only the registered Cookie")
		}
		if r.Header.Get("Rix-Api-User") != "1663" {
			return cookieAccessResponse(r, 401, ""), nil
		}
		return cookieAccessResponse(r, 200, cookieAccessSelf), nil
	})
	if err := a.Validate(ctx, s.AccountID); err != nil {
		t.Fatalf("explicit Cookie validation must discover the site's user ID header: %v", err)
	}
	cred, err := a.load(ctx, s.AccountID)
	if err != nil || calls != 6 || cred.State != "ready" || cred.ExternalUserID != "1663" {
		t.Fatalf("header discovery must verify and bind the Cookie identity: calls=%d err=%v", calls, err)
	}
}

func TestCookieCollectionReusesDiscoveredHeader(t *testing.T) {
	for _, tc := range []struct {
		name, token        string
		expiresLater       bool
		cookieCalls, calls int
	}{
		{"cookie_only", "", false, 4, 0},
		{"expired_token", "test-only-stale", false, 4, 7},
		{"token_expires_during_collection", "test-only-live", true, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, initial := cookieAccessFixture(t)
			ctx := context.Background()
			cookieCalls, tokenCalls := 0, 0
			a.Client.HC = &http.Client{Transport: cookieWire(func(r *http.Request) (*http.Response, error) {
				tokenCalls++
				if tc.expiresLater && tokenCalls == 1 {
					return cookieAccessResponse(r, 200, cookieAccessSelf), nil
				}
				return cookieAccessResponse(r, 401, ""), nil
			})}
			a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
				cookieCalls++
				if r.Header.Get("Veloera-User") != "1663" {
					return cookieAccessResponse(r, 401, ""), nil
				}
				return cookieAccessResponse(r, 200, cookieAccessSelf), nil
			})
			adapter := collector.NewNewAPIAdapter(a.Client)
			s, err := adapter.Authenticate(ctx, collector.Credential{
				AccountID: initial.AccountID, ChannelID: initial.ChannelID, BaseURL: initial.BaseURL,
				ExternalUserID: initial.ExternalUserID, QuotaPerUnit: 500000, AccessToken: tc.token, CookieEnabled: true,
			})
			if err != nil {
				t.Fatalf("Cookie authentication failed to discover the header: %v", err)
			}
			if !tc.expiresLater && s.UserIDHeader != "Veloera-User" {
				t.Fatal("authenticated session must expose the verified Cookie header")
			}
			for range 2 {
				if _, err := adapter.FetchAccount(ctx, s); err != nil {
					t.Fatalf("account collection must reuse the discovered header: %v", err)
				}
			}
			if cookieCalls != tc.cookieCalls || tokenCalls != tc.calls {
				t.Fatalf("discovery must run once per session: Cookie=%d token=%d", cookieCalls, tokenCalls)
			}
		})
	}
}

func TestCookieHeaderDiscoveryStopsAfterReplacement(t *testing.T) {
	a, s := cookieAccessFixture(t)
	ctx := context.Background()
	requests, waits := 0, 0
	a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
		requests++
		return cookieAccessResponse(r, 401, ""), nil
	})
	a.Client.MinInterval = time.Millisecond
	a.Client.WaitHost = func(ctx context.Context, _ string, _ time.Duration) error {
		waits++
		if waits != 2 {
			return nil
		}
		conn, release, err := a.Pool.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := a.Credentials.Save(ctx, tx, s.AccountID, store.SaveCookieCredentialInput{
			Enabled: true, CookieHeader: "session=replacement-test-only",
		}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	_, err := a.Do(cookieAccessRequest(t, s, "/api/user/self"), s)
	if !errors.Is(err, store.ErrCookieCredentialChanged) || requests != 1 || waits != 2 {
		t.Fatalf("discovery must stop before sending a replacement Cookie: requests=%d waits=%d err=%v", requests, waits, err)
	}
	cred, err := a.load(ctx, s.AccountID)
	if err != nil || cred.State != "unverified" || !s.CookieState.Revision.IsZero() {
		t.Fatal("failed discovery must not change the replacement Cookie state or verify its revision")
	}
}
