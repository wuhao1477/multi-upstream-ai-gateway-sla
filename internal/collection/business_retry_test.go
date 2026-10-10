package collection

import (
	"context"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func TestBusinessAuthenticationFailureUsesAuthBackoff(t *testing.T) {
	result := &collector.SyncResult{Items: []collector.SyncItem{{
		Capability: collector.CapAccount, Status: collector.StatusFailed,
		HTTPStatus: 200, BusinessCode: 401, AuthenticationFailed: true,
	}}}
	_, failed, _ := completedCapabilities(channelTask{capabilities: []collector.Capability{collector.CapAccount}}, result)
	if len(failed) != 1 || failed[0].minimumDelay != 5*time.Minute {
		t.Fatal("HTTP 200 business authentication failure skipped authentication backoff")
	}
}

func TestBusinessAuthenticationErrorUsesAuthBackoff(t *testing.T) {
	svc := &Service{
		Now: time.Now,
		TryLock: func(context.Context, int64) (func(), bool, error) {
			return func() {}, true, nil
		},
		Sync: func(context.Context, store.Channel, []collector.Capability) (*collector.SyncResult, error) {
			return nil, &collector.HTTPError{StatusCode: 200, Cause: collector.ErrUnauthorized}
		},
	}
	outcome := svc.collectChannel(context.Background(), channelTask{capabilities: []collector.Capability{collector.CapAccount}})
	if len(outcome.failed) != 1 || outcome.failed[0].minimumDelay != 5*time.Minute {
		t.Fatal("authentication error before capability collection skipped authentication backoff")
	}
}
