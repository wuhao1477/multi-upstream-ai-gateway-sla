package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Business failures and malformed codes are controlled inputs (CLAUDE.md §1),
// not claims that any real upstream emits these exact envelopes.
func TestSub2APIBusinessCodesRejectBeforeReturningData(t *testing.T) {
	for _, tc := range []struct {
		code         string
		reject, auth bool
	}{
		{"0", false, false}, {`"0"`, false, false}, {"", false, false},
		{"401", true, true}, {`"401"`, true, true}, {"500", true, false},
		{"null", true, false}, {"false", true, false}, {"{}", true, false},
		{"[]", true, false}, {"1.5", true, false}, {`"0junk"`, true, false},
		{"1e-400", true, false}, {"9223372036854775808", true, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			body := `{"data":{}}`
			if tc.code != "" {
				body = fmt.Sprintf(`{"code":%s,"message":"test-secret-marker","data":null}`, tc.code)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			_, raw, err := NewClient(0).getJSONAuth(context.Background(),
				Session{BaseURL: server.URL, Family: FamilySub2API}, "/api/v1/auth/me")
			if (err != nil) != tc.reject {
				t.Fatalf("rejected = %t, want %t", err != nil, tc.reject)
			}
			if !tc.reject {
				return
			}
			if len(raw) != 0 || strings.Contains(err.Error(), "test-secret-marker") {
				t.Fatal("business rejection leaked response")
			}
			if IsAuthenticationFailure(err) != tc.auth {
				t.Fatal("incorrect authentication classification")
			}
			status, _, ok := HTTPFailure(err)
			if !ok || status != 200 {
				t.Fatal("business failure lost the real HTTP 200 status")
			}
		})
	}
}

func TestSub2APIBusinessRejectionReachesAllReadPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":401,"message":"test-secret-marker","data":null}`)
	}))
	defer server.Close()
	a := NewSub2APIAdapter(NewClient(0))
	s := Session{BaseURL: server.URL, Family: FamilySub2API, Token: "test-access"}
	cred := Credential{BaseURL: server.URL, Family: FamilySub2API, AccessToken: "test-access", RefreshToken: "test-refresh"}
	_, authErr := a.Authenticate(context.Background(), cred)
	_, accountErr := a.FetchAccount(context.Background(), s)
	_, keysErr := a.FetchKeys(context.Background(), s)
	_, groupsErr := a.FetchGroups(context.Background(), s)
	_, pricingErr := a.FetchPricing(context.Background(), s)
	_, catalogErr := a.FetchModelCatalog(context.Background(), s)
	createErr := a.CreateRemoteKey(context.Background(), s, RemoteKeyRequest{GroupRef: "1", Name: "test"})
	_, refreshErr := a.Refresh(context.Background(), cred)
	for _, err := range []error{authErr, accountErr, keysErr, groupsErr, pricingErr, catalogErr, createErr, refreshErr} {
		if !IsAuthenticationFailure(err) || strings.Contains(err.Error(), "test-secret-marker") {
			t.Fatal("an adapter path accepted or exposed the rejection")
		}
	}
	if !errors.Is(refreshErr, ErrNeedsRelogin) {
		t.Fatal("rejected refresh did not require relogin")
	}

	var result SyncResult
	(&Syncer{}).run(context.Background(), &result, a.Capabilities(), CapAccount,
		func() (int, int, string, error) { return 0, 0, "", accountErr })
	blob, err := json.Marshal(result.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		HTTPStatus   int `json:"http_status"`
		BusinessCode int `json:"business_code"`
	}
	if err := json.Unmarshal(blob, &item); err != nil {
		t.Fatal(err)
	}
	if item.HTTPStatus != 200 || item.BusinessCode != 401 {
		t.Fatal("sync result conflated HTTP and business status")
	}
}

func TestNewAPICodeFieldIsNotSub2APIBusinessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"code":401,"data":{"id":1}}`)
	}))
	defer server.Close()
	_, _, err := NewClient(0).getJSONAuth(context.Background(), Session{BaseURL: server.URL, Family: FamilyNewAPI}, "/api/user/self")
	if err != nil {
		t.Fatal("Sub2API code validation changed NewAPI semantics")
	}
}
