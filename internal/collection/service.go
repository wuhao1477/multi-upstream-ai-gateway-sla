package collection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// Intervals are the four independently configurable collection periods.
type Intervals struct {
	Balance  time.Duration
	KeyQuota time.Duration
	Price    time.Duration
	Catalog  time.Duration
}

// IntervalsFromSnapshot reads and validates FR-116 collection periods.
func IntervalsFromSnapshot(snap *config.Snapshot) (Intervals, error) {
	balance, err := positiveInt(snap, "collector_balance_interval_min")
	if err != nil {
		return Intervals{}, err
	}
	keys, err := positiveInt(snap, "collector_keyquota_interval_min")
	if err != nil {
		return Intervals{}, err
	}
	price, err := positiveInt(snap, "collector_price_interval_h")
	if err != nil {
		return Intervals{}, err
	}
	catalog, err := positiveInt(snap, "collector_catalog_interval_h")
	if err != nil {
		return Intervals{}, err
	}
	return Intervals{
		Balance:  time.Duration(balance) * time.Minute,
		KeyQuota: time.Duration(keys) * time.Minute,
		Price:    time.Duration(price) * time.Hour,
		Catalog:  time.Duration(catalog) * time.Hour,
	}, nil
}

func positiveInt(snap *config.Snapshot, key string) (int, error) {
	n, err := snap.Int(key)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("config: 键 %q 必须大于 0", key)
	}
	return n, nil
}

var capabilityOrder = []collector.Capability{
	collector.CapAccount,
	collector.CapGroups,
	collector.CapKeys,
	collector.CapPricing,
	collector.CapModelCatalog,
}

const (
	maxConcurrentChannels     = 4
	defaultChannelSyncTimeout = 2 * time.Minute
	retryBackoffBase          = 30 * time.Second
	retryBackoffMax           = 30 * time.Minute
)

// 编译期守住连接预算：每个 worker 峰值占两条连接（整轮持有的渠道 advisory lock
// + 临时的凭证/限速/写入连接）。调大 maxConcurrentChannels 而不同步抬高
// store.MinPoolConns 会让整轮采集阻塞到 120s 超时 —— 让它在这里编译不过，
// 而不是在生产上表现为"采集莫名全红"。
const _ = uint(store.MinPoolConns - 2*maxConcurrentChannels)

type scheduleKey struct {
	channelID  int64
	capability collector.Capability
}

type scheduleState struct {
	nextAt       time.Time
	failureCount uint8
}

// Schedule tracks the next run for each channel capability in memory.
type Schedule struct {
	interval map[collector.Capability]time.Duration
	states   map[scheduleKey]scheduleState
}

// NewSchedule creates a schedule whose first pass is due immediately.
func NewSchedule(period Intervals) *Schedule {
	return &Schedule{
		interval: map[collector.Capability]time.Duration{
			collector.CapAccount:      period.Balance,
			collector.CapGroups:       period.Catalog,
			collector.CapKeys:         period.KeyQuota,
			collector.CapPricing:      period.Price,
			collector.CapModelCatalog: period.Catalog,
		},
		states: map[scheduleKey]scheduleState{},
	}
}

// Due returns capabilities whose interval has elapsed in collector dependency order.
func (s *Schedule) Due(channelID int64, now time.Time) []collector.Capability {
	var due []collector.Capability
	for _, capability := range capabilityOrder {
		key := scheduleKey{channelID: channelID, capability: capability}
		state, ok := s.states[key]
		if !ok {
			s.states[key] = scheduleState{}
		}
		if state.nextAt.IsZero() || !now.Before(state.nextAt) {
			due = append(due, capability)
		}
	}
	return due
}

// MarkSuccess advances successful capabilities and clears their failure count.
func (s *Schedule) MarkSuccess(
	channelID int64, now time.Time, capabilities []collector.Capability,
) {
	for _, capability := range capabilities {
		key := scheduleKey{channelID: channelID, capability: capability}
		s.states[key] = scheduleState{nextAt: now.Add(s.interval[capability])}
	}
}

// MarkFailure retries failed capabilities independently with exponential backoff and jitter.
func (s *Schedule) MarkFailure(
	channelID int64, now time.Time, failure capabilityFailure,
) {
	key := scheduleKey{channelID: channelID, capability: failure.capability}
	state := s.states[key]
	if state.failureCount < 63 {
		state.failureCount++
	}
	delay := retryDelay(s.interval[failure.capability], state.failureCount)
	delay = max(delay, failure.minimumDelay)
	state.nextAt = now.Add(delay)
	s.states[key] = state
}

// MarkSkipped retries lock contention soon without counting it as an upstream failure.
func (s *Schedule) MarkSkipped(
	channelID int64, now time.Time, capabilities []collector.Capability,
) {
	for _, capability := range capabilities {
		key := scheduleKey{channelID: channelID, capability: capability}
		state := s.states[key]
		state.nextAt = now.Add(retryDelay(s.interval[capability], 1))
		s.states[key] = state
	}
}

func retryDelay(interval time.Duration, failures uint8) time.Duration {
	limit := min(interval, retryBackoffMax)
	delay := retryBackoffBase
	for attempt := uint8(1); attempt < failures && delay < limit; attempt++ {
		delay = min(delay*2, limit)
	}
	delay = min(delay, limit)
	spread := delay / 5
	if spread > 0 {
		delay += time.Duration(rand.Int64N(int64(2*spread)+1)) - spread
	}
	return delay
}

// Prune forgets channels that are absent or currently disabled. Re-enabled channels run immediately.
func (s *Schedule) Prune(active map[int64]bool) {
	for key := range s.states {
		if !active[key.channelID] {
			delete(s.states, key)
		}
	}
}

// NextDelay returns the time until any capability is next due.
func (s *Schedule) NextDelay(now time.Time) time.Duration {
	var shortest time.Duration
	for _, state := range s.states {
		d := state.nextAt.Sub(now)
		if d <= 0 {
			return 0
		}
		if shortest == 0 || d < shortest {
			shortest = d
		}
	}
	return shortest
}

// Service runs periodic collection over all registered channels.
type Service struct {
	Schedule     *Schedule
	Now          func() time.Time
	ListChannels func(context.Context) ([]store.Channel, error)
	Sync         func(context.Context, store.Channel, []collector.Capability) (*collector.SyncResult, error)
	TryLock      func(context.Context, int64) (release func(), acquired bool, err error)
	Logger       *slog.Logger
}

type channelTask struct {
	channel      store.Channel
	capabilities []collector.Capability
}

type channelOutcome struct {
	channelID   int64
	completed   []collector.Capability
	failed      []capabilityFailure
	skipped     []collector.Capability
	completedAt time.Time
	err         error
}

type capabilityFailure struct {
	capability   collector.Capability
	minimumDelay time.Duration
}

// NewService wires a Service to PostgreSQL and the shared Runner.
func NewService(pool *store.Pool, runner *Runner, schedule *Schedule, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		Schedule: schedule,
		Now:      time.Now,
		Logger:   logger,
		ListChannels: func(ctx context.Context) ([]store.Channel, error) {
			conn, release, err := pool.Acquire(ctx)
			if err != nil {
				return nil, err
			}
			defer release()
			return store.ListChannels(ctx, conn)
		},
		Sync: runner.Sync,
		TryLock: func(ctx context.Context, channelID int64) (func(), bool, error) {
			conn, releaseConn, err := pool.Acquire(ctx)
			if err != nil {
				return nil, false, err
			}
			locked, err := store.TryChannelSyncLock(ctx, conn, channelID)
			if err != nil || !locked {
				releaseConn()
				return nil, locked, err
			}
			release := func() {
				unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := store.UnlockChannelSync(unlockCtx, conn, channelID); err != nil {
					logger.Error("释放渠道采集锁失败", "channel_id", channelID, "err", err)
				}
				releaseConn()
			}
			return release, true, nil
		},
	}
}

// RunOnce runs every capability currently due across eligible channels.
func (s *Service) RunOnce(ctx context.Context) error {
	now := s.Now()
	channels, err := s.ListChannels(ctx)
	if err != nil {
		return err
	}
	active := make(map[int64]bool, len(channels))
	var tasks []channelTask
	for _, channel := range channels {
		if collectionDisabled(channel, now) {
			continue
		}
		active[channel.ID] = true
		due := s.Schedule.Due(channel.ID, now)
		if len(due) == 0 {
			continue
		}
		tasks = append(tasks, channelTask{channel: channel, capabilities: due})
	}
	s.Schedule.Prune(active)

	var failures []error
	for _, outcome := range s.runTasks(ctx, tasks) {
		s.Schedule.MarkSuccess(outcome.channelID, outcome.completedAt, outcome.completed)
		for _, failure := range outcome.failed {
			s.Schedule.MarkFailure(outcome.channelID, outcome.completedAt, failure)
		}
		s.Schedule.MarkSkipped(outcome.channelID, outcome.completedAt, outcome.skipped)
		if outcome.err != nil {
			failures = append(failures, outcome.err)
		}
	}
	if err := ctx.Err(); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (s *Service) runTasks(ctx context.Context, tasks []channelTask) []channelOutcome {
	if len(tasks) == 0 {
		return nil
	}
	jobs := make(chan channelTask)
	outcomes := make(chan channelOutcome, len(tasks))
	workers := min(len(tasks), maxConcurrentChannels)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				outcomes <- s.collectChannel(ctx, task)
			}
		}()
	}

submit:
	for _, task := range tasks {
		select {
		case jobs <- task:
		case <-ctx.Done():
			break submit
		}
	}
	close(jobs)
	wg.Wait()
	close(outcomes)

	result := make([]channelOutcome, 0, len(outcomes))
	for outcome := range outcomes {
		result = append(result, outcome)
	}
	return result
}

func (s *Service) collectChannel(ctx context.Context, task channelTask) channelOutcome {
	outcome := channelOutcome{channelID: task.channel.ID, completedAt: s.Now()}
	release, acquired, err := s.TryLock(ctx, task.channel.ID)
	if err != nil {
		outcome.failed = failuresFor(task.capabilities, 0, 0)
		outcome.err = fmt.Errorf("渠道 %d 加锁: %w", task.channel.ID, err)
		return outcome
	}
	if !acquired {
		outcome.skipped = task.capabilities
		return outcome
	}
	defer release()

	syncCtx, cancel := context.WithTimeout(ctx, defaultChannelSyncTimeout)
	defer cancel()
	result, err := s.Sync(syncCtx, task.channel, task.capabilities)
	outcome.completedAt = s.Now()
	if err != nil {
		status, retryAfter, _ := collector.HTTPFailure(err)
		outcome.failed = failuresFor(task.capabilities, status, retryAfter)
		outcome.err = fmt.Errorf("渠道 %d: %w", task.channel.ID, err)
		return outcome
	}
	if result == nil {
		outcome.failed = failuresFor(task.capabilities, 0, 0)
		return outcome
	}
	outcome.completed, outcome.failed, outcome.err = completedCapabilities(task, result)
	return outcome
}

func completedCapabilities(
	task channelTask, result *collector.SyncResult,
) ([]collector.Capability, []capabilityFailure, error) {
	failed := make(map[collector.Capability]capabilityFailure, len(task.capabilities))
	seen := make(map[collector.Capability]bool, len(result.Items))
	var failures []error
	for _, item := range result.Items {
		seen[item.Capability] = true
		if item.Status == collector.StatusFailed || item.Status == collector.StatusPartial {
			failed[item.Capability] = capabilityFailure{
				capability: item.Capability,
				minimumDelay: retryMinimum(
					item.HTTPStatus, time.Duration(item.RetryAfterMs)*time.Millisecond),
			}
			failures = append(failures, fmt.Errorf(
				"渠道 %d 的 %s 采集为 %s: %s",
				task.channel.ID, item.Capability, item.Status, item.Error))
		}
	}
	var completed []collector.Capability
	var retry []capabilityFailure
	for _, capability := range task.capabilities {
		failure, didFail := failed[capability]
		if seen[capability] && !didFail {
			completed = append(completed, capability)
		} else {
			if !didFail {
				failure.capability = capability
				failures = append(failures, fmt.Errorf(
					"渠道 %d 未返回 %s 的采集结果", task.channel.ID, capability))
			}
			retry = append(retry, failure)
		}
	}
	return completed, retry, errors.Join(failures...)
}

func failuresFor(
	capabilities []collector.Capability, status int, retryAfter time.Duration,
) []capabilityFailure {
	failures := make([]capabilityFailure, 0, len(capabilities))
	for _, capability := range capabilities {
		failures = append(failures, capabilityFailure{
			capability: capability, minimumDelay: retryMinimum(status, retryAfter),
		})
	}
	return failures
}

func retryMinimum(status int, retryAfter time.Duration) time.Duration {
	switch status {
	case 401, 403:
		return max(retryAfter, 5*time.Minute)
	case 429:
		if retryAfter > 0 {
			return retryAfter
		}
		return 5 * time.Minute
	}
	return retryAfter
}

func collectionDisabled(channel store.Channel, now time.Time) bool {
	if channel.Status != "disabled" {
		return false
	}
	return channel.DisabledUntil == nil || channel.DisabledUntil.After(now)
}

// Run keeps collecting until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	for {
		if err := s.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.Logger.Error("周期采集部分失败", "err", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		delay := s.Schedule.NextDelay(s.Now())
		if delay <= 0 {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
