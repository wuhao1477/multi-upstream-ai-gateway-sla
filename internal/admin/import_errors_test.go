package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// Invalid uploads and read failures are controlled inputs, not upstream fixtures.
// Import diagnostics must distinguish an empty backup without exposing input.
func TestImportBackupDiagnostics(t *testing.T) {
	s := NewServer(nil, "test-admin", nil, nil)
	mux := http.NewServeMux()
	s.ImportRoutes(mux)
	for _, tc := range []struct {
		name    string
		body    io.Reader
		status  int
		message string
	}{
		{"no_sites", strings.NewReader(`{"accounts":{"accounts":[]}}`), 400,
			"导出文件里没有站点（accounts.accounts 为空）"},
		{"invalid_json", strings.NewReader(`{"accounts":{"accounts":"test-error-secret-marker"}}`), 400,
			"解析 all-api-hub 导出失败（JSON 格式无效）"},
		{"trailing_json", strings.NewReader(`{} {"access_token":"test-error-secret-marker"}`), 400,
			"解析 all-api-hub 导出失败（JSON 格式无效）"},
		{"read_failure", iotest.ErrReader(errors.New("test-error-secret-marker")), 400,
			"读取 all-api-hub 导出失败: 网络读取或请求失败"},
		{"too_large", strings.NewReader(strings.Repeat(" ", (32<<20)+1)), 413,
			"请求体超过大小限制"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/import/all-api-hub", tc.body)
			req.Header.Set("Authorization", "Bearer test-admin")
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d", rec.Code, tc.status)
			}
			if strings.Contains(rec.Body.String(), "test-error-secret-marker") {
				t.Fatal("import error exposed request data or the raw read failure")
			}
			var result struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error != tc.message {
				t.Errorf("error=%q want=%q", result.Error, tc.message)
			}
		})
	}
}
