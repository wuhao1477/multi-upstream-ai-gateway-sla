package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// `/admin/version` 的三件事：挂上了、要令牌、空值不回空串。
//
// 这里的 httptest 起的是**我们自己的 mux**，不是假上游 —— 被测对象就是这段
// handler 与它的路由注册，不存在"造一个别人家站点"的问题（CLAUDE.md §1）。
//
// 为什么值得有：界面把这个数当"正在跑哪一版"来显示。路由漏注册会 404、
// 忘了包 requireToken 会把版本号裸露出去、Version 没注入会显示空白 ——
// 三种都不会让别的用例红，而第三种在界面上看起来像"接口坏了"。
func getVersionResp(t *testing.T, s *Server, auth string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/admin/version", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var body struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body.Version
}

func TestVersionEndpointReturnsInjectedVersion(t *testing.T) {
	s := NewServer(nil, "t0k", nil, nil)
	s.Version = "v1.0.7"
	code, got := getVersionResp(t, s, "Bearer t0k")
	if code != http.StatusOK {
		t.Fatalf("带正确令牌的 /admin/version 应 200，实际 %d", code)
	}
	if got != "v1.0.7" {
		t.Errorf("应回注入的版本 v1.0.7，实际 %q", got)
	}
}

// 没注入时回 "dev" 而不是空串：空串在侧栏里渲染成什么都没有，
// 与"接口坏了"无从区分，而 main.version 的零值本来就是 "dev"。
func TestVersionEndpointFallsBackToDev(t *testing.T) {
	s := NewServer(nil, "t0k", nil, nil)
	if _, got := getVersionResp(t, s, "Bearer t0k"); got != "dev" {
		t.Errorf("未注入版本时应回 dev，实际 %q", got)
	}
}

// 反向哨兵：只有正向断言的话，把路由挂到 requireToken 外面也照样绿。
func TestVersionEndpointRequiresToken(t *testing.T) {
	s := NewServer(nil, "t0k", nil, nil)
	s.Version = "v1.0.7"
	if code, got := getVersionResp(t, s, ""); code != http.StatusUnauthorized || got != "" {
		t.Errorf("无令牌应 401 且不回版本号，实际 %d / %q", code, got)
	}
	if code, _ := getVersionResp(t, s, "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("错令牌应 401，实际 %d", code)
	}
}
