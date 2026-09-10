package collection

import (
	"context"
	"errors"
	"reflect"
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
	if got := s.Due(now); !reflect.DeepEqual(got, all) {
		t.Fatalf("首轮能力 = %v，期望 %v", got, all)
	}
	s.Mark(now, all)

	wantSixHours := []collector.Capability{
		collector.CapAccount, collector.CapKeys, collector.CapPricing,
	}
	if got := s.Due(now.Add(6 * time.Hour)); !reflect.DeepEqual(got, wantSixHours) {
		t.Fatalf("6 小时后到期能力 = %v，期望 %v", got, wantSixHours)
	}
	if got := s.Due(now.Add(12 * time.Hour)); !reflect.DeepEqual(got, all) {
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
			synced = append(synced, ch.ID)
			if len(caps) != 5 {
				t.Fatalf("首轮应采 5 项能力，得到 %v", caps)
			}
			return &collector.SyncResult{ChannelID: ch.ID}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}
	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
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

func TestRunOnceKeepsFailedCapabilitiesDueForRetry(t *testing.T) {
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
	if got := schedule.Due(now); len(got) != len(capabilityOrder) {
		t.Fatalf("失败能力必须继续保持到期以便重试，得到 %v", got)
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
