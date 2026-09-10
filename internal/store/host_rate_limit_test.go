package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestHostRequestLimiterSerializesIndependentInstances(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	ctx := context.Background()
	poolA, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("建连接池 A: %v", err)
	}
	defer poolA.Close()
	poolB, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("建连接池 B: %v", err)
	}
	defer poolB.Close()

	const host = "host-rate-limit-test.example.invalid"
	clearHostRateLimitRows(t, poolA, host)
	defer clearHostRateLimitRows(t, poolA, host)

	limiters := []*HostRequestLimiter{
		NewHostRequestLimiter(poolA),
		NewHostRequestLimiter(poolB),
	}
	start := make(chan struct{})
	elapsed := make(chan time.Duration, len(limiters))
	errs := make(chan error, len(limiters))
	var wg sync.WaitGroup
	for _, limiter := range limiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			began := time.Now()
			err := limiter.Wait(ctx, host, 120*time.Millisecond)
			elapsed <- time.Since(began)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(elapsed)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("等待 host 时隙: %v", err)
		}
	}

	var longest time.Duration
	for duration := range elapsed {
		if duration > longest {
			longest = duration
		}
	}
	if longest < 100*time.Millisecond {
		t.Fatalf("两个独立 limiter 最长只等待 %s，未共享 120ms host 间隔", longest)
	}
}

func TestHostRequestLimiterDoesNotBlockDifferentHosts(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const hostA = "host-rate-limit-a.example.invalid"
	const hostB = "host-rate-limit-b.example.invalid"
	clearHostRateLimitRows(t, pool, hostA, hostB)
	defer clearHostRateLimitRows(t, pool, hostA, hostB)

	limiter := NewHostRequestLimiter(pool)
	if err := limiter.Wait(ctx, hostA, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := limiter.Wait(ctx, hostB, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > 100*time.Millisecond {
		t.Fatalf("不同 host 被互相阻塞了 %s", elapsed)
	}
}

func TestHostRequestLimiterHonorsCancellation(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const host = "host-rate-limit-cancel.example.invalid"
	clearHostRateLimitRows(t, pool, host)
	defer clearHostRateLimitRows(t, pool, host)

	limiter := NewHostRequestLimiter(pool)
	if err := limiter.Wait(ctx, host, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = limiter.Wait(waitCtx, host, 500*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("取消后的错误 = %v，期望 context deadline exceeded", err)
	}
	if elapsed := time.Since(began); elapsed > 200*time.Millisecond {
		t.Fatalf("取消后等待了 %s，未及时退出", elapsed)
	}
}

func clearHostRateLimitRows(t *testing.T, pool *Pool, hosts ...string) {
	t.Helper()
	ctx := context.Background()
	conn, release, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := conn.Exec(ctx,
		`DELETE FROM collector_host_rate_limits WHERE host = ANY($1)`, hosts); err != nil {
		t.Fatalf("清理 host 时隙: %v", err)
	}
}
