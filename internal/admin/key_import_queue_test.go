package admin

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// ⚠️ 本文件注入的是一个**结果序列**（第 1 个账号 401、第 2 个账号成功…），
// 不是一个假上游。按 CLAUDE.md §1 的例外判据——"真依赖能不能按需产出这个输入"
// ——真站点没法在你要的那一刻恰好回 401、下一刻恰好回 429；而被测对象正是
// **编排**：一个账号栽了另一个跑不跑、什么错值得重排、预算按谁算。
// 协议形态一个字节都没在这里被断言，那部分的举证责任仍在 verify/ui-stack.sh
// 的真上游那一段。

// fakeDB 只为让 runKeyImportTask 能取到"一条连接"。它不执行任何 SQL ——
// 这段代码路径里唯一碰库的是注入进来的 ImportKeys，而那正是被替换掉的那个。
type fakeDB struct{ err error }

func (d fakeDB) Acquire(context.Context) (*pgx.Conn, func(), error) {
	if d.err != nil {
		return nil, nil, d.err
	}
	return nil, func() {}, nil
}

func queueServer(t *testing.T, importKeys func(int64) (collector.KeyImportResult, error)) *Server {
	t.Helper()
	return &Server{
		DB:     fakeDB{},
		Logger: slog.New(slog.NewTextHandler(discard{}, nil)),
		queue:  newKeyImportQueue(),
		ImportKeys: func(
			_ context.Context, _ *pgx.Conn, _, accountID int64, _ collector.KeyImportRequest,
		) (collector.KeyImportResult, error) {
			return importKeys(accountID)
		},
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func accounts(ids ...int64) []store.Account {
	out := make([]store.Account, 0, len(ids))
	for _, id := range ids {
		out = append(out, store.Account{ID: id, ChannelID: id, Status: "active"})
	}
	return out
}

// 这是本次修复的核心断言：**一个账号失败不再中断整批**。
//
// 旧实现在 `err != nil` 时 break 整个循环。2026-09-16 对真上游实测：选 10 个
// 渠道，第 2 个站鉴权失败，后面 8 个站一个字节都没发出去，而响应里
// deferred_accounts 还是 0 —— 界面上读起来就是"全都同步完了"。
func TestKeyImportJobKeepsGoingAfterOneAccountFails(t *testing.T) {
	var touched []int64
	s := queueServer(t, func(accountID int64) (collector.KeyImportResult, error) {
		touched = append(touched, accountID)
		if accountID == 2 {
			// 401 是确定性错误：不重排，但也不该拖累别的站点
			return collector.KeyImportResult{}, &collector.HTTPError{StatusCode: 401}
		}
		return collector.KeyImportResult{Found: 1, Imported: 1, SecretResolves: 1}, nil
	})
	job, _ := s.queue.begin(4)
	s.runKeyImportJob(context.Background(), job, accounts(1, 2, 3, 4))

	got, _ := s.queue.snapshot(job.ID)
	if len(touched) != 4 {
		t.Fatalf("四个账号都该被跑到，实际只跑了 %v", touched)
	}
	if got.Count != 4 || got.Done != 4 {
		t.Fatalf("每个账号都该留下一条结果，实际 count=%d done=%d", got.Count, got.Done)
	}
	if got.Imported != 3 {
		t.Fatalf("另外三个账号应各导入 1 把，实际 imported=%d", got.Imported)
	}
	if got.Status != "done" || got.Pending != 0 {
		t.Fatalf("批次应正常结束，实际 status=%q pending=%d", got.Status, got.Pending)
	}
	// 失败那条必须**指名道姓**：只给一个总数的话，"哪个站要去修凭证"还得人去翻日志
	var failed *keyImportItem
	for i := range got.Items {
		if got.Items[i].AccountID == 2 {
			failed = &got.Items[i]
		}
	}
	if failed == nil || failed.Status != "failed" || failed.Error == "" {
		t.Fatalf("账号 2 应留下一条带原因的 failed，实际 %+v", failed)
	}
}

// 账号级计数与 Key 级计数是两把尺子，汇总里必须都有。
//
// 2026-09-16 对真上游实测撞到的第二个洞：10 个渠道里 5 个站整个连不上，而汇总
// 里 `failed` 是 2 —— 那个 2 来自唯一一个连得上的站里失败的两把 Key；5 个连不上
// 的站一把 Key 都没轮到，对 `failed` 的贡献是 0。于是"失败 2"读起来像"只有两处
// 小问题"，而事实是半数站点根本没采到。
func TestKeyImportSummaryCountsFailedAccountsSeparatelyFromFailedKeys(t *testing.T) {
	s := queueServer(t, func(accountID int64) (collector.KeyImportResult, error) {
		switch accountID {
		case 1, 2, 3:
			// 整个账号连不上：一把 Key 都没轮到，Key 级计数全是 0
			return collector.KeyImportResult{}, &collector.HTTPError{
				StatusCode: 401, Message: "上游返回 401",
			}
		default:
			// 连得上，但里面两把 Key 失败了
			return collector.KeyImportResult{Found: 2, Failed: 2, SecretResolves: 2}, nil
		}
	})
	job, _ := s.queue.begin(4)
	s.runKeyImportJob(context.Background(), job, accounts(1, 2, 3, 4))
	got, _ := s.queue.snapshot(job.ID)

	if got.Failed != 2 {
		t.Fatalf("Key 级失败应是 2 把，实际 %d", got.Failed)
	}
	if got.FailedAccounts != 3 {
		t.Fatalf("账号级失败应是 3 个，实际 %d —— 汇总只报 Key 级的话，"+
			"三个整站连不上会显示成「失败 2」", got.FailedAccounts)
	}
}

// 停用的账号记进 SkippedAccounts，不混进失败里：它不是故障，是人自己关掉的。
func TestKeyImportSummaryCountsSkippedAccounts(t *testing.T) {
	s := queueServer(t, func(int64) (collector.KeyImportResult, error) {
		return collector.KeyImportResult{Found: 1, Imported: 1, SecretResolves: 1}, nil
	})
	job, _ := s.queue.begin(2)
	s.runKeyImportJob(context.Background(), job, []store.Account{
		{ID: 1, ChannelID: 1, Status: "disabled"},
		{ID: 2, ChannelID: 2, Status: "active"},
	})
	got, _ := s.queue.snapshot(job.ID)
	if got.SkippedAccounts != 1 || got.FailedAccounts != 0 {
		t.Fatalf("停用账号应记 skipped 而非 failed，实际 skipped=%d failed=%d",
			got.SkippedAccounts, got.FailedAccounts)
	}
	if got.Imported != 1 {
		t.Fatalf("另一个账号照常跑，实际 imported=%d", got.Imported)
	}
}

// 明文读取预算**按渠道算**。
//
// 上游那个"20 次 / 20 分钟"是按站点的，做成全批共享意味着 A 站花完了 B 站就
// 没得花 —— 那正是"单次只能同步 20 个账号"这条限制的由来，而它是我们自己
// 加的，不是上游要求的。
func TestKeyImportBudgetIsPerChannelNotPerBatch(t *testing.T) {
	var budgets []int
	s := queueServer(t, func(int64) (collector.KeyImportResult, error) { return collector.KeyImportResult{}, nil })
	s.ImportKeys = func(
		_ context.Context, _ *pgx.Conn, _, _ int64, request collector.KeyImportRequest,
	) (collector.KeyImportResult, error) {
		budgets = append(budgets, request.MaxSecretResolves)
		// 每个账号都把本渠道的预算吃光
		return collector.KeyImportResult{
			Found:          collector.DefaultKeySecretResolveLimit,
			Imported:       collector.DefaultKeySecretResolveLimit,
			SecretResolves: collector.DefaultKeySecretResolveLimit,
		}, nil
	}
	job, _ := s.queue.begin(3)
	// 三个账号分属三个渠道（accounts() 让 ChannelID == ID）
	s.runKeyImportJob(context.Background(), job, accounts(1, 2, 3))

	if len(budgets) != 3 {
		t.Fatalf("三个渠道都该拿到自己的预算，实际只发起 %d 次", len(budgets))
	}
	for i, b := range budgets {
		if b != collector.DefaultKeySecretResolveLimit {
			t.Fatalf("第 %d 个渠道拿到的预算是 %d，期望满额 %d —— 预算被跨渠道共享了",
				i+1, b, collector.DefaultKeySecretResolveLimit)
		}
	}
}

// 同一个渠道下的第二个账号把预算用光时记 deferred 而不是 failed：
// 两者要人做的事完全不同（等一会儿 vs 去修凭证）。
func TestKeyImportSecondAccountOnSameChannelIsDeferredNotFailed(t *testing.T) {
	s := queueServer(t, func(int64) (collector.KeyImportResult, error) {
		return collector.KeyImportResult{
			Found: 20, Imported: 20, SecretResolves: collector.DefaultKeySecretResolveLimit,
		}, nil
	})
	job, _ := s.queue.begin(2)
	// 两个账号**同一个渠道**
	s.runKeyImportJob(context.Background(), job, []store.Account{
		{ID: 1, ChannelID: 7, Status: "active"},
		{ID: 2, ChannelID: 7, Status: "active"},
	})
	got, _ := s.queue.snapshot(job.ID)
	if len(got.Items) != 2 {
		t.Fatalf("两个账号都该留下结果，实际 %d 条", len(got.Items))
	}
	if got.Items[1].Status != "deferred" {
		t.Fatalf("预算用尽应记 deferred（稍后再来），实际 %q", got.Items[1].Status)
	}
	if got.Failed != 0 {
		t.Fatalf("预算用尽不是失败，failed 应为 0，实际 %d", got.Failed)
	}
}

// 429 要退避重排，而且**排到队尾** —— 站点之间互不相干，没有理由让整条队列
// 陪一个正在退避的站点干等。
func TestKeyImportRateLimitedAccountRetriesAndOthersRunFirst(t *testing.T) {
	var order []int64
	attempts := map[int64]int{}
	s := queueServer(t, func(accountID int64) (collector.KeyImportResult, error) {
		order = append(order, accountID)
		attempts[accountID]++
		if accountID == 1 && attempts[1] == 1 {
			// Retry-After 给一个极短的值：被测的是"有没有按它排"，不是真去等 60 秒
			return collector.KeyImportResult{}, &collector.HTTPError{
				StatusCode: 429, RetryAfter: 10 * time.Millisecond,
			}
		}
		return collector.KeyImportResult{Found: 1, Imported: 1, SecretResolves: 1}, nil
	})
	job, _ := s.queue.begin(2)
	s.runKeyImportJob(context.Background(), job, accounts(1, 2))

	if len(order) != 3 {
		t.Fatalf("账号 1 该跑两次、账号 2 一次，实际顺序 %v", order)
	}
	if order[0] != 1 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("被限流的账号应排到队尾让别的站先跑，实际顺序 %v", order)
	}
	got, _ := s.queue.snapshot(job.ID)
	if got.Imported != 2 || got.Failed != 0 {
		t.Fatalf("重试之后两个账号都该成功，实际 imported=%d failed=%d", got.Imported, got.Failed)
	}
	for _, item := range got.Items {
		if item.AccountID == 1 && item.Attempts != 2 {
			t.Fatalf("账号 1 的结果应标明是第 2 次尝试得出的，实际 %d", item.Attempts)
		}
	}
}

// 确定性错误不重试：401 重排多少次都是同一个 401，重试只是把它再打一遍别人家站点。
func TestKeyImportDeterministicFailureIsNotRetried(t *testing.T) {
	calls := 0
	s := queueServer(t, func(int64) (collector.KeyImportResult, error) {
		calls++
		return collector.KeyImportResult{}, &collector.HTTPError{StatusCode: 401}
	})
	job, _ := s.queue.begin(1)
	s.runKeyImportJob(context.Background(), job, accounts(1))
	if calls != 1 {
		t.Fatalf("401 不该重试，实际打了 %d 次", calls)
	}
}

// 重试有上限：凭证彻底失效的账号不能把队列拖成永动机。
func TestKeyImportRetryStopsAtMaxAttempts(t *testing.T) {
	calls := 0
	s := queueServer(t, func(int64) (collector.KeyImportResult, error) {
		calls++
		return collector.KeyImportResult{}, &collector.HTTPError{
			StatusCode: 429, RetryAfter: time.Millisecond,
		}
	})
	job, _ := s.queue.begin(1)
	s.runKeyImportJob(context.Background(), job, accounts(1))
	if calls != keyImportMaxAttempts {
		t.Fatalf("应恰好尝试 %d 次，实际 %d 次", keyImportMaxAttempts, calls)
	}
	got, _ := s.queue.snapshot(job.ID)
	if got.Status != "done" || len(got.Items) != 1 || got.Items[0].Status != "failed" {
		t.Fatalf("耗尽重试后该落一条 failed 并正常收尾，实际 %+v", got.Items)
	}
}

// 进程退出时批次要停下并标成 canceled，而不是假装跑完了。
// 队列只在内存里，"没跑完"这件事必须说出来 —— 否则重启之后那批任务就是静默消失。
func TestKeyImportJobCanceledOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := queueServer(t, func(accountID int64) (collector.KeyImportResult, error) {
		if accountID == 1 {
			cancel()
		}
		return collector.KeyImportResult{Found: 1, Imported: 1, SecretResolves: 1}, nil
	})
	job, _ := s.queue.begin(3)
	s.runKeyImportJob(ctx, job, accounts(1, 2, 3))
	got, _ := s.queue.snapshot(job.ID)
	if got.Status != "canceled" {
		t.Fatalf("ctx 取消后批次应标 canceled，实际 %q", got.Status)
	}
	if got.Error == "" {
		t.Fatal("canceled 的批次必须带上原因，否则界面只看到一个停住的进度条")
	}
	// 没轮到的账号要有个数。队列在内存里，重启就没了 —— 不报出来的话，
	// 「被打断」与「跑完了、只是没什么可导」在界面上一模一样。
	// 3 个账号：第 1 个在跑的时候 ctx 被取消（它的结果要丢掉，算未处理），
	// 另外 2 个还在队里 —— 一个都不能漏报。
	if got.DeferredAccounts != 3 {
		t.Fatalf("被取消的批次要把没跑完的 3 个账号全报出来，实际 deferred_accounts=%d",
			got.DeferredAccounts)
	}
}

// 同时只跑一个批次：两批并发会各花一半限流预算，然后双双撞 429。
func TestKeyImportQueueRunsOneBatchAtATime(t *testing.T) {
	q := newKeyImportQueue()
	first, ok := q.begin(3)
	if !ok {
		t.Fatal("首个批次应能开始")
	}
	again, ok := q.begin(2)
	if ok {
		t.Fatal("已有批次在跑时不该再开一个")
	}
	if again.ID != first.ID {
		t.Fatalf("被拒时要回正在跑的那个批次 id（界面据此切过去），实际 %d", again.ID)
	}
	q.finish(first, "done", "")
	if _, ok := q.begin(1); !ok {
		t.Fatal("上一批结束后应能再开")
	}
}

// 历史裁剪不能把**正在跑**的那个裁掉：界面轮询会得到 404，看起来像批次凭空消失。
func TestKeyImportQueueNeverEvictsRunningJob(t *testing.T) {
	q := newKeyImportQueue()
	running, _ := q.begin(1)
	for i := 0; i < keyImportJobHistory+5; i++ {
		job, ok := q.begin(1)
		if !ok {
			// 第一个还在跑，后面的 begin 都会被拒 —— 直接造历史条目
			q.mu.Lock()
			q.nextID++
			done := &keyImportJob{ID: q.nextID, Status: "done"}
			q.jobs[done.ID] = done
			q.order = append(q.order, done.ID)
			q.mu.Unlock()
			continue
		}
		q.finish(job, "done", "")
	}
	if _, ok := q.snapshot(running.ID); !ok {
		t.Fatal("正在跑的批次被历史裁剪挤掉了")
	}
}

func TestKeyImportRetryDelayClassifiesErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{name: "429 值得等", err: &collector.HTTPError{StatusCode: 429}, retry: true},
		{name: "502 值得等", err: &collector.HTTPError{StatusCode: 502}, retry: true},
		{name: "401 不值得", err: &collector.HTTPError{StatusCode: 401}, retry: false},
		{name: "404 不值得", err: &collector.HTTPError{StatusCode: 404}, retry: false},
		{name: "超时值得等", err: context.DeadlineExceeded, retry: true},
		{name: "网络超时值得等", err: timeoutErr{}, retry: true},
		{name: "我们自己取消的不重试", err: context.Canceled, retry: false},
		{name: "说不清的错不重试", err: errors.New("站型不支持自动读取 Key 明文"), retry: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delay, retry := keyImportRetryDelay(tc.err, 1)
			if retry != tc.retry {
				t.Fatalf("keyImportRetryDelay(%v) retry=%v，期望 %v", tc.err, retry, tc.retry)
			}
			if retry && delay <= 0 {
				t.Fatalf("要重试就必须给一个正的等待时长，实际 %v", delay)
			}
		})
	}
}

// Retry-After 优先于我们自己的退避序列：那是上游明说的节奏，比我们猜的准。
func TestKeyImportRetryDelayHonorsRetryAfter(t *testing.T) {
	delay, retry := keyImportRetryDelay(
		&collector.HTTPError{StatusCode: 429, RetryAfter: 7 * time.Second}, 1)
	if !retry || delay != 7*time.Second {
		t.Fatalf("应采用上游给的 Retry-After=7s，实际 retry=%v delay=%v", retry, delay)
	}
}

// 退避要随次数变长，且有封顶 —— 否则第三次已经等到明天去了。
func TestKeyImportRetryDelayGrowsAndCaps(t *testing.T) {
	first, _ := keyImportRetryDelay(&collector.HTTPError{StatusCode: 500}, 1)
	second, _ := keyImportRetryDelay(&collector.HTTPError{StatusCode: 500}, 2)
	if second <= first {
		t.Fatalf("退避应随尝试次数变长，实际 %v → %v", first, second)
	}
	huge, _ := keyImportRetryDelay(&collector.HTTPError{StatusCode: 500}, 30)
	if huge != keyImportRetryCap {
		t.Fatalf("退避应封顶在 %v，实际 %v", keyImportRetryCap, huge)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }
func (timeoutErr) Temporary() bool {
	return true
}

var _ net.Error = timeoutErr{}
