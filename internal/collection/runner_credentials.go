package collection

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// NewRunner builds a production Runner backed by PostgreSQL.
func NewRunner(pool *store.Pool, client *collector.Client) *Runner {
	credentials := store.NewCredentialStore(pool)
	return &Runner{
		Client: client,
		Sink:   store.NewCollectorSink(pool),
		Auth:   collector.NewAuthenticator(credentials),
		Logger: slog.Default(),
		LoadCredentials: func(ctx context.Context, ch store.Channel) ([]collector.Credential, error) {
			conn, release, err := pool.Acquire(ctx)
			if err != nil {
				return nil, err
			}
			defer release()
			creds, err := credentials.ListByChannel(ctx, conn, ch)
			if err != nil {
				return nil, err
			}
			quotaPerUnit := store.QuotaPerUnit(ctx, conn, ch.ID)
			// 手工登记或早期导入可能没有探测快照；采集前补取并保存，不猜换算基数。
			if quotaPerUnit <= 0 && ch.SiteFamily == string(collector.FamilyNewAPI) {
				detected, err := collector.Detect(ctx, client, ch.BaseURL)
				if err != nil {
					return nil, fmt.Errorf("渠道 %d 获取 quota_per_unit 失败: %w", ch.ID, err)
				}
				if detected.QuotaPerUnit <= 0 {
					return nil, fmt.Errorf("渠道 %d 的 /api/status 未返回有效 quota_per_unit，无法换算额度", ch.ID)
				}
				if err := credentials.SaveDetected(ctx, conn, ch.ID, detected); err != nil {
					return nil, fmt.Errorf("渠道 %d 保存额度换算参数失败: %w", ch.ID, err)
				}
				quotaPerUnit = detected.QuotaPerUnit
			}
			for i := range creds {
				creds[i].QuotaPerUnit = quotaPerUnit
			}
			return creds, nil
		},
	}
}
