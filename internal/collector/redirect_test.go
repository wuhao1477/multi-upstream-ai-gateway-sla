package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// These endpoints only observe requests; they do not model any upstream protocol.
func TestOutboundDefaultsDoNotFollowRedirects(t *testing.T) {
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
			session := Session{BaseURL: source.URL, Family: FamilyNewAPI, Token: "test-secret-marker"}
			for name, run := range map[string]func() error{
				"client": func() error {
					_, _, err := NewClient(0).getJSONAuth(context.Background(), session, "/api/user/self")
					return err
				},
				"nil-http-client": func() error {
					_, _, err := (&Client{}).getJSONAuth(context.Background(), session, "/api/user/self")
					return err
				},
				"nil-detector": func() error {
					_, err := Detect(context.Background(), nil, source.URL)
					return err
				},
				"refresh": func() error {
					_, err := NewSub2APIAdapter(NewClient(0)).Refresh(context.Background(), Credential{
						BaseURL: source.URL, Family: FamilySub2API, RefreshToken: "test-secret-marker",
					})
					return err
				},
			} {
				t.Run(name, func(t *testing.T) {
					err := run()
					if err == nil || observed.Load() != 0 {
						t.Fatal("redirect was accepted or a second target received a request")
					}
					if strings.Contains(err.Error(), "test-secret-marker") {
						t.Fatal("redirect failure exposed Location or response contents")
					}
					if got, _, ok := HTTPFailure(err); !ok || got != status {
						t.Fatal("redirect failure lost its real HTTP status")
					}
				})
			}
		})
	}
}

func TestNewAPIUsesCanonicalTokenListPath(t *testing.T) {
	// Canonical path observed on 2026-10-08 (cookie_lifecycle_test.go).
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
	}))
	defer server.Close()
	_, err := NewNewAPIAdapter(NewClient(0)).FetchKeys(context.Background(), Session{
		BaseURL: server.URL, Family: FamilyNewAPI, QuotaPerUnit: 1,
	})
	if err != nil || path != "/api/token/" {
		t.Fatal("token list request did not directly use the canonical path")
	}
}
