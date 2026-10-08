package collection

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func TestCookieReadRejectsRequestBody(t *testing.T) {
	r, err := http.NewRequest(http.MethodGet, "https://cookie.example.invalid/api/user/self", strings.NewReader("unexpected body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cookieReadPath(r, "https://cookie.example.invalid"); err == nil {
		t.Fatal("Cookie read must not forward arbitrary request bodies")
	}
}

// 2026-10-08 首站免密 GET 实测：/api/token?p=1&size=100 → 301 /api/token/?p=1&size=100。
// 只验证本机请求路径，不把免密探测当作 Cookie 认证成功。
func TestCookieUsesCanonicalTokenListPath(t *testing.T) {
	a, s := cookieAccessFixture(t)
	lastPath := ""
	a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
		lastPath = r.URL.Path
		return cookieAccessResponse(r, 200, cookieAccessSelf), nil
	})
	resp, err := a.Do(cookieAccessRequest(t, s, "/api/token?p=1&size=100"), s)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if lastPath != "/api/token/" {
		t.Fatalf("Cookie must directly use the canonical list path, got %s", lastPath)
	}
}

// 真数据库在限速等待期间修改配置；断言验证过的旧会话不能继续出站。
func TestCookieAccessRechecksAfterWait(t *testing.T) {
	for _, replace := range []bool{false, true} {
		a, s := cookieAccessFixture(t)
		calls := 0
		a.http.Transport = cookieWire(func(r *http.Request) (*http.Response, error) {
			calls++
			return cookieAccessResponse(r, 200, cookieAccessSelf), nil
		})
		resp, err := a.Do(cookieAccessRequest(t, s, "/api/user/self"), s)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		a.Client.MinInterval = time.Millisecond
		a.Client.WaitHost = func(ctx context.Context, _ string, _ time.Duration) error {
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
			in := store.SaveCookieCredentialInput{}
			if replace {
				in.Enabled, in.CookieHeader = true, "session=replacement-test-only"
			}
			if _, err := a.Credentials.Save(ctx, tx, s.AccountID, in); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		_, err = a.Do(cookieAccessRequest(t, s, "/api/pricing"), s)
		if err == nil || calls != 1 {
			t.Fatal("changed Cookie was sent after pacing wait")
		}
		if replace && !errors.Is(err, store.ErrCookieCredentialChanged) {
			t.Fatal("old session must report revision conflict")
		}
	}
}

func TestCookieValidationAndKeyImportShareChannelLock(t *testing.T) {
	a, s := cookieAccessFixture(t)
	ctx := context.Background()
	conn, release, err := a.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	unlock, err := lockChannelRead(ctx, conn, s.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := a.Validate(ctx, s.AccountID); !errors.Is(err, ErrChannelReadBusy) {
		t.Fatalf("validation must not overlap channel sync: %v", err)
	}
	other, done, err := a.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	runner := &Runner{Client: a.Client}
	_, err = runner.ImportKeys(ctx, other, store.Channel{ID: s.ChannelID, SiteFamily: "newapi"}, []collector.Credential{{AccountID: s.AccountID}}, collector.KeyImportRequest{})
	if !errors.Is(err, ErrChannelReadBusy) {
		t.Fatalf("key import must not overlap channel sync: %v", err)
	}
}
