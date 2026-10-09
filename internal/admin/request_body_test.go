package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// The database is real; the counters observe calls that rejected inputs must never cause.
type bodyEffects struct {
	DB
	acquires, upstream int
}

func (b *bodyEffects) Acquire(ctx context.Context) (*pgx.Conn, func(), error) {
	b.acquires++
	return b.DB.Acquire(ctx)
}

func bodyTestServer(t *testing.T) (*http.ServeMux, *bodyEffects) {
	t.Helper()
	effects := &bodyEffects{DB: testPool(t)}
	s := NewServer(effects, "test-admin", testServer().Logger, nil)
	s.CookieCredentials = adminCookieStore(t)
	s.ValidateCookie = func(context.Context, int64) error { effects.upstream++; return nil }
	s.Detect = func(context.Context, string) (collector.DetectResult, error) {
		effects.upstream++
		return collector.DetectResult{}, nil
	}
	s.ImportKeys = func(context.Context, *pgx.Conn, int64, int64, collector.KeyImportRequest) (collector.KeyImportResult, error) {
		effects.upstream++
		return collector.KeyImportResult{}, nil
	}
	s.ProvisionKeys = func(context.Context, *pgx.Conn, int64, int64, collector.KeyProvisionRequest) (collector.KeyProvisionResult, error) {
		effects.upstream++
		return collector.KeyProvisionResult{}, nil
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	s.UpstreamRoutes(mux)
	s.ImportRoutes(mux)
	s.HubSyncRoutes(mux)
	return mux, effects
}

func bodyRequest(h http.Handler, method, path, body string, chunked bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-admin")
	if chunked {
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminJSONLimitsBeforeSideEffects(t *testing.T) {
	mux, effects := bodyTestServer(t)
	for _, route := range []struct {
		method, path string
		limit        int
	}{
		{"POST", "/admin/config/preview", 1 << 20}, {"POST", "/admin/config/apply", 1 << 20},
		{"POST", "/admin/channels", 1 << 20}, {"PATCH", "/admin/channels/1", 1 << 20},
		{"POST", "/admin/accounts", 1 << 20}, {"PATCH", "/admin/accounts/1", 1 << 20},
		{"POST", "/admin/keys", 1 << 20}, {"PATCH", "/admin/keys/1", 1 << 20},
		{"POST", "/admin/collector/credentials", 1 << 20},
		{"POST", "/admin/keys/import", 1 << 20}, {"POST", "/admin/keys/provision", 1 << 20},
		{"PUT", "/admin/hub-sync", 1 << 20},
		{"PUT", "/admin/accounts/1/cookie-credentials", 128 << 10},
		{"POST", "/admin/accounts/1/cookie-credentials/validate", 1024},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			for _, chunked := range []bool{false, true} {
				effects.acquires, effects.upstream = 0, 0
				rec := bodyRequest(mux, route.method, route.path, "{}"+strings.Repeat(" ", route.limit), chunked)
				if rec.Code != http.StatusRequestEntityTooLarge || effects.acquires != 0 || effects.upstream != 0 {
					t.Errorf("oversize: status=%d database=%d upstream=%d", rec.Code, effects.acquires, effects.upstream)
				}
			}
			effects.acquires, effects.upstream = 0, 0
			rec := bodyRequest(mux, route.method, route.path, `{} {"access_token":"test-body-secret-marker"}`, true)
			if rec.Code != 400 || effects.acquires != 0 || effects.upstream != 0 || strings.Contains(rec.Body.String(), "test-body-secret-marker") {
				t.Errorf("trailing JSON: status=%d database=%d upstream=%d", rec.Code, effects.acquires, effects.upstream)
			}
		})
	}
}

func TestAdminJSONBoundaryAndImportLimit(t *testing.T) {
	mux, effects := bodyTestServer(t)
	valid := `{"param_key":"collector_request_interval_ms","new_value":"200","changed_by":"test","change_reason":"test"}`
	rec := bodyRequest(mux, "POST", "/admin/config/preview", valid+strings.Repeat(" ", (1<<20)-len(valid)), true)
	if rec.Code != 200 {
		t.Fatalf("exact 1 MiB ordinary request rejected: %d", rec.Code)
	}
	// An empty site entry avoids inventing an upstream; this tests upload framing only.
	backup := `{"accounts":{"accounts":[{}]}}`
	for _, size := range []int{(1 << 20) + 1, 32 << 20, (32 << 20) + 1} {
		effects.acquires, effects.upstream = 0, 0
		rec := bodyRequest(mux, "POST", "/admin/import/all-api-hub?dry_run=true", backup+strings.Repeat(" ", size-len(backup)), true)
		want := 200
		if size > 32<<20 {
			want = 413
		}
		if rec.Code != want || effects.upstream != 0 || (want == 413 && effects.acquires != 0) {
			t.Errorf("import size=%d status=%d want=%d", size, rec.Code, want)
		}
	}
	rec = bodyRequest(mux, "POST", "/admin/import/all-api-hub?dry_run=true", backup+` {}`, true)
	if rec.Code != 400 {
		t.Fatal("import accepted trailing JSON")
	}
	rec = bodyRequest(mux, "POST", "/admin/config/preview", `{"param_key":`, true)
	if rec.Code != 400 {
		t.Fatal("invalid JSON no longer returns 400")
	}
}

func TestAdminTokenOutcomesDoNotExposeInput(t *testing.T) {
	for _, tc := range []struct {
		configured, submitted string
		want                  int
	}{
		{"test-admin", "test-admin", 204},
		{"test-admin", "test-secret-marker", 401},
		{"test-admin", "", 401},
		{"", "test-secret-marker", 503},
	} {
		s := NewServer(nil, tc.configured, nil, nil)
		h := s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		req := httptest.NewRequest("POST", "/admin/test", nil)
		req.Header.Set("Authorization", "Bearer "+tc.submitted)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want || strings.Contains(rec.Body.String(), "test-secret-marker") {
			t.Fatalf("authentication status=%d want=%d or input exposed", rec.Code, tc.want)
		}
	}
}
