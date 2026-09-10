package collection

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func TestIntervalsFromSnapshot(t *testing.T) {
	snap := config.NewSnapshot(map[string]string{
		"collector_balance_interval_min":  "7",
		"collector_keyquota_interval_min": "31",
		"collector_price_interval_h":      "8",
		"collector_catalog_interval_h":    "13",
	})
	got, err := IntervalsFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	want := Intervals{
		Balance: 7 * time.Minute, KeyQuota: 31 * time.Minute,
		Price: 8 * time.Hour, Catalog: 13 * time.Hour,
	}
	if got != want {
		t.Fatalf("周期 = %+v，期望 %+v", got, want)
	}
}

func TestScheduleUsesIndependentCapabilityIntervals(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	s := NewSchedule(Intervals{
		Balance: 5 * time.Minute, KeyQuota: 30 * time.Minute,
		Price: 6 * time.Hour, Catalog: 12 * time.Hour,
	})
	all := []collector.Capability{
		collector.CapAccount, collector.CapGroups, collector.CapKeys,
		collector.CapPricing, collector.CapModelCatalog,
	}
	if got := s.Due(1, now); !reflect.DeepEqual(got, all) {
		t.Fatalf("首轮能力 = %v，期望 %v", got, all)
	}
	s.MarkSuccess(1, now, all)

	wantSixHours := []collector.Capability{
		collector.CapAccount, collector.CapKeys, collector.CapPricing,
	}
	if got := s.Due(1, now.Add(6*time.Hour)); !reflect.DeepEqual(got, wantSixHours) {
		t.Fatalf("6 小时后到期能力 = %v，期望 %v", got, wantSixHours)
	}
	if got := s.Due(1, now.Add(12*time.Hour)); !reflect.DeepEqual(got, all) {
		t.Fatalf("12 小时后到期能力 = %v，期望 %v", got, all)
	}
}

func TestRunOnceSkipsActivelyDisabledChannels(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	channels := []store.Channel{
		{ID: 1, Status: "enabled"},
		{ID: 2, Status: "disabled"},
		{ID: 3, Status: "disabled", DisabledUntil: &future},
		{ID: 4, Status: "disabled", DisabledUntil: &past},
	}
	var syncedMu sync.Mutex
	var synced []int64
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: time.Minute, KeyQuota: time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: func() time.Time { return now },
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return channels, nil
		},
		Sync: func(_ context.Context, ch store.Channel, caps []collector.Capability) (*collector.SyncResult, error) {
			syncedMu.Lock()
			synced = append(synced, ch.ID)
			syncedMu.Unlock()
			if len(caps) != 5 {
				t.Fatalf("首轮应采 5 项能力，得到 %v", caps)
			}
			items := make([]collector.SyncItem, 0, len(caps))
			for _, capability := range caps {
				items = append(items, collector.SyncItem{
					Capability: capability,
					Status:     collector.StatusOK,
				})
			}
			return &collector.SyncResult{ChannelID: ch.ID, Items: items}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}
	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	syncedMu.Lock()
	defer syncedMu.Unlock()
	slices.Sort(synced)
	if want := []int64{1, 4}; !reflect.DeepEqual(synced, want) {
		t.Fatalf("实际采集渠道 = %v，期望 %v", synced, want)
	}
}

func TestRunOnceReleasesLockAfterFailure(t *testing.T) {
	released := false
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: time.Minute, KeyQuota: time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: func() time.Time { return time.Now() },
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{{ID: 1, Status: "enabled"}}, nil
		},
		Sync: func(context.Context, store.Channel, []collector.Capability) (*collector.SyncResult, error) {
			return nil, errors.New("upstream failed")
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() { released = true }, true, nil
		},
	}
	if err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("采集失败应返回 error")
	}
	if !released {
		t.Fatal("采集失败后必须释放渠道锁")
	}
}

func TestRunOnceSchedulesFailedCapabilitiesForRetry(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	schedule := NewSchedule(Intervals{
		Balance: time.Minute, KeyQuota: time.Minute,
		Price: time.Hour, Catalog: time.Hour,
	})
	svc := &Service{
		Schedule: schedule,
		Now:      func() time.Time { return now },
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{{ID: 1, Status: "enabled"}}, nil
		},
		Sync: func(context.Context, store.Channel, []collector.Capability) (*collector.SyncResult, error) {
			return nil, errors.New("上游暂时不可用")
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}
	if err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("采集失败应返回 error")
	}
	if got := schedule.Due(1, now); len(got) != 0 {
		t.Fatalf("失败能力进入退避后不应立即到期，得到 %v", got)
	}
	if got := schedule.Due(1, now.Add(40*time.Second)); len(got) != len(capabilityOrder) {
		t.Fatalf("首次退避结束后全部失败能力应重新到期，得到 %v", got)
	}
}

func TestRunOnceReportsFailedItems(t *testing.T) {
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: time.Minute, KeyQuota: time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: func() time.Time { return time.Now() },
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{{ID: 7, Status: "enabled"}}, nil
		},
		Sync: func(context.Context, store.Channel, []collector.Capability) (*collector.SyncResult, error) {
			return &collector.SyncResult{Items: []collector.SyncItem{{
				Capability: collector.CapModelCatalog,
				Status:     collector.StatusFailed,
			}}}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}
	if err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("逐项结果含 failed 时，周期采集必须报告失败")
	}
}

func TestRunOnceIsolatesFailedChannelSchedule(t *testing.T) {
	now := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	var callsMu sync.Mutex
	calls := map[int64]int{}
	var secondChannelOneCaps []collector.Capability
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: 5 * time.Minute, KeyQuota: 30 * time.Minute,
			Price: 6 * time.Hour, Catalog: 12 * time.Hour,
		}),
		Now: func() time.Time { return now },
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{
				{ID: 1, Status: "enabled"},
				{ID: 2, Status: "enabled"},
			}, nil
		},
		Sync: func(
			_ context.Context, ch store.Channel, caps []collector.Capability,
		) (*collector.SyncResult, error) {
			callsMu.Lock()
			calls[ch.ID]++
			call := calls[ch.ID]
			if ch.ID == 1 && call == 2 {
				secondChannelOneCaps = append([]collector.Capability(nil), caps...)
			}
			callsMu.Unlock()
			items := make([]collector.SyncItem, 0, len(caps))
			for _, capability := range caps {
				item := collector.SyncItem{Capability: capability, Status: collector.StatusOK}
				if ch.ID == 1 && capability == collector.CapAccount {
					item.Status = collector.StatusFailed
					item.Error = "余额端点暂时不可用"
				}
				items = append(items, item)
			}
			return &collector.SyncResult{ChannelID: ch.ID, Items: items}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}

	if err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("渠道 1 的余额采集失败时应报告错误")
	}
	now = now.Add(40 * time.Second)
	if err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("渠道 1 重试仍失败时应报告错误")
	}

	callsMu.Lock()
	defer callsMu.Unlock()
	if calls[1] != 2 {
		t.Fatalf("失败渠道调用次数 = %d，期望 2", calls[1])
	}
	if calls[2] != 1 {
		t.Fatalf("成功渠道调用次数 = %d，期望 1；失败渠道不应使其提前重采", calls[2])
	}
	if want := []collector.Capability{collector.CapAccount}; !reflect.DeepEqual(secondChannelOneCaps, want) {
		t.Fatalf("失败渠道第二轮能力 = %v，期望只重试 %v", secondChannelOneCaps, want)
	}
}

func TestRunOnceDoesNotLetSlowChannelBlockOtherChannels(t *testing.T) {
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	fastFinished := make(chan struct{})
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: time.Minute, KeyQuota: time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: time.Now,
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{
				{ID: 1, Status: "enabled"},
				{ID: 2, Status: "enabled"},
			}, nil
		},
		Sync: func(
			ctx context.Context, ch store.Channel, caps []collector.Capability,
		) (*collector.SyncResult, error) {
			if ch.ID == 1 {
				close(slowStarted)
				select {
				case <-releaseSlow:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			} else {
				close(fastFinished)
			}
			items := make([]collector.SyncItem, 0, len(caps))
			for _, capability := range caps {
				items = append(items, collector.SyncItem{
					Capability: capability,
					Status:     collector.StatusOK,
				})
			}
			return &collector.SyncResult{ChannelID: ch.ID, Items: items}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- svc.RunOnce(context.Background()) }()
	<-slowStarted

	fastWasBlocked := false
	select {
	case <-fastFinished:
	case <-time.After(100 * time.Millisecond):
		fastWasBlocked = true
	}
	close(releaseSlow)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fastWasBlocked {
		t.Fatal("慢渠道阻塞了后续渠道，渠道之间应并发采集")
	}
}

func TestRunOnceLimitsConcurrentChannels(t *testing.T) {
	channels := make([]store.Channel, 6)
	for i := range channels {
		channels[i] = store.Channel{ID: int64(i + 1), Status: "enabled"}
	}
	release := make(chan struct{})
	fifthStarted := make(chan struct{})
	var fifthOnce sync.Once
	var mu sync.Mutex
	running, maxRunning := 0, 0
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: time.Minute, KeyQuota: time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: func() time.Time { return time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC) },
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return channels, nil
		},
		Sync: func(
			ctx context.Context, ch store.Channel, caps []collector.Capability,
		) (*collector.SyncResult, error) {
			mu.Lock()
			running++
			if running > maxRunning {
				maxRunning = running
			}
			if running == 5 {
				fifthOnce.Do(func() { close(fifthStarted) })
			}
			mu.Unlock()

			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			mu.Lock()
			running--
			mu.Unlock()
			items := make([]collector.SyncItem, 0, len(caps))
			for _, capability := range caps {
				items = append(items, collector.SyncItem{
					Capability: capability,
					Status:     collector.StatusOK,
				})
			}
			return &collector.SyncResult{ChannelID: ch.ID, Items: items}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- svc.RunOnce(context.Background()) }()
	select {
	case <-fifthStarted:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maxRunning > 4 {
		t.Fatalf("渠道最大并发数 = %d，期望不超过 4", maxRunning)
	}
}

func TestRunOnceLimitsChannelSyncToTwoMinutes(t *testing.T) {
	var observed time.Duration
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: time.Minute, KeyQuota: time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: time.Now,
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{{ID: 1, Status: "enabled"}}, nil
		},
		Sync: func(
			ctx context.Context, ch store.Channel, caps []collector.Capability,
		) (*collector.SyncResult, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("周期采集必须设置整渠道超时")
			}
			observed = time.Until(deadline)
			items := make([]collector.SyncItem, 0, len(caps))
			for _, capability := range caps {
				items = append(items, collector.SyncItem{
					Capability: capability,
					Status:     collector.StatusOK,
				})
			}
			return &collector.SyncResult{ChannelID: ch.ID, Items: items}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observed < 119*time.Second || observed > 120*time.Second {
		t.Fatalf("整渠道超时 = %s，期望约 2 分钟", observed)
	}
}

func TestRunOnceBacksOffRepeatedChannelFailuresAndResetsAfterSuccess(t *testing.T) {
	base := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	now := base
	calls := 0
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: 10 * time.Minute, KeyQuota: 10 * time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: func() time.Time { return now },
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{{ID: 1, Status: "enabled"}}, nil
		},
		Sync: func(
			_ context.Context, ch store.Channel, caps []collector.Capability,
		) (*collector.SyncResult, error) {
			calls++
			items := make([]collector.SyncItem, 0, len(caps))
			for _, capability := range caps {
				item := collector.SyncItem{Capability: capability, Status: collector.StatusOK}
				if capability == collector.CapAccount && (calls == 1 || calls == 2 || calls == 4) {
					item.Status = collector.StatusFailed
					item.Error = "临时网络错误"
				}
				items = append(items, item)
			}
			return &collector.SyncResult{ChannelID: ch.ID, Items: items}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}

	run := func(at time.Duration, wantCalls int) {
		t.Helper()
		now = base.Add(at)
		_ = svc.RunOnce(context.Background())
		if calls != wantCalls {
			t.Fatalf("经过 %s 后调用次数 = %d，期望 %d", at, calls, wantCalls)
		}
	}

	run(0, 1)               // 第一次失败，进入约 30 秒退避。
	run(time.Second, 1)     // 退避期间不得立即重试。
	run(40*time.Second, 2)  // 第一次退避最多 36 秒。
	run(80*time.Second, 2)  // 第二次退避至少 48 秒。
	run(115*time.Second, 3) // 第二次退避最多 72 秒，本次成功并清零失败次数。
	run(716*time.Second, 4) // 正常 10 分钟周期后再次失败。
	run(756*time.Second, 5) // 清零后应重新从约 30 秒退避开始。
}
