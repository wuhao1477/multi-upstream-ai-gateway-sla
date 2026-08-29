package admin

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// webFS 嵌入前端构建产物（web/ 下的 Vue 工程，vite build 落到 webdist/）。
//
// 前端与后端**源码分离、产物合一**：web/ 是独立的 Vite 工程（TS + Vue3 + Pinia），
// 构建结果编进同一个二进制、同一个镜像、同一个容器。这样 /admin/* 仍然整体
// 不经 Caddy 代理（06 §1 的边界不变），内网/离线环境也不依赖任何外链或第二个容器。
//
// 用 all: 前缀是**必需的**：webdist/ 里有一个被 git 跟踪的 .gitignore 占位文件
// （`*` + `!.gitignore`，让目录进版本库而内容不进），而 go:embed 的模式
// 匹配不到任何文件就是编译错误 —— 刚 clone 还没 npm run build 的仓库
// 靠这个占位文件才能 go build 过。普通模式会跳过点号开头的文件，收不到它。
// 对应地，下面的处理器**必须拒绝点号开头的路径**，别把它当静态资源发出去。
//
//go:embed all:webdist
var webFS embed.FS

// uiPrefix 是界面的挂载点。与 vite.config.ts 的 base 必须一致 ——
// 不一致的话 index.html 里引用的 /admin/ui/assets/xxx.js 会 404，页面白屏。
const uiPrefix = "/admin/ui/"

// WebRoutes 挂载管理界面。
//
// ⚠️ **界面本身不做鉴权**（它是静态资源，没有秘密），
// 但它调用的每个 /admin/* 接口都要令牌 —— 令牌由用户在页面里填写、
// 存 localStorage，不写进服务端。
// 这与 06 §1 的边界一致：整个 /admin/* 前缀（含本页面）都不经 Caddy 代理，
// 只在容器网络/本机可达。
func (s *Server) WebRoutes(mux *http.ServeMux) {
	dist, err := fs.Sub(webFS, "webdist")
	if err == nil {
		if _, err2 := fs.Stat(dist, "index.html"); err2 != nil {
			// 只有占位文件、没有 index.html：说明构建阶段漏了 npm run build。
			// 报清楚"缺什么、怎么补"，而不是让运维对着 404 猜。
			err = err2
		}
	}
	if err != nil {
		// 嵌入失败是构建问题，此时让 /admin/ui 明确报错而不是静默 404
		msg := "管理界面未正确嵌入（前端产物缺失，需先在 web/ 下执行 npm ci && npm run build）: " +
			err.Error()
		mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, msg, http.StatusInternalServerError)
		})
		mux.HandleFunc("GET "+uiPrefix, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, msg, http.StatusInternalServerError)
		})
		s.faviconRoute(mux)
		return
	}

	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "读取管理界面失败: "+err.Error(), http.StatusInternalServerError)
		})
		s.faviconRoute(mux)
		return
	}

	serveIndex := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 不缓存 index.html：它引用的是带 hash 的资源名，每次发布都变。
		// 缓存住它会让运维打开旧壳子去引用已经不存在的资源 —— 白屏且难查。
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(index)
	}

	// 不带尾斜杠的入口也要能开：运维手打的地址通常没有尾斜杠。
	// 不做 301 跳转 —— vue-router 的 base 对两者都认，少一跳少一处出错的地方。
	mux.HandleFunc("GET /admin/ui", serveIndex)

	// 前缀路由兜底：既发静态资源，也承担 SPA 的 history 回退。
	mux.HandleFunc("GET "+uiPrefix, func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, uiPrefix)
		if rel == "" {
			serveIndex(w, r)
			return
		}
		// 点号开头一律拒绝：webdist/.gitignore 是给 go:embed 用的占位文件，
		// 不是要对外提供的资源。整段路径逐段检查，'..' 也一并挡掉。
		clean := path.Clean(rel)
		if clean != rel || hasDotSegment(clean) {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(dist, clean)
		if err != nil {
			// 找不到就当路由：/admin/ui/detail 这类 history 路径回 index.html，
			// 由前端路由接管。这是 SPA 刷新能工作的前提。
			serveIndex(w, r)
			return
		}
		if strings.HasPrefix(clean, "assets/") {
			// 资源名自带内容 hash，可以放心长缓存。
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		// 零值 modtime：不发 Last-Modified（嵌入文件没有有意义的时间戳），
		// 但 ServeContent 仍会处理 Range 与 If-None-Match。
		http.ServeContent(w, r, clean, time.Time{}, bytes.NewReader(data))
	})

	s.faviconRoute(mux)
}

// hasDotSegment 判断路径里是否有以点号开头的段。
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// faviconRoute 提供内联 favicon。
//
// 浏览器会自动请求它，没有就在控制台留一条 404。
// 那条 404 无害，但会淹没真正的错误 —— 排障时"控制台干净"本身有价值。
func (s *Server) faviconRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" ` +
			`viewBox="0 0 16 16"><rect width="16" height="16" rx="3" fill="#4c8eda"/>` +
			`<text x="8" y="12" font-size="11" text-anchor="middle" fill="#fff">P</text></svg>`))
	})
}
