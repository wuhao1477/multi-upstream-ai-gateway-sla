package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 「同步已有 Key」的后台队列。
//
// 为什么非得是后台：这个操作的时长由**上游**决定，不由我们决定。每把 Key 的
// 明文都要单独打一次上游（NewAPI 是 POST /api/token/{id}/key），而站点按 IP
// 限流（实测 20 次 / 20 分钟）。一个跨 30 个站点的批次因此可能要跑上小时级 ——
// 那个时长塞不进一个 HTTP 请求，也不该让人对着转圈的按钮等。
//
// 队列**只在内存里**，重启即丢（2026-09-16 定）。理由是这批任务的输入极廉价：
// 范围就是几十个渠道 id，重点一次就重建了；而落库要为它开一张表、一套迁移、
// 一套裁剪策略，换来的只是"重启后自动接着跑"。代价必须说清楚：**进程重启时
// 在跑的批次会消失**，界面上那条进度条也随之消失 —— 不是悄悄变慢，是没了。
//
// 同一时刻只跑一个批次。不是为了简单：并发跑两批会让"每站 20 次 / 20 分钟"
// 这个预算被两条互不知情的路径同时花掉，于是两批都在中途撞 429。
type keyImportQueue struct {
	mu      sync.Mutex
	jobs    map[int64]*keyImportJob
	order   []int64
	nextID  int64
	running *keyImportJob
}

// keyImportJobHistory 是内存里保留的批次数。
//
// 保留历史而不是只留当前那一个：这个操作最常见的结局是"大部分成功、两三个站
// 失败"，而失败的名单要在批次结束之后还看得见 —— 人多半是过一会儿才回来看的。
const keyImportJobHistory = 20

// keyImportJob 是一个批次的全部可见状态。
//
// 计数与 keyImportBatchResult 完全一致（同步端点原来返回的就是它），
// 于是界面上"跑完之后看到的东西"与从前逐字相同，只是晚一点到。
type keyImportJob struct {
	ID         int64      `json:"id"`
	Status     string     `json:"status"` // running | done | canceled
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Total 是这批要处理的账号数，Done 是已出结果的账号数。
	// 两个数一起给：只给百分比的话，"3/40" 与 "30/400" 在界面上一样长。
	Total int `json:"total"`
	Done  int `json:"done"`
	// Pending 是还排在队里的账号数（含等着退避重试的）。
	Pending int `json:"pending"`
	// NextRetryAt 是队首那个账号最早什么时候轮到它。批次卡在限流退避里时，
	// 界面要能回答"它是死了还是在等" —— 没有这个字段就只能猜。
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	// Error 是**整批**失败的原因（取不到连接之类），空串 = 批次本身没问题。
	// 单个账号的失败在 Items 里，不进这里。
	Error string `json:"error,omitempty"`
	keyImportBatchResult
}

// keyImportTask 是队列里的一项：一个账号一次尝试。
type keyImportTask struct {
	account   store.Account
	attempts  int
	notBefore time.Time
}

// keyImportMaxAttempts 是单个账号的最大尝试次数（含第一次）。
//
// 3 次而不是无限：限流退避的第三次已经在二十分钟开外，再往后重试的价值迅速
// 趋近于零，而代价是这批任务永远结束不了 —— 一个凭证彻底失效的账号会把整条
// 队列拖成一个永动机，持续打别人家站点。
const keyImportMaxAttempts = 3

// keyImportRetryBase 是没有 Retry-After 可依时的退避基数。
//
// 60 秒起步是照着上游那个窗口来的（实测 20 次 / 20 分钟 ≈ 每分钟一次）。
// 退避序列因此是 1 分钟 → 4 分钟，第三次失败就认账。
const keyImportRetryBase = time.Minute

// keyImportRetryCap 封顶。再长就不像"稍后重试"，像"这批卡住了"。
const keyImportRetryCap = 10 * time.Minute

func newKeyImportQueue() *keyImportQueue {
	return &keyImportQueue{jobs: map[int64]*keyImportJob{}}
}

// begin 登记一个新批次。已有批次在跑时返回那个批次，第二个返回值为 false。
func (q *keyImportQueue) begin(total int) (*keyImportJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running != nil {
		return q.running, false
	}
	q.nextID++
	job := &keyImportJob{
		ID: q.nextID, Status: "running", StartedAt: time.Now(),
		Total: total, Pending: total,
		keyImportBatchResult: keyImportBatchResult{Items: []keyImportItem{}},
	}
	q.jobs[job.ID] = job
	q.order = append(q.order, job.ID)
	// 裁掉最老的。只裁**已结束**的：正在跑的那个被裁掉之后，界面轮询会得到 404，
	// 看起来像批次凭空消失了。运行中的批次同一时刻只有一个，不会撑爆。
	for len(q.order) > keyImportJobHistory {
		oldest := q.order[0]
		if old, ok := q.jobs[oldest]; ok && old.Status == "running" {
			break
		}
		delete(q.jobs, oldest)
		q.order = q.order[1:]
	}
	q.running = job
	return job, true
}

func (q *keyImportQueue) finish(job *keyImportJob, status, failure string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	job.Status = status
	job.FinishedAt = &now
	// 没轮到就结束的那些记进 DeferredAccounts。队列只在内存里，重启就没了 ——
	// 「这批没跑完」必须有个数字说出来，否则它与「跑完了、只是没什么可导」
	// 在界面上长得一模一样。正常跑完时 Pending 已经是 0，这行不改变任何东西。
	job.DeferredAccounts += job.Pending
	job.Pending = 0
	job.NextRetryAt = nil
	if failure != "" {
		job.Error = failure
	}
	if q.running == job {
		q.running = nil
	}
}

// snapshot 复制一份给 HTTP 响应用。
//
// **必须复制**：Items 是切片，直接把 job 交给 json.Encoder 时，worker 正在
// append 的那一刻会与序列化撞上 —— 那是数据竞争，而 `-race` 下它会红在一个
// 与本功能毫无关系的测试里。
func (q *keyImportQueue) snapshot(id int64) (keyImportJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobs[id]
	if !ok {
		return keyImportJob{}, false
	}
	return cloneKeyImportJob(job), true
}

// list 按**新的在前**返回全部批次快照。
func (q *keyImportQueue) list() []keyImportJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]keyImportJob, 0, len(q.order))
	for i := len(q.order) - 1; i >= 0; i-- {
		if job, ok := q.jobs[q.order[i]]; ok {
			out = append(out, cloneKeyImportJob(job))
		}
	}
	return out
}

func cloneKeyImportJob(job *keyImportJob) keyImportJob {
	out := *job
	out.Items = append([]keyImportItem{}, job.Items...)
	return out
}

// record 把一个账号的结果并进批次。所有对 job 的写入都经过这里，
// 于是"谁在改 job"只有一个答案。
func (q *keyImportQueue) record(job *keyImportJob, item keyImportItem, got collector.KeyImportResult) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job.Found += got.Found
	job.Imported += got.Imported
	job.Skipped += got.Skipped
	job.Failed += got.Failed
	job.Deferred += got.Deferred
	// 账号级计数就地累加，不让调用方（更不让界面）从 items 里自己数 ——
	// 数一遍就会有人数错，而数错的方向永远是"看起来比实际好"。
	switch item.Status {
	case "failed":
		job.FailedAccounts++
	case "deferred":
		job.DeferredAccounts++
	case "skipped":
		job.SkippedAccounts++
	}
	job.Items = append(job.Items, item)
	job.Count = len(job.Items)
	job.Done++
}

// progress 更新"还剩多少、下一个什么时候轮到"。
func (q *keyImportQueue) progress(job *keyImportJob, pending int, nextAt time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job.Pending = pending
	if nextAt.IsZero() {
		job.NextRetryAt = nil
		return
	}
	at := nextAt
	job.NextRetryAt = &at
}

// keyImportRetryDelay 判断这个失败值不值得再来一次，以及等多久。
//
// 分类而不是一刀切（2026-09-16 与需求方确认的取舍）：
//   - 429 / 5xx / 网络超时 = **暂时性**。它们是"现在不行"，退避之后多半能成，
//     而限流恰恰是这个功能最常见的失败 —— 不自动退避等于把重试的活全推给人。
//   - 401 / 403 / 站型不支持 = **确定性**。凭证失效、站点不认这套接口，
//     重排多少次都是同一个错，重试只是把同一个 401 再打一遍别人家站点。
//
// 429 优先用上游给的 Retry-After：它比我们的退避序列准，也是人家明说的节奏。
func keyImportRetryDelay(err error, attempts int) (time.Duration, bool) {
	// 翻倍到封顶为止。**不要写成 `base << (attempts-1)`**：attempts 只要大一点
	// 那个移位就溢出成负数，于是"退避封顶"变成"立刻重试"——
	// 而它看起来完全正常（一个 Duration 而已），只是符号反了。
	backoff := keyImportRetryBase
	for i := 1; i < attempts && backoff < keyImportRetryCap; i++ {
		backoff *= 2
	}
	if backoff > keyImportRetryCap {
		backoff = keyImportRetryCap
	}
	if status, retryAfter, ok := collector.HTTPFailure(err); ok {
		switch {
		case status == 429:
			if retryAfter > 0 {
				return retryAfter, true
			}
			return backoff, true
		case status >= 500:
			return backoff, true
		default:
			// 4xx（401/403/404…）是确定性的：换个时间再打还是同一个答案。
			return 0, false
		}
	}
	// 不是 HTTP 错误：只有超时类值得重来。ctx 取消不算 —— 那是我们自己要停。
	if errors.Is(err, context.Canceled) {
		return 0, false
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return backoff, true
	}
	return 0, false
}

// runKeyImportJob 是队列的主循环：逐个账号跑，失败的按分类重排。
//
// 三条与从前（同步端点那个 for 循环）不同的性质，每条都是一个已确认的缺陷：
//
//  1. **一个账号失败不再中断整批**。旧代码在 `err != nil || Failed>0 || Deferred>0`
//     时 `break` —— 2026-09-16 实测：选 10 个渠道，第 2 个站鉴权失败，后面 8 个
//     站一个字节都没发出去，而响应里 `deferred_accounts` 还是 0。这些站点彼此
//     毫无关系，一个挂了不是不跑另一个的理由。
//  2. **明文读取预算按渠道算，不是整批一个**。上游那个 20 次的窗口是**按站点**
//     的，把它做成全批共享意味着 A 站花完了 B 站就没得花 —— 这正是"单次只能
//     处理 20 个账号"这个限制的由来，而它是我们自己加的，不是上游要求的。
//  3. **每个账号最终都有一条结果**。跑不到的账号也要在 Items 里留个 pending/failed，
//     否则"这批到底动了谁"只能靠人去数。
func (s *Server) runKeyImportJob(ctx context.Context, job *keyImportJob, targets []store.Account) {
	queue := make([]keyImportTask, 0, len(targets))
	for _, account := range targets {
		queue = append(queue, keyImportTask{account: account})
	}
	// 每个渠道一份预算。map 而不是一个数：见上面第 2 条。
	budget := map[int64]int{}

	for len(queue) > 0 {
		task := queue[0]
		queue = queue[1:]
		s.queue.progress(job, len(queue)+1, task.notBefore)
		if !task.notBefore.IsZero() {
			if err := sleepUntil(ctx, task.notBefore); err != nil {
				s.queue.finish(job, "canceled", "进程退出，批次未跑完")
				return
			}
		}
		s.queue.progress(job, len(queue), time.Time{})

		item, got, err := s.runKeyImportTask(ctx, task, budget)
		if ctx.Err() != nil {
			// 手上这个账号的结果要丢掉（它是被打断的，不是一个结论），
			// 所以把它算回未处理数 —— 上面那次 progress 已经把它从 Pending 里
			// 减掉了，不补这一下，"没跑完的账号数"就会少一个。
			s.queue.progress(job, len(queue)+1, time.Time{})
			s.queue.finish(job, "canceled", "进程退出，批次未跑完")
			return
		}
		// 值得重来就排回队尾，让别的站先跑 —— 站点之间互不相干，
		// 没有理由让整条队列陪一个正在退避的站点等着。
		if err != nil {
			if delay, retry := keyImportRetryDelay(err, task.attempts+1); retry &&
				task.attempts+1 < keyImportMaxAttempts {
				task.attempts++
				task.notBefore = time.Now().Add(delay)
				queue = append(queue, task)
				s.queue.progress(job, len(queue), queue[0].notBefore)
				continue
			}
		}
		s.queue.record(job, item, got)
	}
	s.queue.finish(job, "done", "")
}

// runKeyImportTask 跑一个账号，返回它这一条结果。
func (s *Server) runKeyImportTask(
	ctx context.Context, task keyImportTask, budget map[int64]int,
) (keyImportItem, collector.KeyImportResult, error) {
	account := task.account
	item := keyImportItem{
		ChannelID: account.ChannelID, AccountID: account.ID, Attempts: task.attempts + 1,
	}
	if account.Status != "active" {
		item.Status, item.Error = "skipped", "账号已停用"
		return item, collector.KeyImportResult{}, nil
	}
	remaining, seen := budget[account.ChannelID]
	if !seen {
		remaining = collector.DefaultKeySecretResolveLimit
	}
	if remaining <= 0 {
		// 同一个渠道下的第二个账号把本渠道预算用光了。这不是失败，是"今天到此为止"，
		// 所以记 deferred 而不是 failed —— 两者要人做的事完全不同。
		item.Status, item.Error = "deferred", "该渠道本次明文读取预算已用尽，稍后再同步"
		return item, collector.KeyImportResult{}, nil
	}

	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		item.Status, item.Error = "failed", keyImportFailureText(err)
		return item, collector.KeyImportResult{}, err
	}
	defer release()
	got, err := s.ImportKeys(ctx, conn, account.ChannelID, account.ID,
		collector.KeyImportRequest{MaxSecretResolves: remaining})
	budget[account.ChannelID] = remaining - got.SecretResolves
	switch {
	case err != nil:
		item.Status, item.Error = "failed", keyImportFailureText(err)
	case got.Failed > 0:
		item.Status = "partial"
	case got.Deferred > 0:
		item.Status = "deferred"
	default:
		item.Status = "ok"
	}
	item.KeyImportResult = got
	return item, got, err
}

// keyImportFailureText 保证失败**一定带得出原因**。
//
// `HTTPError.Error()` 只回 Message，而不是每条构造路径都填它 —— 空串一路落到
// 界面上就是一行光秃秃的 "failed"，人看不出该去修凭证还是等限流过去。
// 有状态码就报状态码，那至少把 401 与 429 分开了。
func keyImportFailureText(err error) string {
	if text := shortErr(err); text != "" {
		return text
	}
	if status, _, ok := collector.HTTPFailure(err); ok {
		return fmt.Sprintf("上游返回 HTTP %d（未带说明）", status)
	}
	return "未知错误（上游未给出说明）"
}

// sleepUntil 等到 at，或 ctx 结束时提前返回错误。
func sleepUntil(ctx context.Context, at time.Time) error {
	delay := time.Until(at)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// backgroundCtx 是后台任务的生命周期（Key 同步批次、all-api-hub 同步共用）。
//
// **不能用请求的 ctx**：它在响应写完那一刻就被取消，任务会在第一次
// `DB.Acquire` 上直接死掉 —— 而且死得静悄悄。用 main 注入的那个信号 ctx，
// 于是 Ctrl-C / SIGTERM 能把在跑的任务停下，而浏览器切走一个页面不能。
func (s *Server) backgroundCtx() context.Context {
	if s.Background != nil {
		return s.Background
	}
	return context.Background()
}

// keyImportAcquireLock 取跨实例的 Key 自动化锁，并返回释放函数。
//
// 与同步端点用的是同一把锁（store.TryKeyAutomationLock），但持有方式不同：
// 那边在一次请求内取了就放，这边要**在整个批次期间**一直握着 —— 否则另一个
// 实例会在同一批站点上并行跑，两边共花同一份限流预算。
// 锁绑在取它的那条连接上，所以连接也得一起攥到批次结束。
func (s *Server) keyImportAcquireLock(ctx context.Context) (func(), bool, error) {
	conn, release, err := s.DB.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	locked, err := store.TryKeyAutomationLock(ctx, conn)
	if err != nil {
		release()
		return nil, false, err
	}
	if !locked {
		release()
		return nil, false, nil
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.UnlockKeyAutomation(unlockCtx, conn); err != nil {
			s.Logger.Error("释放 Key 自动化锁失败", "err", err)
		}
		release()
	}, true, nil
}
