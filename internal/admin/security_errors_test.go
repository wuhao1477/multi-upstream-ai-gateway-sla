package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

const errorSecret = "test-error-secret-marker"

// Inject failures at the transport boundary, never a successful upstream protocol.
type errorTransport struct{ cause error }

func (e errorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.cause }

func TestChannelURLValidationDoesNotExposeInput(t *testing.T) {
	s := NewServer(nil, "test-admin", testServer().Logger, nil)
	mux := http.NewServeMux()
	s.UpstreamRoutes(mux)
	for _, base := range []string{
		"https://" + errorSecret + ".invalid:bad/",
		"https:///" + errorSecret,
	} {
		body, err := json.Marshal(map[string]any{"name": "invalid URL", "base_url": base})
		if err != nil {
			t.Fatal(err)
		}
		code, response := do(t, mux, "test-admin", http.MethodPost, "/admin/channels", string(body))
		if code != 400 || strings.Contains(response, errorSecret) {
			t.Errorf("URL validation must reject without echoing input: status=%d", code)
		}
	}
}

func TestChannelDetectionDoesNotLogURL(t *testing.T) {
	s, mux, token := httpServer(t)
	var logs bytes.Buffer
	s.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	s.Detect = func(ctx context.Context, base string) (collector.DetectResult, error) {
		return collector.Detect(ctx, &http.Client{Transport: errorTransport{&net.DNSError{
			Name: errorSecret, Err: errorSecret, IsNotFound: true,
		}}}, base)
	}
	base := "https://" + errorSecret + ":" + errorSecret + "@error-log.example.invalid/backup?token=" + errorSecret
	body, err := json.Marshal(map[string]any{"name": "failure logging", "base_url": base, "auto_detect": true})
	if err != nil {
		t.Fatal(err)
	}
	conn, release, err := s.DB.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wipe(context.Background(), t, conn, base)
	defer wipe(context.Background(), t, conn, base)
	code, response := do(t, mux, token, http.MethodPost, "/admin/channels", string(body))
	if code != 201 || !strings.Contains(logs.String(), "DNS") {
		t.Fatalf("detection failure should retain diagnosis and channel creation: status=%d", code)
	}
	if strings.Contains(logs.String(), errorSecret) || strings.Contains(response, errorSecret) {
		t.Error("channel detection attached the credential-bearing URL to a safe error")
	}
}

func TestHubSyncErrorsStaySafeInStorageAPIAndLogs(t *testing.T) {
	ctx := context.Background()
	s := NewServer(testPool(t), "test-admin", nil, nil)
	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	original, err := store.LoadHubSyncConfig(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreErrorTestConfig(t, conn, original)
	if err := store.SaveHubSyncConfig(ctx, conn, store.HubSyncConfig{
		WebDAVURL: "https://" + errorSecret + ":" + errorSecret + "@dav.example.invalid/" +
			errorSecret + ".json?token=" + errorSecret + "#" + errorSecret,
		IntervalMinutes: 5, ApplyMode: store.HubSyncModeReport,
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.HubSyncRoutes(mux)
	for _, tc := range []struct {
		name, category string
		cause          error
	}{
		{"dns", "DNS", &net.DNSError{Name: errorSecret, Err: errorSecret, IsNotFound: true}},
		{"connect", "连接", &net.OpError{Op: "dial", Err: errors.New(errorSecret)}},
		{"cancel", "canceled", context.Canceled},
		{"timeout", "deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			s.HubHTTP = &http.Client{Transport: errorTransport{tc.cause}}
			_, runErr := s.RunHubSync(ctx, store.HubSyncTriggerManual, false)
			if runErr == nil || !strings.Contains(runErr.Error(), tc.category) || strings.Contains(runErr.Error(), errorSecret) {
				t.Fatal("returned error exposed a secret or lost its diagnostic category")
			}
			if tc.cause == context.Canceled || tc.cause == context.DeadlineExceeded {
				if !errors.Is(runErr, tc.cause) {
					t.Fatal("context error identity lost")
				}
			}
			assertSafeHubRun(t, conn, mux, runErr.Error())
			code, _ := do(t, mux, "test-admin", http.MethodPost, "/admin/hub-sync/run", "")
			if code != http.StatusAccepted {
				t.Fatalf("manual sync status=%d", code)
			}
			deadline := time.Now().Add(5 * time.Second)
			for s.hubSyncBusy.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s.hubSyncBusy.Load() {
				t.Fatal("manual sync did not finish")
			}
			assertSafeHubRun(t, conn, mux, runErr.Error())
			if !strings.Contains(logs.String(), "手动同步失败") || !strings.Contains(logs.String(), tc.category) || strings.Contains(logs.String(), errorSecret) {
				t.Fatal("manual sync log exposed a secret or omitted the failure")
			}
		})
	}
}

func assertSafeHubRun(t *testing.T, conn *pgx.Conn, mux http.Handler, want string) {
	t.Helper()
	runs, err := store.ListHubSyncRuns(context.Background(), conn, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("read persisted sync failure: %v", err)
	}
	id := runs[0].ID
	defer func() {
		if _, err := conn.Exec(context.Background(), `DELETE FROM hub_sync_runs WHERE id=$1`, id); err != nil {
			t.Error(err)
		}
	}()
	if runs[0].Error != want || runs[0].Applied {
		t.Error("persisted sync failure lost its safe error or claimed data was applied")
	}
	for _, path := range []string{"/admin/hub-sync/runs", fmt.Sprintf("/admin/hub-sync/runs/%d", id)} {
		code, body := do(t, mux, "test-admin", http.MethodGet, path, "")
		if code != 200 || !strings.Contains(body, want) || strings.Contains(body, errorSecret) {
			t.Errorf("sync history API lost the safe error: %s status=%d", path, code)
		}
	}
}

func restoreErrorTestConfig(t *testing.T, conn *pgx.Conn, c store.HubSyncConfig) {
	t.Helper()
	if err := store.SaveHubSyncConfig(context.Background(), conn, c); err != nil {
		t.Error(err)
	}
	// 保存时空密码表示"不改"；原本没有密码的要显式清掉。
	_, err := conn.Exec(context.Background(), `UPDATE hub_sync_config
   SET webdav_password_ciphertext = CASE WHEN $1 THEN NULL ELSE webdav_password_ciphertext END,
       backup_password_ciphertext = CASE WHEN $2 THEN NULL ELSE backup_password_ciphertext END
 WHERE id=1`, c.WebDAVPassword == "", c.BackupPassword == "")
	if err != nil {
		t.Error(err)
	}
}
