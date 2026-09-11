package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 端点形态来自 all-api-hub 当前适配器：
// https://github.com/qixing-jk/all-api-hub/blob/main/src/services/apiService/newApiFamily/default/tokenKeyResolver.ts
// 以及 NewAPI 的路由/控制器源码链接（见该文件注释）。
func TestNewAPIResolveKeySecretUsesPostRevealEndpoint(t *testing.T) {
	var method, path, authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, authorization = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/api/token/17/key" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"key":"remote-secret"}}`))
	}))
	defer srv.Close()

	adapter := NewNewAPIAdapter(NewClient(0))
	adapter.C.HC = srv.Client()
	got, err := adapter.ResolveKeySecret(context.Background(), Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "session-token",
		UserIDHeader: "New-API-User", ExternalUserID: "42",
	}, "17")
	if err != nil {
		t.Fatal(err)
	}
	if got != "remote-secret" {
		t.Fatalf("secret = %q, want remote-secret", got)
	}
	if method != http.MethodPost || path != "/api/token/17/key" {
		t.Fatalf("request = %s %s, want POST /api/token/17/key", method, path)
	}
	if authorization != "Bearer session-token" {
		t.Fatalf("Authorization = %q, want Bearer session-token", authorization)
	}
}

// 端点形态来自 all-api-hub 当前 Sub2API 适配器：
// https://github.com/qixing-jk/all-api-hub/blob/main/src/services/apiService/sub2api/index.ts
func TestSub2APIResolveKeySecretReadsDetailKey(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/keys/23" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"key":"remote-secret"}}`))
	}))
	defer srv.Close()

	adapter := NewSub2APIAdapter(NewClient(0))
	adapter.C.HC = srv.Client()
	got, err := adapter.ResolveKeySecret(context.Background(), Session{
		Family: FamilySub2API, BaseURL: srv.URL, Token: "jwt-token",
	}, "23")
	if err != nil {
		t.Fatal(err)
	}
	if got != "remote-secret" {
		t.Fatalf("secret = %q, want remote-secret", got)
	}
	if method != http.MethodGet || path != "/api/v1/keys/23" {
		t.Fatalf("request = %s %s, want GET /api/v1/keys/23", method, path)
	}
}

func TestResolveKeySecretRejectsMissingOrMaskedKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		text string
	}{
		{name: "missing", body: `{"data":{}}`, text: ""},
		{name: "masked", body: `{"data":{"key":"sk-****abcd"}}`, text: "sk-****abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			adapter := NewNewAPIAdapter(NewClient(0))
			adapter.C.HC = srv.Client()
			_, err := adapter.ResolveKeySecret(context.Background(), Session{
				Family: FamilyNewAPI, BaseURL: srv.URL, Token: "session-token",
				ExternalUserID: "42",
			}, "17")
			if err == nil {
				t.Fatal("missing or masked key should fail")
			}
			if tc.text != "" && strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error contains key value: %q", err)
			}
		})
	}
}
