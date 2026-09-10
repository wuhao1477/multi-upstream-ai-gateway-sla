package collection

import (
	"context"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func TestRetryDelayCapsAtThirtyMinutes(t *testing.T) {
	got := retryDelay(6*time.Hour, 20)
	if got < 24*time.Minute || got > 36*time.Minute {
		t.Fatalf("长周期能力退避 = %s，期望在 30 分钟 ±20%% 内", got)
	}
}

func TestRetryDelayAddsJitter(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 20 {
		delay := retryDelay(10*time.Minute, 1)
		if delay < 24*time.Second || delay > 36*time.Second {
			t.Fatalf("首次退避 = %s，期望在 30 秒 ±20%% 内", delay)
		}
		seen[delay] = true
	}
	if len(seen) == 1 {
		t.Fatal("重复计算得到完全相同的退避时间，未加入 jitter")
	}
}

func TestRunOnceReportsMissingCapabilityItems(t *testing.T) {
	svc := &Service{
		Schedule: NewSchedule(Intervals{
			Balance: time.Minute, KeyQuota: time.Minute,
			Price: time.Hour, Catalog: time.Hour,
		}),
		Now: time.Now,
		ListChannels: func(context.Context) ([]store.Channel, error) {
			return []store.Channel{{ID: 8, Status: "enabled"}}, nil
		},
		Sync: func(context.Context, store.Channel, []collector.Capability) (*collector.SyncResult, error) {
			return &collector.SyncResult{Items: []collector.SyncItem{{
				Capability: collector.CapAccount,
				Status:     collector.StatusOK,
			}}}, nil
		},
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
	}
	if err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("缺少到期 capability 的结果时必须报告错误")
	}
}

func TestRunOnceUsesHTTPFailureRetryHints(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		retryAfterMs int64
		before       time.Duration
		after        time.Duration
	}{
		{name: "unauthorized", status: 401, before: 4 * time.Minute, after: 5*time.Minute + time.Second},
		{name: "forbidden", status: 403, before: 4 * time.Minute, after: 5*time.Minute + time.Second},
		{name: "retry-after", status: 429, retryAfterMs: 120_000,
			before: 119 * time.Second, after: 121 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
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
						if calls == 1 && capability == collector.CapAccount {
							item.Status = collector.StatusFailed
							item.Error = "upstream rejected request"
							item.HTTPStatus = tc.status
							item.RetryAfterMs = tc.retryAfterMs
						}
						items = append(items, item)
					}
					return &collector.SyncResult{ChannelID: ch.ID, Items: items}, nil
				},
				TryLock: func(context.Context, int64) (func(), bool, error) {
					return func() {}, true, nil
				},
			}

			_ = svc.RunOnce(context.Background())
			now = base.Add(tc.before)
			_ = svc.RunOnce(context.Background())
			if calls != 1 {
				t.Fatalf("%s 后调用次数 = %d，期望仍为 1", tc.before, calls)
			}
			now = base.Add(tc.after)
			if err := svc.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("%s 后调用次数 = %d，期望重试一次", tc.after, calls)
			}
		})
	}
}
