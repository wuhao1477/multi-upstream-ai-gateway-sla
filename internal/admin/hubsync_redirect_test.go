package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// The second endpoint only observes unwanted requests; it is not a fake WebDAV service.
func TestWebDAVDefaultsRejectRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var observed atomic.Int32
			observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				observed.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer observer.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", observer.URL+"/test-secret-marker")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "test-secret-marker")
			}))
			defer source.Close()
			for _, client := range []*http.Client{(&Server{}).hubSyncClient(), nil} {
				_, err := collector.FetchHubBackup(context.Background(), client, collector.HubWebDAVConfig{
					URL: source.URL + "/backup.json?token=test-secret-marker", Password: "test-secret-marker",
				})
				if err == nil || observed.Load() != 0 {
					t.Fatal("WebDAV followed a redirect")
				}
				if strings.Contains(err.Error(), "test-secret-marker") || !strings.Contains(err.Error(), fmt.Sprint(status)) {
					t.Fatal("WebDAV redirect error exposed secrets or lost its HTTP status")
				}
			}
		})
	}
}
