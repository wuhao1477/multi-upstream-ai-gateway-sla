package collector

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These malformed responses are controlled error inputs, not upstream protocol
// fixtures (CLAUDE.md §1). Removing response redaction must fail these checks.
func TestReadAuthResponseRedactsFailures(t *testing.T) {
	const marker = "test-secret-marker"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"non_json", 200, marker},
		{"http_error", 429, marker},
		{"business_message", 200, `{"success":false,"message":"` + marker + `"}`},
		{"business_body", 200, `{"success":false,"refresh_token":"` + marker + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"2"}},
				Body: io.NopCloser(strings.NewReader(tc.body))}
			_, raw, err := ReadAuthResponse(resp, http.MethodGet, "/api/user/self?token="+marker, false)
			if err == nil || len(raw) != 0 || strings.Contains(err.Error(), marker) {
				t.Fatal("failed response exposed its body or query, or was accepted")
			}
			if tc.status == 429 {
				status, after, ok := HTTPFailure(err)
				if !ok || status != 429 || after != 2*time.Second {
					t.Fatal("redaction discarded HTTP status or Retry-After")
				}
			}
		})
	}
}

type securityErrorTransport struct{ err error }

func (s securityErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, s.err
}

// Transport errors are controlled inputs; the real http.Client still wraps
// them in url.Error. The assertions concern our public errors, not the double.
func TestOutboundErrorsRedactURLsAndPreserveCategories(t *testing.T) {
	const marker = "test-secret-marker"
	for _, tc := range []struct {
		name     string
		err      error
		category string
		cause    error
	}{
		{"dns", &net.DNSError{Name: marker, Err: marker}, "DNS", nil},
		{"connect", &net.OpError{Op: "dial", Err: errors.New(marker)}, "连接", nil},
		{"tls", &tls.CertificateVerificationError{Err: errors.New(marker)}, "TLS", nil},
		{"cancel", context.Canceled, "", context.Canceled},
		{"timeout", context.DeadlineExceeded, "", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc := &http.Client{Transport: securityErrorTransport{tc.err}}
			target := "https://user:" + marker + "@example.invalid/" + marker + ".json?token=" + marker + "#" + marker
			c := NewClient(0)
			c.HC = hc
			req, err := http.NewRequest(http.MethodGet, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, requestErr := c.Do(req)
			_, _, detectErr := getJSON(context.Background(), hc, target)
			_, webdavErr := FetchHubBackup(context.Background(), hc, HubWebDAVConfig{URL: target})
			for _, got := range []error{requestErr, detectErr, webdavErr} {
				if got == nil || strings.Contains(got.Error(), marker) || !strings.Contains(got.Error(), tc.category) {
					t.Fatal("network error exposed a secret or lost its category")
				}
				if tc.cause != nil && !errors.Is(got, tc.cause) {
					t.Fatal("network error lost cancellation or timeout semantics")
				}
			}
		})
	}
}

func TestInvalidOutboundURLsDoNotEscapeInErrors(t *testing.T) {
	const target = "https://example.invalid/%zz?token=test-secret-marker"
	c := NewClient(0)
	_, _, authErr := c.getJSONAuth(context.Background(), Session{BaseURL: target}, "/api/user/self")
	_, _, detectErr := getJSON(context.Background(), http.DefaultClient, target)
	_, davErr := ResolveHubBackupURL(target)
	_, refreshErr := NewSub2APIAdapter(c).Refresh(context.Background(), Credential{
		BaseURL: target, RefreshToken: "test-refresh",
	})
	for _, err := range []error{authErr, detectErr, davErr, refreshErr} {
		if err == nil || strings.Contains(err.Error(), "test-secret-marker") {
			t.Fatal("invalid outbound URL was accepted or exposed in error")
		}
	}
}

type securityErrorBody struct{ err error }

func (s securityErrorBody) Read([]byte) (int, error) { return 0, s.err }
func (s securityErrorBody) Close() error             { return nil }

func TestReadAuthResponseRedactsBodyReadError(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Body: securityErrorBody{errors.New("test-secret-marker")}}
	_, raw, err := ReadAuthResponse(resp, http.MethodGet, "/api/user/self", false)
	if err == nil || len(raw) != 0 || strings.Contains(err.Error(), "test-secret-marker") {
		t.Fatal("body read error was exposed or accepted")
	}
}

func TestRefreshFailuresKeepCredentialsAndHideResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing_access", 200, `{"data":{"refresh_token":"test-secret-marker"}}`},
		{"invalid_json", 200, "test-secret-marker"},
		{"http_error", 503, "test-secret-marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			cred := Credential{BaseURL: server.URL, AccessToken: "previous-access", RefreshToken: "previous-refresh"}
			next, err := NewSub2APIAdapter(NewClient(0)).Refresh(context.Background(), cred)
			if err == nil || strings.Contains(err.Error(), "test-secret-marker") {
				t.Fatal("refresh failure exposed its response or was accepted")
			}
			if next.AccessToken != cred.AccessToken || next.RefreshToken != cred.RefreshToken {
				t.Fatal("failed refresh overwrote the existing credentials")
			}
		})
	}
}
