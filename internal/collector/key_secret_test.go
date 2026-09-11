package collector

import (
	"context"
	"encoding/json"
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

func TestKeyImportResultUsesJSONFieldNames(t *testing.T) {
	raw, err := json.Marshal(KeyImportResult{
		Found: 1, Imported: 2, Skipped: 3, Failed: 4, Deferred: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"found", "imported", "skipped", "failed", "deferred"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("KeyImportResult JSON 缺少 %q：%s", name, raw)
		}
	}
	for _, name := range []string{"Found", "Imported", "Skipped", "Failed", "Deferred"} {
		if _, ok := fields[name]; ok {
			t.Fatalf("KeyImportResult JSON 不应使用 Go 字段名 %q：%s", name, raw)
		}
	}
}

// 来源：NewAPI bdef117 controller/token.go:AddToken 接收 Token JSON 并由 /api/token/ 注册：
// https://github.com/QuantumNous/new-api/blob/bdef117505247769268b209665fb3ad7554c3da7/controller/token.go
func TestNewAPICreateRemoteKeyUsesTokenEndpointAndGroup(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/token/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":""}`))
	}))
	defer srv.Close()

	adapter := NewNewAPIAdapter(NewClient(0))
	adapter.C.HC = srv.Client()
	err := adapter.CreateRemoteKey(context.Background(), Session{
		BaseURL: srv.URL, Token: "session-token", ExternalUserID: "42",
	}, RemoteKeyRequest{Name: "gateway-auto", GroupRef: "vip"})
	if err != nil {
		t.Fatal(err)
	}
	if body["name"] != "gateway-auto" || body["group"] != "vip" || body["unlimited_quota"] != true {
		t.Fatalf("创建载荷 = %#v", body)
	}
}

// 来源：Sub2API cdb5cfa handler/api_key_handler.go:CreateAPIKeyRequest 要求 group_id 为 *int64：
// https://github.com/Wei-Shaw/sub2api/blob/cdb5cfaf6c8cb08612ef552a4458d8d0b5850184/backend/internal/handler/api_key_handler.go
func TestSub2APICreateRemoteKeyUsesNumericGroupID(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/keys" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":23,"key":"remote-secret"}}`))
	}))
	defer srv.Close()

	adapter := NewSub2APIAdapter(NewClient(0))
	adapter.C.HC = srv.Client()
	err := adapter.CreateRemoteKey(context.Background(), Session{
		BaseURL: srv.URL, Token: "jwt-token",
	}, RemoteKeyRequest{Name: "gateway-auto", GroupRef: "17"})
	if err != nil {
		t.Fatal(err)
	}
	if body["name"] != "gateway-auto" || body["group_id"] != float64(17) || body["quota"] != float64(0) {
		t.Fatalf("创建载荷 = %#v", body)
	}
}

func TestSub2APICreateRemoteKeyRejectsNonNumericGroupBeforeRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	adapter := NewSub2APIAdapter(NewClient(0))
	adapter.C.HC = srv.Client()
	err := adapter.CreateRemoteKey(context.Background(), Session{
		BaseURL: srv.URL, Token: "jwt-token",
	}, RemoteKeyRequest{Name: "gateway-auto", GroupRef: "g-1"})
	if err == nil {
		t.Fatal("非数值 Sub2API 分组必须在发请求前被拒绝")
	}
	if requests != 0 {
		t.Fatalf("非数值分组不应触达创建端点，实际请求 %d 次", requests)
	}
}
