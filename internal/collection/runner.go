// Package collection assembles channel adapters and runs manual or periodic collection.
package collection

import (
	"context"
	"fmt"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// Runner owns the dependencies shared by manual and periodic channel syncs.
type Runner struct {
	Client         *collector.Client
	Sink           collector.Sink
	Auth           *collector.Authenticator
	LoadCredential func(context.Context, store.Channel) (collector.Credential, error)
}

// NewRunner builds a production Runner backed by PostgreSQL.
func NewRunner(pool *store.Pool, client *collector.Client) *Runner {
	credentials := store.NewCredentialStore(pool)
	return &Runner{
		Client: client,
		Sink:   store.NewCollectorSink(pool),
		Auth:   collector.NewAuthenticator(credentials),
		LoadCredential: func(ctx context.Context, ch store.Channel) (collector.Credential, error) {
			conn, release, err := pool.Acquire(ctx)
			if err != nil {
				return collector.Credential{}, err
			}
			defer release()
			cred, err := credentials.Load(ctx, conn, ch)
			if err != nil {
				return collector.Credential{}, err
			}
			cred.QuotaPerUnit = store.QuotaPerUnit(ctx, conn, ch.ID)
			return cred, nil
		},
	}
}

// Sync runs all capabilities when capabilities is empty, otherwise only the selected set.
func (r *Runner) Sync(
	ctx context.Context, ch store.Channel, capabilities []collector.Capability,
) (*collector.SyncResult, error) {
	reg, ok := collector.Lookup(collector.Family(ch.SiteFamily))
	if !ok {
		return nil, fmt.Errorf("%w：渠道站型 %q 无对应适配器", collector.ErrPrecondition, ch.SiteFamily)
	}
	if r.LoadCredential == nil {
		return nil, fmt.Errorf("%w：采集凭证加载器未配置", collector.ErrPrecondition)
	}
	cred, err := r.LoadCredential(ctx, ch)
	if err != nil {
		return nil, fmt.Errorf("%w：%w", collector.ErrPrecondition, err)
	}
	adapter := reg.New(r.Client)
	refresher, _ := adapter.(collector.Refresher)
	syncer := &collector.Syncer{
		Adapter: adapter, Sink: r.Sink, Auth: r.Auth, Refresher: refresher,
	}
	if len(capabilities) == 0 {
		return syncer.Sync(ctx, cred)
	}
	return syncer.SyncSelected(ctx, cred, capabilities...)
}
