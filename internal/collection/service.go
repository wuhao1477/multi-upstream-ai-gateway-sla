package collection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Schedule tracks the next run for each capability in memory.
type Schedule struct {
	interval map[collector.Capability]time.Duration
	next     map[collector.Capability]time.Time
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
		next: map[collector.Capability]time.Time{},
	}
}

// Due returns capabilities whose interval has elapsed in collector dependency order.
func (s *Schedule) Due(now time.Time) []collector.Capability {
	var due []collector.Capability
	for _, capability := range capabilityOrder {
		if next := s.next[capability]; next.IsZero() || !now.Before(next) {
			due = append(due, capability)
		}
	}
	return due
}

// Mark advances only the capabilities attempted in this pass.
func (s *Schedule) Mark(now time.Time, capabilities []collector.Capability) {
	for _, capability := range capabilities {
		s.next[capability] = now.Add(s.interval[capability])
	}
}

// NextDelay returns the time until any capability is next due.
func (s *Schedule) NextDelay(now time.Time) time.Duration {
	var shortest time.Duration
	for _, capability := range capabilityOrder {
		d := s.next[capability].Sub(now)
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
	due := s.Schedule.Due(now)
	if len(due) == 0 {
		return nil
	}
	channels, err := s.ListChannels(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, channel := range channels {
		if collectionDisabled(channel, now) {
			continue
		}
		release, acquired, err := s.TryLock(ctx, channel.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("渠道 %d 加锁: %w", channel.ID, err))
			continue
		}
		if !acquired {
			continue
		}
		var result *collector.SyncResult
		func() {
			defer release()
			syncCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			result, err = s.Sync(syncCtx, channel, due)
		}()
		if err != nil {
			failures = append(failures, fmt.Errorf("渠道 %d: %w", channel.ID, err))
			continue
		}
		if result == nil {
			continue
		}
		for _, item := range result.Items {
			if item.Status == collector.StatusFailed || item.Status == collector.StatusPartial {
				failures = append(failures, fmt.Errorf(
					"渠道 %d 的 %s 采集为 %s: %s",
					channel.ID, item.Capability, item.Status, item.Error))
			}
		}
	}
	s.Schedule.Mark(now, due)
	return errors.Join(failures...)
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
