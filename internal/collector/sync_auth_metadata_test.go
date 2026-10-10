package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Mixed failures are controlled behavioral inputs: a later account's business
// 401 must not be hidden by an earlier 429, nor discard that account's Retry-After.
func TestSyncerKeepsBusinessAuthMetadataAcrossAccounts(t *testing.T) {
	_, _, rejected := ReadAuthResponse(&http.Response{
		StatusCode: 200, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"code":401,"message":"test-secret-marker"}`)),
	}, FamilySub2API, http.MethodGet, "/api/v1/auth/me", false)
	limited := &HTTPError{StatusCode: 429, RetryAfter: 10 * time.Minute, Message: "rate limited"}
	for _, errs := range [][]error{{limited, rejected}, {rejected, limited}} {
		var result SyncResult
		(&Syncer{}).run(context.Background(), &result, CapabilityMap{CapAccount: Supported}, CapAccount,
			func() (int, int, string, error) { return capabilityResult(1, errs, "") })
		item := result.Items[0]
		if item.Status != StatusPartial || item.Rows != 1 || item.Failed != 2 {
			t.Fatal("mixed failures lost the successful account or partial status")
		}
		if item.HTTPStatus != 200 || item.BusinessCode != 401 || item.RetryAfterMs != 600000 {
			t.Errorf("mixed account metadata: HTTP=%d business=%d retry_ms=%d", item.HTTPStatus, item.BusinessCode, item.RetryAfterMs)
		}
		if !item.AuthenticationFailed {
			t.Fatal("business 401 lost the authentication failure marker")
		}
		_, _, _, err := capabilityResult(1, errs, "")
		if !errors.Is(err, ErrUnauthorized) || !strings.Contains(item.Error, "rate limited") || strings.Contains(item.Error, "test-secret-marker") {
			t.Fatal("combined error lost classification, diagnosis, or exposed an upstream message")
		}
	}
}

// The rejection body was observed on a real NewAPI site (see rejectionBody).
// Mixed failures and Retry-After are controlled inputs to test retry behavior;
// real accounts cannot produce these failures in a requested order.
func TestSyncerKeepsNewAPIAuthMetadata(t *testing.T) {
	_, _, rejected := ReadAuthResponse(&http.Response{
		StatusCode: 200, Header: http.Header{"Retry-After": {"120"}},
		Body: io.NopCloser(strings.NewReader(rejectionBody)),
	}, FamilyNewAPI, http.MethodGet, "/api/user/self", false)
	_, _, other := ReadAuthResponse(&http.Response{
		StatusCode: 200, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"success":false,"message":"test-secret-marker"}`)),
	}, FamilyNewAPI, http.MethodGet, "/api/user/self", false)
	limited := &HTTPError{StatusCode: 429, RetryAfter: 10 * time.Minute, Message: "rate limited"}
	for _, tc := range []struct {
		name         string
		errs         []error
		rows, failed int
		status       ItemStatus
		authFailed   bool
		retryAfterMs int64
	}{
		{"failed", []error{rejected}, 0, 0, StatusFailed, true, 120000},
		{"partial_auth_first", []error{rejected, limited}, 1, 2, StatusPartial, true, 600000},
		{"partial_auth_last", []error{limited, rejected}, 1, 2, StatusPartial, true, 600000},
		{"other_rejection", []error{other}, 0, 0, StatusFailed, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result SyncResult
			(&Syncer{}).run(context.Background(), &result, CapabilityMap{CapAccount: Supported}, CapAccount,
				func() (int, int, string, error) { return capabilityResult(tc.rows, tc.errs, "") })
			item := result.Items[0]
			if item.Status != tc.status || item.Rows != tc.rows || item.Failed != tc.failed {
				t.Fatalf("lost capability outcome: status=%s rows=%d failed=%d", item.Status, item.Rows, item.Failed)
			}
			if item.HTTPStatus != 200 || item.BusinessCode != 0 || item.RetryAfterMs != tc.retryAfterMs {
				t.Errorf("lost real response metadata: HTTP=%d business=%d retry_ms=%d", item.HTTPStatus, item.BusinessCode, item.RetryAfterMs)
			}
			raw, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				AuthenticationFailed bool `json:"authentication_failed"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.AuthenticationFailed != tc.authFailed {
				t.Errorf("authentication_failed=%v, want %v", decoded.AuthenticationFailed, tc.authFailed)
			}
			if strings.Contains(item.Error, "test-secret-marker") {
				t.Fatal("upstream rejection exposed its message")
			}
		})
	}
}
