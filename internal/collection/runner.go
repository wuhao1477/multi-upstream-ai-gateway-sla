// Package collection assembles channel adapters and runs manual or periodic collection.
package collection

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
		imported, err := r.importFetchedKeys(ctx, conn, ch, cred, session, resolver, keys)
		mergeKeyImportResult(&result, imported)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// ProvisionKeys 补齐一个账号在目标分组中缺少的远端 Key，并立即登记明文。
func (r *Runner) ProvisionKeys(
	ctx context.Context, conn *pgx.Conn, ch store.Channel, cred collector.Credential,
	request collector.KeyProvisionRequest,
) (collector.KeyProvisionResult, error) {
	var result collector.KeyProvisionResult
	adapter, err := r.adapter(ch)
	if err != nil {
		return result, err
	}
	resolver, ok := adapter.(collector.KeySecretResolver)
	if !ok {
		return result, fmt.Errorf("站型 %q 不支持自动读取 Key 明文", ch.SiteFamily)
	}
	refresher, _ := adapter.(collector.Refresher)
	if r.Auth != nil && refresher != nil {
		cred, err = r.Auth.EnsureFresh(ctx, cred, refresher, time.Now())
		if err != nil {
			return result, fmt.Errorf("账号 %d 凭证续期失败", cred.AccountID)
		}
	}
	session, err := adapter.Authenticate(ctx, cred)
	if err != nil {
		return result, fmt.Errorf("账号 %d 会话鉴权失败", cred.AccountID)
	}
	var imported, importFailed int
	result, err = provisionRemoteKeys(ctx, adapter, session, request,
		func(keys []collector.Key, groups []collector.Group) error {
			if request.DryRun {
				return nil
			}
			if r.Sink != nil {
				if _, err := r.Sink.SaveGroups(ctx, ch.ID, groups); err != nil {
					return err
				}
			}
			got, err := r.importFetchedKeys(ctx, conn, ch, cred, session, resolver, keys)
			imported += got.Imported
			importFailed += got.Failed + got.Deferred
			return err
		},
		func(key collector.Key) error {
			got, err := r.importFetchedKeys(ctx, conn, ch, cred, session, resolver, []collector.Key{key})
			imported += got.Imported
			importFailed += got.Failed + got.Deferred
			if err != nil {
				return err
			}
			if got.Imported != 1 {
				return fmt.Errorf("新建 Key 未能登记")
			}
			return nil
		})
	result.Imported = imported
	result.Failed += importFailed
	return result, err
}

func (r *Runner) importFetchedKeys(
	ctx context.Context, conn *pgx.Conn, ch store.Channel, cred collector.Credential,
	session collector.Session, resolver collector.KeySecretResolver, keys []collector.Key,
) (collector.KeyImportResult, error) {
	result := collector.KeyImportResult{Found: len(keys)}
	existing, err := store.KeyRefIndex(ctx, conn, cred.AccountID)
	if err != nil {
		r.logKeyImportFailure(ch, cred.AccountID, "lookup_existing", "", err)
		return result, err
	}
	for index, key := range keys {
		if key.KeyRef == "" {
			result.Failed++
			continue
		}
		if keyID, found := existing[key.KeyRef]; found {
			if err := updateImportedKey(ctx, conn, ch.ID, keyID, key); err != nil {
				r.logKeyImportFailure(ch, cred.AccountID, "update_existing", key.KeyRef, err)
				result.Failed++
				continue
			}
			result.Skipped++
			continue
		}
		secret, err := resolver.ResolveKeySecret(ctx, session, key.KeyRef)
		if err != nil {
			r.logKeyImportFailure(ch, cred.AccountID, "resolve_secret", key.KeyRef, err)
			if status, _, ok := collector.HTTPFailure(err); ok && status == 429 {
				result.Deferred += len(keys) - index
				break
			}
			result.Failed++
			continue
		}
		groupID, err := importedGroupID(ctx, conn, ch.ID, key.GroupRef)
		if err != nil {
			result.Failed++
			continue
		}
		if _, err := store.CreateKey(ctx, conn, cred.AccountID, secret, key.KeyRef, groupID); err != nil {
			r.logKeyImportFailure(ch, cred.AccountID, "create", key.KeyRef, err)
			result.Failed++
			continue
		}
		existing[key.KeyRef] = 0
		result.Imported++
	}
	return result, nil
}

func mergeKeyImportResult(dst *collector.KeyImportResult, src collector.KeyImportResult) {
	dst.Found += src.Found
	dst.Imported += src.Imported
	dst.Skipped += src.Skipped
	dst.Failed += src.Failed
	dst.Deferred += src.Deferred
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

func selectProvisionGroups(
	keys []collector.Key, groups []collector.Group, model string, onlyWithoutKeys bool,
) ([]collector.Group, string) {
	if onlyWithoutKeys && len(keys) > 0 {
		return nil, "账号已有远端 Key"
	}
	covered := map[string]struct{}{}
	for _, key := range keys {
		covered[key.GroupRef] = struct{}{}
	}
	seen := map[string]struct{}{}
	out := make([]collector.Group, 0, len(groups))
	for _, group := range groups {
		if group.GroupRef == "" || (model != "" && !slices.Contains(group.AvailableModels, model)) {
			continue
		}
		if _, exists := covered[group.GroupRef]; exists {
			continue
		}
		if _, exists := seen[group.GroupRef]; exists {
			continue
		}
		seen[group.GroupRef] = struct{}{}
		out = append(out, group)
	}
	return out, ""
}

func provisionRemoteKeys(
	ctx context.Context, adapter collector.Adapter, session collector.Session,
	request collector.KeyProvisionRequest, onDiscovered func([]collector.Key, []collector.Group) error,
	onCreated func(collector.Key) error,
) (collector.KeyProvisionResult, error) {
	var result collector.KeyProvisionResult
	provisioner, ok := adapter.(collector.KeyProvisioner)
	if !ok {
		return result, fmt.Errorf("站型 %q 不支持远端创建 Key", session.Family)
	}
	keys, err := adapter.FetchKeys(ctx, session)
	if err != nil {
		return result, err
	}
	groups, err := adapter.FetchGroups(ctx, session)
	if err != nil {
		return result, err
	}
	result.Found = len(keys)
	if onDiscovered != nil {
		if err := onDiscovered(keys, groups); err != nil {
			return result, err
		}
	}
	missing, reason := selectProvisionGroups(keys, groups, request.Model, request.OnlyWithoutKeys)
	if reason != "" {
		result.SkippedReason = reason
		return result, nil
	}
	result.MatchedGroups = countProvisionGroups(groups, request.Model)
	result.ExistingGroups = result.MatchedGroups - len(missing)
	result.WouldCreate = len(missing)
	if request.DryRun {
		return result, nil
	}

	for _, group := range missing {
		before := keyRefs(keys)
		if err := provisioner.CreateRemoteKey(ctx, session, collector.RemoteKeyRequest{
			Name: "gateway-auto", GroupRef: group.GroupRef,
		}); err != nil {
			result.Failed++
			continue
		}
		after, err := adapter.FetchKeys(ctx, session)
		if err != nil {
			result.Failed++
			return result, fmt.Errorf("创建后核对 Key 列表: %w", err)
		}
		created, ok := singleCreatedKey(before, after, group.GroupRef)
		if !ok || onCreated(created) != nil {
			result.Failed++
			keys = after
			continue
		}
		result.Created++
		keys = after
	}
	return result, nil
}

func countProvisionGroups(groups []collector.Group, model string) int {
	seen := map[string]struct{}{}
	for _, group := range groups {
		if group.GroupRef != "" && (model == "" || slices.Contains(group.AvailableModels, model)) {
			seen[group.GroupRef] = struct{}{}
		}
	}
	return len(seen)
}

func keyRefs(keys []collector.Key) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		out[key.KeyRef] = struct{}{}
	}
	return out
}

func singleCreatedKey(before map[string]struct{}, after []collector.Key, groupRef string) (collector.Key, bool) {
	var found []collector.Key
	for _, key := range after {
		if _, exists := before[key.KeyRef]; !exists && key.GroupRef == groupRef {
			found = append(found, key)
		}
	}
	if len(found) != 1 {
		return collector.Key{}, false
	}
	return found[0], true
}
