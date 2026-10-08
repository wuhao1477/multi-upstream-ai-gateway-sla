package collector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCookieSessionDoesNotSendEmptyBearer(t *testing.T) {
	h := authHeaders(Session{ExternalUserID: "42", UserIDHeader: "New-API-User"})
	if _, exists := h["Authorization"]; exists {
		t.Fatal("session without a token must not send Authorization")
	}
	if h.Get("New-API-User") != "42" {
		t.Fatal("user ID must be preserved")
	}
}

type cookieTimeoutBody struct{}

func (cookieTimeoutBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

func TestCookieReadTimeoutKeepsRetryCause(t *testing.T) {
	resp := cookieResponse(200, "")
	resp.Body = io.NopCloser(cookieTimeoutBody{})
	_, raw, err := ReadAuthResponse(resp, http.MethodGet, "/api/user/self", true)
	if !errors.Is(err, context.DeadlineExceeded) || len(raw) != 0 {
		t.Fatal("Cookie response timeout must remain retryable without exposing a partial body")
	}
}

// 行为类输入：真站点无法按需产生 401/429/权限错误；不举证上游协议。
// 成功与失效样本沿用 newapi_rejection_test.go 的实测形态。
const cookieSelfBody = `{"success":true,"data":{"id":1663,"quota":189502940}}`

func cookieResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCookieSwitchOnlyOnAuthenticationFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantCookie bool
	}{
		{"valid_token", 200, cookieSelfBody, false},
		{"unauthorized", 401, "", true},
		{"known_invalid_token", 200, rejectionBody, true},
		{"forbidden", 403, "", false},
		{"rate_limit", 429, "", false},
		{"server_error", 503, "", false},
		{"other_business_error", 200, `{"success":false,"message":"permission denied"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokenCalls, cookieCalls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls++
				if r.Header.Get("Authorization") != "Bearer test-only" || r.Header.Get("Cookie") != "" {
					t.Error("token request headers changed")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := NewClient(0)
			c.CookieRequest = func(r *http.Request, s Session) (*http.Response, error) {
				cookieCalls++
				if r.Header.Get("Authorization") != "" || s.AccountID != 7 {
					t.Error("Cookie request must remove Bearer and retain account identity")
				}
				return cookieResponse(200, cookieSelfBody), nil
			}
			s := SessionFrom(Credential{AccountID: 7, Family: FamilyNewAPI, AccessToken: "test-only", CookieEnabled: true}, srv.URL, 1)
			_, _, err := c.getJSONAuth(context.Background(), s, "/api/user/self")
			if tc.wantCookie {
				if err != nil || cookieCalls != 1 {
					t.Fatalf("want one Cookie attempt, got %d: %v", cookieCalls, err)
				}
				_, _, err = c.getJSONAuth(context.Background(), s, "/api/pricing")
				if err != nil || tokenCalls != 1 || cookieCalls != 2 {
					t.Fatal("the next read must stay on Cookie, not retry the rejected token")
				}
			} else if cookieCalls != 0 {
				t.Fatal("non-authentication failure must not switch credentials")
			}
		})
	}
}

func TestNewAPICookieAuthenticationRunsOnce(t *testing.T) {
	for _, token := range []string{"", "stale"} {
		t.Run("token="+token, func(t *testing.T) {
			srv := rejectingNewAPI(t)
			c := NewClient(0)
			calls := 0
			c.CookieRequest = func(r *http.Request, s Session) (*http.Response, error) {
				calls++
				if r.URL.Path != "/api/user/self" || r.Header.Get("Authorization") != "" {
					t.Error("Cookie authentication must only read self without Bearer")
				}
				return cookieResponse(200, cookieSelfBody), nil
			}
			s, err := NewNewAPIAdapter(c).Authenticate(context.Background(), Credential{
				AccountID: 7, Family: FamilyNewAPI, BaseURL: srv.URL, ExternalUserID: "1663",
				AccessToken: token, CookieEnabled: true,
			})
			if err != nil || calls != 1 || s.AccountID != 7 || !s.CookieAllowed {
				t.Fatalf("Cookie authentication must run once: count=%d err=%v", calls, err)
			}
		})
	}
}

func TestCookieResponseErrorsDoNotExposeBody(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{503, "sensitive-cookie-value"},
		{200, "<html>sensitive-cookie-value</html>"},
		{200, `{"success":false,"message":"sensitive-cookie-value"}`},
	} {
		c := NewClient(0)
		calls := 0
		c.CookieRequest = func(*http.Request, Session) (*http.Response, error) {
			calls++
			return cookieResponse(tc.status, tc.body), nil
		}
		s := SessionFrom(Credential{AccountID: 7, Family: FamilyNewAPI, CookieEnabled: true}, "https://cookie.example.invalid", 1)
		_, raw, err := c.getJSONAuth(context.Background(), s, "/api/user/self")
		if calls != 1 || err == nil || strings.Contains(err.Error(), "sensitive-cookie-value") || len(raw) != 0 {
			t.Fatal("Cookie failures must expose neither response body nor secret")
		}
	}
}
