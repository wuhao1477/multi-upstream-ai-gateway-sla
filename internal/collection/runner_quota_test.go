package collection

import (
	"context"
	"net/http"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 行为测试：真站点不能按需切换换算基数或返回缺字段响应。
// /api/status 信封来自 2026-10-08 api.lyjxka.top 公开实测；250000 仅用于防止写死 500000。
func TestRunnerRecoversMissingQuotaPerUnit(t *testing.T) {
	access, session := cookieAccessFixture(t)
	ctx := context.Background()
	ch := store.Channel{ID: session.ChannelID, BaseURL: session.BaseURL, SiteFamily: "newapi"}
	conn, release, err := access.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() {
		if _, err := conn.Exec(ctx, `DELETE FROM collector_snapshots WHERE channel_id=$1`, ch.ID); err != nil {
			t.Error(err)
		}
	}()
	requests := 0
	access.Client.HC = &http.Client{Transport: cookieWire(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != http.MethodGet || req.URL.String() != ch.BaseURL+"/api/status" ||
			req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Error("quota discovery must only read the site's public status without credentials")
		}
		return cookieAccessResponse(req, 200, `{"success":true,"data":{"quota_per_unit":250000}}`), nil
	})}

	creds, err := NewRunner(access.Pool, access.Client).LoadCredentials(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].QuotaPerUnit != 250000 {
		t.Fatal("missing quota_per_unit must be recovered from the site's public status")
	}
	if !creds[0].CookieEnabled || creds[0].AccountID != session.AccountID {
		t.Fatal("recovering quota must preserve the Cookie account")
	}
	if store.QuotaPerUnit(ctx, conn, ch.ID) != 250000 {
		t.Fatal("discovered quota must be persisted for other collection paths")
	}

	// 新 Runner 仍复用持久化结果，不依赖单进程内存缓存。
	creds, err = NewRunner(access.Pool, access.Client).LoadCredentials(ctx, ch)
	if err != nil || len(creds) != 1 || creds[0].QuotaPerUnit != 250000 || requests != 1 {
		t.Fatalf("stored quota must be reused without another probe: requests=%d err=%v", requests, err)
	}
}

func TestRunnerRejectsUnavailableQuotaPerUnit(t *testing.T) {
	access, session := cookieAccessFixture(t)
	ctx := context.Background()
	ch := store.Channel{ID: session.ChannelID, BaseURL: session.BaseURL, SiteFamily: "newapi"}
	conn, release, err := access.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() {
		if _, err := conn.Exec(ctx, `DELETE FROM collector_snapshots WHERE channel_id=$1`, ch.ID); err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing", 200, `{"success":true,"data":{"checkin_enabled":true}}`},
		{"zero", 200, `{"success":true,"data":{"quota_per_unit":0}}`},
		{"unavailable", 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access.Client.HC = &http.Client{Transport: cookieWire(func(req *http.Request) (*http.Response, error) {
				return cookieAccessResponse(req, tc.status, tc.body), nil
			})}
			_, err := NewRunner(access.Pool, access.Client).LoadCredentials(ctx, ch)
			if err == nil {
				t.Fatal("unavailable quota must fail instead of silently using zero or a fixed default")
			}
			var snapshots int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM collector_snapshots WHERE channel_id=$1`, ch.ID).Scan(&snapshots); err != nil {
				t.Fatal(err)
			}
			if snapshots != 0 {
				t.Fatal("failed quota discovery must not persist a detection result")
			}
		})
	}
}
