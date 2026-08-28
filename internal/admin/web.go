package admin

import (
	"embed"
	"net/http"
)

// webFS 嵌入管理界面。单文件自包含（无构建步骤、无 CDN 依赖）——
// 09 §7 定"一期 UI 可用最简形态"，而一个纯静态页面既满足"web 端可操作"，
// 又不引入前端工具链与其版本跟随负担。
//
//go:embed web/index.html
var webFS embed.FS

// WebRoutes 挂载管理界面。
//
// ⚠️ **界面本身不做鉴权**（它是静态资源，没有秘密），
// 但它调用的每个 /admin/* 接口都要令牌 —— 令牌由用户在页面里填写、
// 存 localStorage，不写进服务端。
// 这与 06 §1 的边界一致：整个 /admin/* 前缀（含本页面）都不经 Caddy 代理，
// 只在容器网络/本机可达。
func (s *Server) WebRoutes(mux *http.ServeMux) {
	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		// 嵌入失败是构建问题，此时让 /admin/ui 明确报错而不是静默 404
		mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "管理界面未正确嵌入: "+err.Error(),
				http.StatusInternalServerError)
		})
		return
	}
	serve := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 不缓存：P1 阶段页面还在改，缓存会让运维看到旧版本而以为功能没上
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(page)
	}
	mux.HandleFunc("GET /admin/ui", serve)
	mux.HandleFunc("GET /admin/ui/", serve)

	// 内联 favicon：浏览器会自动请求它，没有就在控制台留一条 404。
	// 那条 404 无害，但会淹没真正的错误 —— 排障时"控制台干净"本身有价值。
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" ` +
			`viewBox="0 0 16 16"><rect width="16" height="16" rx="3" fill="#4c8eda"/>` +
			`<text x="8" y="12" font-size="11" text-anchor="middle" fill="#fff">P</text></svg>`))
	})
}
