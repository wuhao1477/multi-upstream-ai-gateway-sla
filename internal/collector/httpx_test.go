package collector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientDoStopsBeforeRequestWhenSharedHostLimiterFails(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested = true
	}))
	defer server.Close()

	waitErr := errors.New("host limiter unavailable")
	client := NewClient(200 * time.Millisecond)
	client.WaitHost = func(context.Context, string, time.Duration) error {
		return waitErr
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); !errors.Is(err, waitErr) {
		t.Fatalf("Client.Do 错误 = %v，期望保留共享限速错误", err)
	}
	if requested {
		t.Fatal("共享 host limiter 失败后仍向上游发出了请求")
	}
}

func TestDetectUsesCollectorClientHostLimiter(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested = true
	}))
	defer server.Close()

	waitErr := errors.New("host limiter unavailable")
	client := NewClient(200 * time.Millisecond)
	client.HC = server.Client()
	client.WaitHost = func(context.Context, string, time.Duration) error {
		return waitErr
	}
	if _, err := Detect(context.Background(), client, server.URL); !errors.Is(err, waitErr) {
		t.Fatalf("Detect 错误 = %v，期望保留共享限速错误", err)
	}
	if requested {
		t.Fatal("站型探测绕过共享 host limiter 发出了请求")
	}
}

func TestGetJSONAuthPreservesHTTPStatusAndRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient(0)
	client.HC = server.Client()
	_, _, err := client.getJSONAuth(context.Background(), Session{BaseURL: server.URL}, "/quota")
	status, retryAfter, ok := HTTPFailure(err)
	if !ok {
		t.Fatalf("429 错误未保留 HTTP 元数据：%v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("HTTP 状态 = %d，期望 429", status)
	}
	if retryAfter != 120*time.Second {
		t.Fatalf("Retry-After = %s，期望 2 分钟", retryAfter)
	}
}

func TestGetJSONAuthParsesRetryAfterHTTPDate(t *testing.T) {
	retryAt := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAt.Format(http.TimeFormat))
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient(0)
	client.HC = server.Client()
	_, _, err := client.getJSONAuth(context.Background(), Session{BaseURL: server.URL}, "/quota")
	_, retryAfter, ok := HTTPFailure(err)
	if !ok {
		t.Fatalf("429 错误未保留 HTTP 元数据：%v", err)
	}
	if retryAfter < 118*time.Second || retryAfter > 120*time.Second {
		t.Fatalf("HTTP-date Retry-After = %s，期望约 2 分钟", retryAfter)
	}
}
