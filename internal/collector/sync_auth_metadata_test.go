package collector

import (
	"context"
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
		_, _, _, err := capabilityResult(1, errs, "")
		if !errors.Is(err, ErrUnauthorized) || !strings.Contains(item.Error, "rate limited") || strings.Contains(item.Error, "test-secret-marker") {
			t.Fatal("combined error lost classification, diagnosis, or exposed an upstream message")
		}
	}
}
