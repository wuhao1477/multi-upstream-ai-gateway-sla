package collection

import (
	"context"
	"errors"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func TestRunnerUnknownFamilyFailsBeforeCredentialLoad(t *testing.T) {
	loaded := false
	r := &Runner{
		Client: collector.NewClient(0),
		LoadCredential: func(context.Context, store.Channel) (collector.Credential, error) {
			loaded = true
			return collector.Credential{}, nil
		},
	}
	_, err := r.Sync(context.Background(), store.Channel{
		ID: 1, SiteFamily: string(collector.FamilyUnknown),
	}, nil)
	if !errors.Is(err, collector.ErrPrecondition) {
		t.Fatalf("未知站型应返回 ErrPrecondition，得到 %v", err)
	}
	if loaded {
		t.Fatal("未知站型不应读取凭证")
	}
}
