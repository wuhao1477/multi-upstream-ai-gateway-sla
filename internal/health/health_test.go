package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func do(t *testing.T, h *Handler) (int, response) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body response
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return rec.Code, body
}

func TestHealthyWhenDBUpAndSnapshotLoaded(t *testing.T) {
	code, body := do(t, &Handler{
		DB:          fakePinger{},
		HasSnapshot: func() bool { return true },
	})
	if code != http.StatusOK || body.Status != "ok" {
		t.Fatalf("期望 200/ok，得到 %d/%s", code, body.Status)
	}
}

// PG 不可达必须返回非 200 而不是 panic（#1 完成标准）。
func TestUnavailableWhenDBDown(t *testing.T) {
	code, body := do(t, &Handler{
		DB:          fakePinger{err: errors.New("dial tcp: connection refused")},
		HasSnapshot: func() bool { return true },
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("PG 不可达应返回 503，得到 %d", code)
	}
	if body.DB == "ok" {
		t.Error("db 字段应报出错误原因")
	}
}

// 快照未加载时不接流量——没快照就无法决策（01 §5）。
func TestUnavailableWhenSnapshotMissing(t *testing.T) {
	code, body := do(t, &Handler{
		DB:          fakePinger{},
		HasSnapshot: func() bool { return false },
	})
	if code != http.StatusServiceUnavailable || body.Snapshot != "missing" {
		t.Fatalf("快照缺失应 503/missing，得到 %d/%s", code, body.Snapshot)
	}
}

// nil 依赖不得 panic —— 探针挂掉比报不健康更糟（Caddy 会拿不到任何信号）。
func TestNilDepsDoNotPanic(t *testing.T) {
	code, _ := do(t, &Handler{})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("依赖未配置应 503，得到 %d", code)
	}
}

// 固化 06 §6 的纪律：响应里必须写明本端点不代表上游可用性。
// 这条测试的作用是让"把渠道健康塞进 healthz"的改动显式地失败。
func TestResponseDisclaimsUpstreamAvailability(t *testing.T) {
	_, body := do(t, &Handler{DB: fakePinger{}, HasSnapshot: func() bool { return true }})
	if body.Note == "" {
		t.Fatal("响应应说明本端点不反映上游渠道可用性（06 §6 健康语义分层）")
	}
}
