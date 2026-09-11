package store

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestPoolConfigReadOnlyMode(t *testing.T) {
	const dsn = "postgres://user:pass@127.0.0.1:5432/db?sslmode=disable"
	readOnly, err := poolConfig(dsn, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := readOnly.ConnConfig.RuntimeParams["default_transaction_read_only"]; got != "on" {
		t.Fatalf("只读连接参数 = %q，期望 on", got)
	}
	readWrite, err := poolConfig(dsn, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readWrite.ConnConfig.RuntimeParams["default_transaction_read_only"]; ok {
		t.Fatal("普通连接池不应强制只读")
	}
}

// 采集 worker 每人峰值占两条连接（渠道锁 + 临时连接），pgx 在 ≤4 核机器上
// 默认只给 4 条，4 个 worker 会把池占满并阻塞到整渠道 120s 超时。
// parsed 直接传入，不依赖跑测试这台机器的核数 —— 否则 16 核 runner 上恒绿。
func TestMaxConnsRaisesSmallDefault(t *testing.T) {
	const dsn = "postgres://user:pass@127.0.0.1:5432/db?sslmode=disable"
	if got := maxConns(dsn, 4); got != MinPoolConns {
		t.Fatalf("4 核默认值被抬到 %d，期望 %d", got, MinPoolConns)
	}
	if got := maxConns(dsn, 32); got != 32 {
		t.Fatalf("已经够大的默认值被改成了 %d，期望保持 32", got)
	}
}

// 显式配置说了算：抬下限不能覆盖运维刻意调小的值。
func TestMaxConnsKeepsExplicitDSNValue(t *testing.T) {
	const dsn = "postgres://user:pass@127.0.0.1:5432/db?sslmode=disable&pool_max_conns=3"
	if got := maxConns(dsn, 3); got != 3 {
		t.Fatalf("MaxConns = %d，期望保留 DSN 显式写的 3", got)
	}
	cfg, err := poolConfig(dsn, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 3 {
		t.Fatalf("poolConfig 后 MaxConns = %d，期望 3", cfg.MaxConns)
	}
}

// 死锁的真库复现：周期采集每个 worker 要同时握两条连接 —— 一条是整轮持有的
// 渠道 advisory lock，一条是临时的凭证读取 / host 限速预留 / 结果写入。
// pgx 默认 MaxConns = max(4, NumCPU)，≤4 核机器上正好 4：4 个 worker 拿走锁
// 连接后池就空了，临时 Acquire 阻塞到整渠道 120s 超时，整轮采集全军覆没。
//
// 为什么必须打真库：被验的是 pgxpool 对真实连接的排队行为，不是我们自己的
// 算术。sqlmock 里"池满了会阻塞"这件事是我写进去的，验它等于验我自己。
//
// ⚠️ 两个池都**显式**给 pool_max_conns，不用裸 DSN 走默认值。裸 DSN 的默认是
// max(4, NumCPU)：11 核开发机上本来就有 11 条，第二半会因为核多而恒绿 ——
// 我第一版就这么写的，把下限删掉跑 harness 照样全绿。"下限有没有被应用"
// 由 TestMaxConnsRaisesSmallDefault 回答（它直接传 parsed=4，不看核数）；
// 本用例只回答"4 条会饿死、MinPoolConns 条够用"这两件真库事实。
func TestPoolMaxConnsFitsConcurrentCollectionWorkers(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库（由 verify/test-migrate.sh 提供）")
	}
	const workers = 4 // = collection.maxConcurrentChannels

	// 先证明 bug 是真的：压到 4 条，即 ≤4 核机器上 pgx 会给的数。
	starved, err := NewPool(context.Background(), withParam(t, dsn, "pool_max_conns", "4"))
	if err != nil {
		t.Fatalf("建 4 连接池: %v", err)
	}
	defer starved.Close()
	if got := holdThenAcquire(t, starved, workers); got == nil {
		t.Fatal("4 条连接竟然喂饱了 2×workers 次并发 Acquire —— " +
			"本用例失去意义，请确认 worker 数与持锁模型没变")
	}

	// 再证明 MinPoolConns 这个数确实够 4 个 worker 用。
	sized, err := NewPool(context.Background(),
		withParam(t, dsn, "pool_max_conns", strconv.Itoa(MinPoolConns)))
	if err != nil {
		t.Fatalf("建 %d 连接池: %v", MinPoolConns, err)
	}
	defer sized.Close()
	if got := holdThenAcquire(t, sized, workers); got != nil {
		t.Fatalf("MinPoolConns=%d 下仍拿不到临时连接: %v", MinPoolConns, got)
	}
}

// withParam 往 DSN 追加一个连接参数。harness 给的 DSN 不一定带 query
// string，直接拼 "&" 会把参数拼进库名（踩过：database "sla&pool_max_conns=4"）。
func withParam(t *testing.T, dsn, key, value string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 SLA_TEST_DSN: %v", err)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// holdThenAcquire 模拟 n 个 worker 各握住一条渠道锁连接后，**同时**再各要一条
// 临时连接，返回其中任一失败（nil 表示 n 条都拿到了）。
//
// 必须并发要 n 条，不能只要 1 条：只要 1 条的话池里有 n+1 条就够，
// 于是 MinPoolConns 被压到 5（< 2n）时照样绿 —— 这版我写错过一次。
// 真实形态是 4 个 worker 各自在读凭证 / 预留 host 时隙 / 写采集结果，
// 峰值就是 2n 条同时在手。
func holdThenAcquire(t *testing.T, pool *Pool, n int) error {
	t.Helper()
	for range n {
		_, release, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("占用渠道锁连接: %v", err)
		}
		defer release()
	}
	// 2s 远短于整渠道 120s 超时：池够大就是立刻返回，不够大就是等到死。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errs := make(chan error, n)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := pool.Acquire(ctx)
			errs <- err
			if err == nil {
				defer release()
				<-done // 拿到的先别还，否则会互相接力而掩盖池不够
			}
		}()
	}
	var firstErr error
	for range n {
		if err := <-errs; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	close(done)
	wg.Wait()
	return firstErr
}
