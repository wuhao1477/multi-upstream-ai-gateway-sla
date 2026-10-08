package collection

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 按 NewAPI v1.0.0-rc.21 的默认 CookieStore / securecookie v1.1.1 格式构造，
// 随机签名密钥不属于任何站点；仅测试身份发现，不能作为真实认证成功的证据。
func identityTestCookie(t *testing.T, id any) string {
	t.Helper()
	var payload bytes.Buffer
	values := map[any]any{"id": id, "username": "identity-test", "role": 1, "status": 1, "group": "default"}
	if err := gob.NewEncoder(&payload).Encode(values); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	encoded := strconv.FormatInt(time.Now().Unix(), 10) + "|" + base64.URLEncoding.EncodeToString(payload.Bytes())
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("session|" + encoded))
	value := append([]byte(encoded+"|"), mac.Sum(nil)...)
	return "session=" + base64.URLEncoding.EncodeToString(value) + "; pref=light"
}

func TestCookieHeadersDiscoverUserID(t *testing.T) {
	for _, tc := range []struct {
		name, cookie, storedID, wantID string
	}{
		{"signed_session", identityTestCookie(t, 1663), "", "1663"},
		{"zero_id", identityTestCookie(t, 0), "", ""},
		{"wrong_id_type", identityTestCookie(t, "1663"), "", ""},
		{"opaque_session", "session=opaque-test-only", "", ""},
		{"duplicate_session", identityTestCookie(t, 1663) + "; " + identityTestCookie(t, 1664), "", ""},
		{"registered_identity", identityTestCookie(t, 1663), "42", "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers, err := cookieHeaders(store.CookieCredential{CookieHeader: tc.cookie, ExternalUserID: tc.storedID}, "")
			if tc.wantID == "" {
				if !errors.Is(err, collector.ErrCookieNeedsAction) {
					t.Fatal("unreadable identity must require action, not guess an ID")
				}
				return
			}
			if err != nil || headers.Get("New-API-User") != tc.wantID || headers.Get("Cookie") != tc.cookie {
				t.Fatalf("expected identity header %s without changing Cookie: %v", tc.wantID, err)
			}
		})
	}
}

func cookieIdentityFixture(t *testing.T) (*CookieAccess, collector.Session) {
	t.Helper()
	a, s := cookieAccessFixture(t)
	ctx := context.Background()
	conn, release, err := a.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE upstream_accounts SET external_user_id=NULL WHERE id=$1`, s.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Credentials.Save(ctx, tx, s.AccountID, store.SaveCookieCredentialInput{
		Enabled: true, CookieHeader: identityTestCookie(t, 1663),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	s.ExternalUserID = ""
	return a, s
}

func TestCookieValidationDiscoversIdentity(t *testing.T) {
	a, s := cookieIdentityFixture(t)
	ctx := context.Background()
	before, err := a.load(ctx, s.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
		calls++
		current, err := a.load(ctx, s.AccountID)
		if err != nil || current.ExternalUserID != "" || current.State != "unverified" {
			t.Fatal("identity must not be saved before the upstream verifies it")
		}
		if r.URL.Path != "/api/user/self" || r.Header.Get("New-API-User") != "1663" || r.Header.Get("Authorization") != "" {
			t.Fatal("identity must be verified with the original Cookie and candidate user ID")
		}
		return cookieAccessResponse(r, 200, cookieAccessSelf), nil
	})
	if err := a.Validate(ctx, s.AccountID); err != nil {
		t.Fatalf("Cookie without a registered user ID must be verifiable: %v", err)
	}
	after, err := a.load(ctx, s.AccountID)
	if err != nil || calls != 1 || after.ExternalUserID != "1663" || after.State != "ready" || after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("verified identity and new Cookie revision were not saved together: %v", err)
	}
}

func TestCookieCollectionDiscoversIdentity(t *testing.T) {
	a, original := cookieIdentityFixture(t)
	a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("New-API-User") != "1663" || r.Header.Get("Authorization") != "" {
			t.Fatal("collection lost the discovered identity or mixed credentials")
		}
		return cookieAccessResponse(r, 200, cookieAccessSelf), nil
	})
	adapter := collector.NewNewAPIAdapter(a.Client)
	session, err := adapter.Authenticate(context.Background(), collector.Credential{
		AccountID: original.AccountID, ChannelID: original.ChannelID, Family: original.Family,
		BaseURL: original.BaseURL, CookieEnabled: true, QuotaPerUnit: 500000,
	})
	if err != nil || session.ExternalUserID != "1663" {
		t.Fatalf("collection must discover a missing user ID: %v", err)
	}
	if _, err := adapter.FetchAccount(context.Background(), session); err != nil {
		t.Fatalf("the authenticated session must remain usable after saving the identity: %v", err)
	}
}

func TestCookieIdentityFailureDoesNotBindAccount(t *testing.T) {
	for _, tc := range []struct {
		name, body, state string
		status, calls     int
		replace           bool
	}{
		{"identity_mismatch", `{"success":true,"data":{"id":1664}}`, "needs_action", 200, 1, false},
		{"unauthorized", "", "expired", 401, 7, false},
		{"cookie_replaced", cookieAccessSelf, "unverified", 200, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s := cookieIdentityFixture(t)
			ctx := context.Background()
			calls := 0
			a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.replace {
					conn, release, err := a.Pool.Acquire(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					tx, err := conn.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(ctx) }()
					if _, err := a.Credentials.Save(ctx, tx, s.AccountID, store.SaveCookieCredentialInput{
						Enabled: true, CookieHeader: "session=replaced-during-verification",
					}); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				}
				return cookieAccessResponse(r, tc.status, tc.body), nil
			})
			if err := a.Validate(ctx, s.AccountID); err == nil || calls != tc.calls {
				t.Fatalf("failed verification must stay within header discovery: calls=%d want=%d err=%v", calls, tc.calls, err)
			}
			cred, err := a.load(ctx, s.AccountID)
			if err != nil || cred.ExternalUserID != "" || cred.State != tc.state {
				t.Fatalf("failed/stale verification changed account identity or state: %v", err)
			}
		})
	}
}
