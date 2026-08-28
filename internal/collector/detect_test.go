package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeSite 按路径返回预设响应，模拟一个上游站点。
func fakeSite(routes map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestDetectNewAPI(t *testing.T) {
	// 真实 NewAPI /api/status 的形态（04 §2/§3.1）
	srv := fakeSite(map[string]string{
		"/api/status": `{"success":true,"data":{
			"version":"v1.0.0-rc.19","quota_per_unit":500000,
			"turnstile_check":false,"checkin_enabled":true}}`,
	})
	defer srv.Close()

	got, err := Detect(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Family != FamilyNewAPI {
		t.Fatalf("家族 = %s，期望 %s", got.Family, FamilyNewAPI)
	}
	if got.Version != "v1.0.0-rc.19" {
		t.Errorf("版本 = %q", got.Version)
	}
	// ⚠️ 逐站读取而非写死：upstream-d.invalid 是 500000，别家不一定（04 §2）
	if got.QuotaPerUnit != 500000 {
		t.Errorf("quota_per_unit = %v，期望 500000", got.QuotaPerUnit)
	}
	if !got.NoShield {
		t.Error("turnstile_check=false 应判为无盾")
	}
	// Detect 只归族，不确定用户 ID 头名（由 Authenticate fan-out，04 §2）
	if got.UserIDHeader != "" {
		t.Errorf("Detect 不该确定 UserIDHeader，得到 %q", got.UserIDHeader)
	}
}

// 开盾站点必须被识别出来：服务端采集对它不可行，须转人工录入（04 §6）。
func TestDetectNewAPIWithShield(t *testing.T) {
	srv := fakeSite(map[string]string{
		"/api/status": `{"data":{"quota_per_unit":500000,"turnstile_check":true}}`,
	})
	defer srv.Close()

	got, _ := Detect(context.Background(), srv.Client(), srv.URL)
	if got.Family != FamilyNewAPI {
		t.Fatalf("家族 = %s", got.Family)
	}
	if got.NoShield {
		t.Error("turnstile_check=true 应判为**开盾**——服务端采集不可行（04 §6）")
	}
}

func TestDetectSub2API(t *testing.T) {
	srv := fakeSite(map[string]string{
		// Sub2API 用 {code,message,data} envelope（04 §2）
		"/api/v1/settings/public": `{"code":0,"message":"ok","data":{
			"site_name":"某站","turnstile_enabled":false,"version":"0.1.163"}}`,
	})
	defer srv.Close()

	got, err := Detect(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Family != FamilySub2API {
		t.Fatalf("家族 = %s，期望 %s", got.Family, FamilySub2API)
	}
	if got.Version != "0.1.163" {
		t.Errorf("版本 = %q", got.Version)
	}
	if !got.NoShield {
		t.Error("turnstile_enabled=false 应判为无盾")
	}
}

func TestDetectASXS(t *testing.T) {
	srv := fakeSite(map[string]string{
		"/api/public/site-config": `{"data":{"iss":"ampmanager","version":"1.2"}}`,
	})
	defer srv.Close()

	got, err := Detect(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Family != FamilyASXS {
		t.Fatalf("家族 = %s，期望 %s", got.Family, FamilyASXS)
	}
}

// ASXS 的 ampmanager 指纹可能出现在非 JSON 响应里，也要能认出来。
func TestDetectASXSFromRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/public/site-config" {
			_, _ = w.Write([]byte(`not-json but mentions ampmanager somewhere`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, err := Detect(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Family != FamilyASXS {
		t.Fatalf("家族 = %s，期望 %s（指纹在原文里）", got.Family, FamilyASXS)
	}
}

// 顺序命中即停：同时具备 NewAPI 与 Sub2API 特征时按 04 §2 的顺序归 NewAPI。
func TestDetectStopsAtFirstMatch(t *testing.T) {
	srv := fakeSite(map[string]string{
		"/api/status":             `{"data":{"quota_per_unit":100}}`,
		"/api/v1/settings/public": `{"data":{"site_name":"x"}}`,
	})
	defer srv.Close()

	got, _ := Detect(context.Background(), srv.Client(), srv.URL)
	if got.Family != FamilyNewAPI {
		t.Fatalf("家族 = %s，期望 %s（顺序 1 优先）", got.Family, FamilyNewAPI)
	}
}

// 全未命中必须是 unknown，**不能猜**：猜错会让后续所有字段映射都错（04 §7）。
func TestDetectUnknownDoesNotGuess(t *testing.T) {
	srv := fakeSite(map[string]string{
		"/": `<html>某个不认识的站</html>`,
	})
	defer srv.Close()

	got, err := Detect(context.Background(), srv.Client(), srv.URL)
	if got.Family != FamilyUnknown {
		t.Fatalf("家族 = %s，期望 unknown（不得猜）", got.Family)
	}
	// 全部端点 404 → 有网络层面的失败，应带出错误供运维区分
	// "站点不可达"与"可达但不属已知家族"
	if err == nil {
		t.Error("全部探测失败时应返回错误，便于区分不可达与未知家族")
	}
}

// 站点返回 HTML 首页（非 JSON）不应 panic 或误判。
func TestDetectHandlesNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>hello</body></html>`))
	}))
	defer srv.Close()

	got, err := Detect(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("非 JSON 响应不应报错: %v", err)
	}
	if got.Family != FamilyUnknown {
		t.Fatalf("家族 = %s，期望 unknown", got.Family)
	}
}

// envelope 剥离：data 层与顶层都要能取到字段。
func TestUnwrapData(t *testing.T) {
	withEnv := map[string]any{"code": 0.0, "data": map[string]any{"k": "v"}}
	if got := unwrapData(withEnv); got["k"] != "v" {
		t.Errorf("未剥离 envelope: %v", got)
	}
	flat := map[string]any{"k": "v"}
	if got := unwrapData(flat); got["k"] != "v" {
		t.Errorf("无 envelope 时应原样返回: %v", got)
	}
	if got := unwrapData(nil); got == nil {
		t.Error("nil 输入应返回空 map 而非 nil，避免调用方取值 panic")
	}
}

func TestDetectRespectsContext(t *testing.T) {
	srv := fakeSite(map[string]string{"/api/status": `{"data":{"quota_per_unit":1}}`})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := Detect(ctx, srv.Client(), srv.URL)
	if err == nil {
		t.Error("已取消的 ctx 应导致失败，而不是继续打上游")
	}
}
