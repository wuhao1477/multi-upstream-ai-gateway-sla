package admin

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestWebSecurityHeaders exercises the actual embedded SPA and its registered
// routes. Removing headers from either history fallback or static responses must
// fail; the frontend build is required just as it is for the release binary.
func TestWebSecurityHeaders(t *testing.T) {
	index, err := fs.ReadFile(webFS, "webdist/index.html")
	if err != nil {
		t.Fatal("frontend build required: run make web before testing SPA responses")
	}
	mux := http.NewServeMux()
	testServer().WebRoutes(mux)
	const policy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; " +
		"base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
	for _, path := range []string{"/admin/ui", "/admin/ui/", "/admin/ui/import", "/admin/ui/index.html"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("SPA response: status=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get("Content-Security-Policy") != policy {
				t.Error("SPA response must enforce the approved same-origin CSP")
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("SPA response must disable MIME sniffing")
			}
		})
	}
	assets := regexp.MustCompile(`(?:src|href)="(/admin/ui/[^\"]+)"`).FindAllSubmatch(index, -1)
	if len(assets) < 2 {
		t.Fatal("built SPA must reference its script and stylesheet")
	}
	paths := []string{"/favicon.ico"}
	for _, match := range assets {
		paths = append(paths, string(match[1]))
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK || strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("static asset: status=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("static asset response must disable MIME sniffing")
			}
		})
	}
}
