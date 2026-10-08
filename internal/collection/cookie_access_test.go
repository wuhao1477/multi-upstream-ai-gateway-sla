package collection

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 只记录客户端字节并提供按需错误，不举证站点协议或真实 Cookie 可用性。
// self 成功样本来源沿用 collector/newapi_rejection_test.go 的实测形态。
const cookieAccessSelf = `{"success":true,"data":{"id":1663,"quota":189502940}}`

type cookieWire func(*http.Request) (*http.Response, error)

func (f cookieWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cookieAccessFixture(t *testing.T) (*CookieAccess, collector.Session) {
	t.Helper()
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, release, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	base := fmt.Sprintf("https://cookie-access-%d.example.invalid", time.Now().UnixNano())
	channel, err := store.CreateChannel(ctx, conn, store.Channel{Name: "cookie-access-test", BaseURL: base, SiteFamily: "newapi"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, done, err := pool.Acquire(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer done()
		if _, err := c.Exec(ctx, `DELETE FROM upstream_accounts WHERE channel_id=$1`, channel); err != nil {
			t.Error(err)
			return
		}
		if _, err := c.Exec(ctx, `DELETE FROM channels WHERE id=$1`, channel); err != nil {
			t.Error(err)
		}
	})
	id, err := store.CreateAccount(ctx, conn, store.Account{ChannelID: channel, ExternalUserID: "1663"})
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	credentials, err := store.NewCookieCredentialStore(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := credentials.Save(ctx, tx, id, store.SaveCookieCredentialInput{Enabled: true, CookieHeader: "session=behavior-test-only=="}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	client := collector.NewClient(0)
	access := NewCookieAccess(pool, credentials, client)
	s := collector.SessionFrom(collector.Credential{
		AccountID: id, ChannelID: channel, Family: collector.FamilyNewAPI,
		ExternalUserID: "1663", CookieEnabled: true,
	}, base, 500000)
	return access, s
}

func cookieAccessRequest(t *testing.T, s collector.Session, path string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer must-not-send")
	return r
}

func cookieAccessResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestCookieAccessVerifiesOnceAndSendsOnlyCookie(t *testing.T) {
	a, s := cookieAccessFixture(t)
	var paths []string
	a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "session=behavior-test-only==" || r.Header.Get("New-API-User") != "1663" {
			t.Error("Cookie headers or account binding violated")
		}
		return cookieAccessResponse(r, 200, cookieAccessSelf), nil
	})
	for range 2 {
		resp, err := a.Do(cookieAccessRequest(t, s, "/api/pricing"), s)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if strings.Join(paths, ",") != "/api/user/self,/api/pricing,/api/pricing" {
		t.Fatalf("identity must be verified once before reading: %v", paths)
	}
	conn, release, err := a.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cred, err := a.Credentials.Load(context.Background(), conn, s.AccountID)
	if err != nil || cred.State != "ready" {
		t.Fatalf("Cookie not marked ready: %v", err)
	}
}

func TestCookieAccessFailureStateAndNoRedirect(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body, state string
	}{
		{"identity_mismatch", 200, `{"data":{"id":1664}}`, "needs_action"},
		{"unauthorized", 401, "private-cookie-marker", "expired"},
		{"business_auth_failure", 200, `{"message":"Unauthorized, invalid access token","success":false}`, "expired"},
		{"redirect", 302, "private-cookie-marker", "needs_action"},
		{"html", 200, "<html>private-cookie-marker</html>", "needs_action"},
		{"rate_limit", 429, "private-cookie-marker", "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s := cookieAccessFixture(t)
			calls := 0
			a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := cookieAccessResponse(r, tc.status, tc.body)
				resp.Header.Set("Location", "https://another.example.invalid/api/user/self")
				resp.Header.Set("Retry-After", "120")
				return resp, nil
			})
			_, err := a.Do(cookieAccessRequest(t, s, "/api/pricing"), s)
			if err == nil || strings.Contains(err.Error(), "private-cookie-marker") || calls != 1 {
				t.Fatal("Cookie failure leaked data or continued sending")
			}
			if tc.status == 429 {
				status, delay, ok := collector.HTTPFailure(err)
				if !ok || status != 429 || delay != 120*time.Second {
					t.Fatal("retry metadata lost")
				}
			}
			conn, release, err := a.Pool.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			cred, err := a.Credentials.Load(context.Background(), conn, s.AccountID)
			if err != nil || cred.State != tc.state {
				t.Fatalf("state=%s want=%s err=%v", cred.State, tc.state, err)
			}
			if tc.state != "unverified" {
				_, _ = a.Do(cookieAccessRequest(t, s, "/api/pricing"), s)
				if calls != 1 {
					t.Fatal("invalid Cookie must stop automatic reads")
				}
				a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
					return cookieAccessResponse(r, 200, cookieAccessSelf), nil
				})
				if err := a.Validate(context.Background(), s.AccountID); err != nil {
					t.Fatalf("explicit validation must retry stored Cookie: %v", err)
				}
			}
		})
	}
}

func TestCookieReadScope(t *testing.T) {
	const base = "https://cookie.example.invalid"
	for _, tc := range []struct {
		method, target string
		allowed        bool
	}{
		{"GET", base + "/api/user/self", true}, {"GET", base + "/api/pricing", true},
		{"GET", base + "/api/token?p=1&size=100", true}, {"POST", base + "/api/token/42/key", true},
		{"POST", base + "/api/token/", false}, {"GET", base + "/api/user/token", false},
		{"POST", base + "/api/user/login", false}, {"GET", base + "/v1/models", false},
		{"POST", base + "/api/token/0/key", false}, {"POST", base + "/api/token/42/key?x=1", false},
		{"GET", base + "/api/token?p=-1", false}, {"GET", base + "/api/token?redirect=https://other.invalid", false},
		{"GET", base + "/api/user/%73elf", false}, {"GET", base + "/api/user/self#fragment", false},
		{"GET", "https://another.example.invalid/api/user/self", false},
		{"GET", "https://user@cookie.example.invalid/api/user/self", false},
	} {
		r, err := http.NewRequest(tc.method, tc.target, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cookieReadPath(r, base)
		if (err == nil) != tc.allowed {
			t.Errorf("%s %s: allowed=%v err=%v", tc.method, tc.target, tc.allowed, err)
		}
	}
}

func TestCookieDialRejectsPrivateDNS(t *testing.T) {
	for _, host := range []string{"localhost:443", "127.0.0.1:443", "[::1]:443", "10.0.0.1:443", "169.254.169.254:443"} {
		conn, err := dialCookie(context.Background(), "tcp", host)
		if conn != nil {
			_ = conn.Close()
			t.Error("private connection opened")
		}
		if !errors.Is(err, store.ErrCookieSite) {
			t.Errorf("private DNS/address not rejected: %s %v", host, err)
		}
	}
}

func TestProvisionKeysDoesNotAuthenticateWithCookie(t *testing.T) {
	client := collector.NewClient(0)
	calls := 0
	client.CookieRequest = func(*http.Request, collector.Session) (*http.Response, error) {
		calls++
		return nil, collector.ErrCookieExpired
	}
	runner := &Runner{Client: client}
	_, err := runner.ProvisionKeys(context.Background(), nil, store.Channel{SiteFamily: "newapi"}, collector.Credential{
		AccountID: 7, Family: collector.FamilyNewAPI, BaseURL: "https://cookie.example.invalid",
		ExternalUserID: "1663", CookieEnabled: true,
	}, collector.KeyProvisionRequest{})
	if err == nil || calls != 0 {
		t.Fatal("remote key provisioning must not authenticate with Cookie")
	}
}
