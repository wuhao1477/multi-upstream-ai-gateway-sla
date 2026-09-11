// Package collection assembles channel adapters and runs manual or periodic collection.
package collection

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// Runner owns the dependencies shared by manual and periodic channel syncs.
type Runner struct {
	Client          *collector.Client
	Sink            collector.Sink
	Auth            *collector.Authenticator
	Logger          *slog.Logger
	LoadCredentials func(context.Context, store.Channel) ([]collector.Credential, error)
}

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
			for i := range creds {
				creds[i].QuotaPerUnit = quotaPerUnit
			}
			return creds, nil
		},
	}
}

// Sync runs all capabilities when capabilities is empty, otherwise only the selected set.
func (r *Runner) Sync(
	ctx context.Context, ch store.Channel, capabilities []collector.Capability,
) (*collector.SyncResult, error) {
	adapter, err := r.adapter(ch)
	if err != nil {
		return nil, err
	}
	if r.LoadCredentials == nil {
		return nil, fmt.Errorf("%w：采集凭证加载器未配置", collector.ErrPrecondition)
	}
	creds, err := r.LoadCredentials(ctx, ch)
	if err != nil {
		return nil, fmt.Errorf("%w：%w", collector.ErrPrecondition, err)
	}
	refresher, _ := adapter.(collector.Refresher)
	syncer := &collector.Syncer{
		Adapter: adapter, Sink: r.Sink, Auth: r.Auth, Refresher: refresher,
	}
	if len(capabilities) == 0 {
		return syncer.SyncMany(ctx, creds)
	}
	return syncer.SyncManySelected(ctx, creds, capabilities...)
}

func (r *Runner) adapter(ch store.Channel) (collector.Adapter, error) {
	reg, ok := collector.Lookup(collector.Family(ch.SiteFamily))
	if !ok {
		return nil, fmt.Errorf("%w：渠道站型 %q 无对应适配器", collector.ErrPrecondition, ch.SiteFamily)
	}
	return reg.New(r.Client), nil
}

// ImportKeys 通过已鉴权会话读取并登记一个渠道账号的上游 Key。
//
// 调用方已经提交渠道、账号和凭证事务；本方法只处理 Key，单把失败继续处理，
// 让导入基础资源与 Key 可用性分开。
func (r *Runner) ImportKeys(
	ctx context.Context, conn *pgx.Conn, ch store.Channel, creds []collector.Credential,
) (collector.KeyImportResult, error) {
	var result collector.KeyImportResult
	if len(creds) == 0 {
		return result, fmt.Errorf("%w：没有可用采集凭证", collector.ErrPrecondition)
	}
	adapter, err := r.adapter(ch)
	if err != nil {
		return result, err
	}
	resolver, ok := adapter.(collector.KeySecretResolver)
	if !ok {
		return result, fmt.Errorf("站型 %q 不支持自动读取 Key 明文", ch.SiteFamily)
	}
	refresher, _ := adapter.(collector.Refresher)

	for _, cred := range creds {
		if r.Auth != nil && refresher != nil {
			cred, err = r.Auth.EnsureFresh(ctx, cred, refresher, time.Now())
			if err != nil {
				r.logKeyImportFailure(ch, cred.AccountID, "renew", "", err)
				return result, fmt.Errorf("账号 %d 凭证续期失败", cred.AccountID)
			}
		}
		session, err := adapter.Authenticate(ctx, cred)
		if err != nil {
			r.logKeyImportFailure(ch, cred.AccountID, "authenticate", "", err)
			return result, fmt.Errorf("账号 %d 会话鉴权失败", cred.AccountID)
		}
		keys, err := adapter.FetchKeys(ctx, session)
		if err != nil {
			r.logKeyImportFailure(ch, cred.AccountID, "list", "", err)
			return result, fmt.Errorf("账号 %d 读取 Key 列表失败", cred.AccountID)
		}
		result.Found += len(keys)
		existing, err := store.KeyRefIndex(ctx, conn, cred.AccountID)
		if err != nil {
			r.logKeyImportFailure(ch, cred.AccountID, "lookup_existing", "", err)
			return result, err
		}
		handled := 0
		for _, key := range keys {
			if key.KeyRef == "" {
				r.logKeyImportFailure(ch, cred.AccountID, "validate_ref", "", fmt.Errorf("empty key reference"))
				result.Failed++
				handled++
				continue
			}
			if keyID, found := existing[key.KeyRef]; found {
				if err := updateImportedKey(ctx, conn, ch.ID, keyID, key); err != nil {
					r.logKeyImportFailure(ch, cred.AccountID, "update_existing", key.KeyRef, err)
					result.Failed++
					handled++
					continue
				}
				result.Skipped++
				handled++
				continue
			}
			secret, err := resolver.ResolveKeySecret(ctx, session, key.KeyRef)
			if err != nil {
				r.logKeyImportFailure(ch, cred.AccountID, "resolve_secret", key.KeyRef, err)
				if status, _, ok := collector.HTTPFailure(err); ok && status == 429 {
					result.Deferred += len(keys) - handled
					break
				}
				result.Failed++
				handled++
				continue
			}
			groupID, err := importedGroupID(ctx, conn, ch.ID, key.GroupRef)
			if err != nil {
				r.logKeyImportFailure(ch, cred.AccountID, "lookup_group", key.KeyRef, err)
				result.Failed++
				handled++
				continue
			}
			if _, err := store.CreateKey(ctx, conn, cred.AccountID, secret, key.KeyRef, groupID); err != nil {
				r.logKeyImportFailure(ch, cred.AccountID, "create", key.KeyRef, err)
				result.Failed++
				handled++
				continue
			}
			result.Imported++
			handled++
		}
	}
	return result, nil
}

func (r *Runner) logKeyImportFailure(
	ch store.Channel, accountID int64, stage, keyRef string, err error,
) {
	if r.Logger == nil {
		return
	}
	attrs := []any{
		"channel_id", ch.ID,
		"account_id", accountID,
		"key_ref", keyRef,
		"stage", stage,
		"error_type", fmt.Sprintf("%T", err),
	}
	if status, _, ok := collector.HTTPFailure(err); ok {
		attrs = append(attrs, "http_status", status)
	}
	r.Logger.Warn("导入上游 Key 失败", attrs...)
}

func importedGroupID(
	ctx context.Context, conn *pgx.Conn, channelID int64, groupRef string,
) (*int64, error) {
	if groupRef == "" {
		return nil, nil
	}
	id, found, err := store.GroupIDByRef(ctx, conn, channelID, groupRef)
	if err != nil || !found {
		return nil, err
	}
	return &id, nil
}

func updateImportedKey(
	ctx context.Context, conn *pgx.Conn, channelID, keyID int64, key collector.Key,
) error {
	groupID, err := importedGroupID(ctx, conn, channelID, key.GroupRef)
	if err != nil {
		return err
	}
	return store.UpdateKeyUsage(ctx, conn, store.KeyUsageRow{
		KeyID: keyID, RemainQuotaUSD: key.RemainQuotaUSD, UsedQuotaUSD: key.UsedQuotaUSD,
		ExpiredAt: key.ExpiredAt, ChannelGroupID: groupID, Unlimited: key.Unlimited,
		SyncedAt: key.Meta.FetchedAt, RPMLimit: intPtr(key.RateLimit.RPM),
		ConcurrencyLimit: intPtr(key.RateLimit.Concurrency),
	})
}

func intPtr(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}
