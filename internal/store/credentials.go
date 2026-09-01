package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// CredentialStore 持久化采集凭证（一期明文，FR-113）。
//
// 实现 collector.CredentialStore —— 那个接口存在的理由是让
// "刷新结果先持久化再释放锁"（不变式 S-1）可以被单测覆盖。
type CredentialStore struct{ Pool *Pool }

// NewCredentialStore 构造。
func NewCredentialStore(p *Pool) *CredentialStore { return &CredentialStore{Pool: p} }

// SaveTx 在**调用方给的**执行器上持久化凭证。
//
// 为什么要与 Save 分成两个（2026-08-29 二次评审）：两个调用点要的不是一件事 ——
//
//	· 续期（Authenticator）：无外层事务，需要自己取连接 → Save
//	· 导入（importOne）：四处写入必须同生共死 → SaveTx，传入 tx
//
// 原先只有前一种，于是导入侧无论怎么包事务都盖不住这处写入：它自取连接、
// 独立提交，中途失败就留下"渠道在、凭证没有"的半成品，而重导会按 base_url
// 判为已存在直接跳过 —— 永不自愈。
func (s *CredentialStore) SaveTx(ctx context.Context, db DBTX, cred collector.Credential) error {
	lockKey := collector.RefreshLockKey(cred)
	_, err := db.Exec(ctx, `
INSERT INTO collector_credentials (account_id, channel_id, site_family, cred_type,
       access_token, refresh_token, external_user_id,
       user_id_header_name, token_expires_at, refresh_lock_key, status, updated_at)
VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),
        NULLIF($8,''),$9,NULLIF($10,''),'valid',now())
ON CONFLICT (account_id) DO UPDATE
   SET channel_id         = EXCLUDED.channel_id,
       site_family        = EXCLUDED.site_family,
       cred_type          = EXCLUDED.cred_type,
       access_token       = COALESCE(NULLIF(EXCLUDED.access_token,''), collector_credentials.access_token),
       refresh_token      = COALESCE(NULLIF(EXCLUDED.refresh_token,''), collector_credentials.refresh_token),
       external_user_id   = COALESCE(EXCLUDED.external_user_id, collector_credentials.external_user_id),
       user_id_header_name = COALESCE(EXCLUDED.user_id_header_name, collector_credentials.user_id_header_name),
       token_expires_at   = EXCLUDED.token_expires_at,
       refresh_lock_key   = COALESCE(NULLIF(collector_credentials.refresh_lock_key,''), EXCLUDED.refresh_lock_key),
       status             = 'valid',
       updated_at         = now()`,
		cred.AccountID, cred.ChannelID, string(cred.Family), cred.CredType,
		cred.AccessToken, cred.RefreshToken, cred.ExternalUserID,
		cred.UserIDHeaderName, nullTime(cred.TokenExpiresAt), lockKey)
	if err != nil {
		return fmt.Errorf("保存账号 %d 凭证: %w", cred.AccountID, err)
	}
	return nil
}

// WithRefreshLock runs one refresh under a PostgreSQL transaction-level advisory lock.
// The callback receives the credential reloaded after the lock is acquired.
func (s *CredentialStore) WithRefreshLock(
	ctx context.Context, cred collector.Credential,
	refresh func(collector.Credential) (collector.Credential, bool, error),
) (collector.Credential, error) {
	conn, release, err := s.Pool.Acquire(ctx)
	if err != nil {
		return cred, err
	}
	defer release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return cred, fmt.Errorf("开启凭证刷新事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockKey string
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(refresh_lock_key,'')
  FROM collector_credentials WHERE account_id=$1`, cred.AccountID).Scan(&lockKey); err != nil {
		return cred, fmt.Errorf("读账号 %d 刷新锁键: %w", cred.AccountID, err)
	}
	if lockKey == "" {
		return cred, fmt.Errorf("账号 %d 缺 refresh_lock_key", cred.AccountID)
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return cred, fmt.Errorf("获取账号 %d 刷新锁: %w", cred.AccountID, err)
	}
	current, err := s.loadByAccount(ctx, tx, cred.AccountID, true)
	if err != nil {
		return cred, err
	}
	if current.RefreshLockKey != lockKey {
		return cred, fmt.Errorf("账号 %d 刷新锁键已变化", cred.AccountID)
	}
	fresh, changed, err := refresh(current)
	if err != nil {
		return current, err
	}
	if changed {
		fresh.RefreshLockKey = lockKey
		tag, err := tx.Exec(ctx, `
UPDATE collector_credentials
   SET access_token = NULLIF($2,''),
       refresh_token = COALESCE(NULLIF($3,''), refresh_token),
       token_expires_at = $4,
       status = 'valid', updated_at = now()
 WHERE refresh_lock_key = $1 AND site_family = $5`, lockKey, fresh.AccessToken,
			fresh.RefreshToken, nullTime(fresh.TokenExpiresAt), string(fresh.Family))
		if err != nil {
			return current, fmt.Errorf("刷新后保存账号 %d 凭证: %w", cred.AccountID, err)
		}
		if tag.RowsAffected() == 0 {
			return current, fmt.Errorf("刷新后保存账号 %d 凭证: 刷新锁键 %q 没有关联凭证",
				cred.AccountID, lockKey)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return current, fmt.Errorf("提交账号 %d 凭证刷新: %w", cred.AccountID, err)
	}
	if changed {
		return fresh, nil
	}
	return current, nil
}

// ListByChannel 读取渠道下全部有效账号的采集凭证。
func (s *CredentialStore) ListByChannel(
	ctx context.Context, db DBTX, ch Channel,
) ([]collector.Credential, error) {
	rows, err := db.Query(ctx, `
SELECT c.account_id, c.site_family, c.cred_type, c.access_token, c.refresh_token,
       c.external_user_id, c.user_id_header_name, c.token_expires_at, c.refresh_lock_key,
       c.channel_id, ch.base_url
  FROM collector_credentials AS c
  JOIN upstream_accounts AS a ON a.id = c.account_id
  JOIN channels AS ch ON ch.id = c.channel_id
 WHERE c.channel_id=$1 AND c.status='valid' AND a.status='active'
 ORDER BY c.account_id`, ch.ID)
	if err != nil {
		return nil, fmt.Errorf("列渠道 %d 凭证: %w", ch.ID, err)
	}
	defer rows.Close()

	out := []collector.Credential{}
	for rows.Next() {
		var cred collector.Credential
		if err := scanCredential(rows, &cred); err != nil {
			return nil, fmt.Errorf("读渠道 %d 凭证: %w", ch.ID, err)
		}
		out = append(out, cred)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列渠道 %d 凭证: %w", ch.ID, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: 渠道 %d 未登记有效账号凭证", ErrNotFound, ch.ID)
	}
	return out, nil
}

func (s *CredentialStore) loadByAccount(
	ctx context.Context, db DBTX, accountID int64, forUpdate bool,
) (collector.Credential, error) {
	var cred collector.Credential
	query := `
SELECT c.account_id, c.site_family, c.cred_type, c.access_token, c.refresh_token,
       c.external_user_id, c.user_id_header_name, c.token_expires_at, c.refresh_lock_key,
       c.channel_id, ch.base_url
  FROM collector_credentials AS c
  JOIN channels AS ch ON ch.id = c.channel_id
 WHERE c.account_id=$1`
	if forUpdate {
		query += " FOR UPDATE OF c"
	}
	row := db.QueryRow(ctx, query, accountID)
	err := scanCredential(row, &cred)
	if errors.Is(err, pgx.ErrNoRows) {
		return cred, fmt.Errorf("%w: 账号 %d 未登记采集凭证", ErrNotFound, accountID)
	}
	if err != nil {
		return cred, fmt.Errorf("读账号 %d 凭证: %w", accountID, err)
	}
	return cred, nil
}

type credentialScanner interface {
	Scan(dest ...any) error
}

func scanCredential(row credentialScanner, cred *collector.Credential) error {
	var family, credType string
	var access, refresh, extUID, hdrName, lockKey *string
	var expiresAt *time.Time
	if err := row.Scan(&cred.AccountID, &family, &credType, &access, &refresh,
		&extUID, &hdrName, &expiresAt, &lockKey, &cred.ChannelID, &cred.BaseURL); err != nil {
		return err
	}
	cred.Family = collector.Family(family)
	cred.CredType = credType
	cred.AccessToken = deref(access)
	cred.RefreshToken = deref(refresh)
	cred.ExternalUserID = deref(extUID)
	cred.UserIDHeaderName = deref(hdrName)
	cred.RefreshLockKey = deref(lockKey)
	if expiresAt != nil {
		cred.TokenExpiresAt = *expiresAt
	}
	return nil
}

// SaveDetected 把 Detect 的结果登记为凭证骨架（尚无令牌）。
//
// 用途：运维在 web 端建渠道时选了自动探测，此时就把 quota_per_unit 等
// 家族特征存下来 —— 它是 NewAPI 系额度换算的必需输入，而 FetchAccount
// **缺它会直接报错**（不猜，猜错差 50 万倍）。
// SaveDetected 在调用方给的执行器上落探测结果。理由同 SaveTx。
//
// 这一处没有"自取连接"的变体：唯一调用方是导入与建渠道，两者都有连接在手。
func (s *CredentialStore) SaveDetected(
	ctx context.Context, db DBTX, channelID int64, d collector.DetectResult,
) error {
	payload := map[string]any{"family": string(d.Family)}
	if d.Version != "" {
		payload["version"] = d.Version
	}
	if d.QuotaPerUnit > 0 {
		payload["quota_per_unit"] = d.QuotaPerUnit
	}
	payload["no_shield"] = d.NoShield
	return InsertSnapshot(ctx, db, SnapshotRow{
		ChannelID: channelID, ScopeType: "pricing", ScopeID: "__detect__",
		Payload: payload, DataSource: "auto_collect", FetchedAt: d.Meta.FetchedAt,
	})
}

// QuotaPerUnit 取该渠道最近探测到的额度换算基数。
//
// 逐站读取、**不写死**（04 §2：upstream-d.invalid 是 500000，别家不一定）。
// 取不到返回 0，由调用方决定是否报错 —— FetchAccount 会报错而非猜。
func QuotaPerUnit(ctx context.Context, conn *pgx.Conn, channelID int64) float64 {
	var v *float64
	err := conn.QueryRow(ctx, `
SELECT (payload->>'quota_per_unit')::float8
  FROM collector_snapshots
 WHERE channel_id=$1 AND scope_type='pricing' AND scope_id='__detect__'
   AND payload ? 'quota_per_unit'
 ORDER BY fetched_at DESC LIMIT 1`, channelID).Scan(&v)
	if err != nil || v == nil {
		return 0
	}
	return *v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullTime(t interface{ IsZero() bool }) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t
}

var _ collector.CredentialStore = (*CredentialStore)(nil)
