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
INSERT INTO collector_credentials (channel_id, site_family, cred_type,
       access_token, refresh_token, username, password, external_user_id,
       user_id_header_name, token_expires_at, refresh_lock_key, status, updated_at)
VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),
        NULLIF($8,''),NULLIF($9,''),$10,NULLIF($11,''),'valid',now())
ON CONFLICT (channel_id) DO UPDATE
   SET access_token       = COALESCE(NULLIF(EXCLUDED.access_token,''), collector_credentials.access_token),
       refresh_token      = COALESCE(NULLIF(EXCLUDED.refresh_token,''), collector_credentials.refresh_token),
       username           = COALESCE(EXCLUDED.username, collector_credentials.username),
       password           = COALESCE(EXCLUDED.password, collector_credentials.password),
       external_user_id   = COALESCE(EXCLUDED.external_user_id, collector_credentials.external_user_id),
       user_id_header_name = COALESCE(EXCLUDED.user_id_header_name, collector_credentials.user_id_header_name),
       token_expires_at   = EXCLUDED.token_expires_at,
       refresh_lock_key   = COALESCE(NULLIF(collector_credentials.refresh_lock_key,''), EXCLUDED.refresh_lock_key),
       status             = 'valid',
       updated_at         = now()`,
		cred.ChannelID, string(cred.Family), cred.CredType,
		cred.AccessToken, cred.RefreshToken, cred.Username, cred.Password,
		cred.ExternalUserID, cred.UserIDHeaderName, nullTime(cred.TokenExpiresAt), lockKey)
	if err != nil {
		return fmt.Errorf("保存渠道 %d 凭证: %w", cred.ChannelID, err)
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
  FROM collector_credentials WHERE channel_id=$1`, cred.ChannelID).Scan(&lockKey); err != nil {
		return cred, fmt.Errorf("读渠道 %d 刷新锁键: %w", cred.ChannelID, err)
	}
	if lockKey == "" {
		return cred, fmt.Errorf("渠道 %d 缺 refresh_lock_key", cred.ChannelID)
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return cred, fmt.Errorf("获取渠道 %d 刷新锁: %w", cred.ChannelID, err)
	}
	current, err := s.load(ctx, tx, Channel{ID: cred.ChannelID, BaseURL: cred.BaseURL}, true)
	if err != nil {
		return cred, err
	}
	if current.RefreshLockKey != lockKey {
		return cred, fmt.Errorf("渠道 %d 刷新锁键已变化", cred.ChannelID)
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
			return current, fmt.Errorf("刷新后保存渠道 %d 凭证: %w", cred.ChannelID, err)
		}
		if tag.RowsAffected() == 0 {
			return current, fmt.Errorf("刷新后保存渠道 %d 凭证: 刷新锁键 %q 没有关联凭证",
				cred.ChannelID, lockKey)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return current, fmt.Errorf("提交渠道 %d 凭证刷新: %w", cred.ChannelID, err)
	}
	if changed {
		return fresh, nil
	}
	return current, nil
}

// Load 读取某渠道的采集凭证。
func (s *CredentialStore) Load(ctx context.Context, conn DBTX, ch Channel) (collector.Credential, error) {
	return s.load(ctx, conn, ch, false)
}

func (s *CredentialStore) load(
	ctx context.Context, conn DBTX, ch Channel, forUpdate bool,
) (collector.Credential, error) {
	var cred collector.Credential
	var family, credType string
	var access, refresh, user, pass, extUID, hdrName, lockKey *string
	// ⚠️ 必须用指针接 token_expires_at：**NewAPI 的长期令牌没有到期时间**
	// （04 §5.1），该列为 NULL。用 time.Time 直接扫会报
	// "cannot scan NULL into *time.Time" —— 而那正是最常见的站型，
	// 等于 NewAPI 渠道一次都采不成。
	var expiresAt *time.Time

	query := `
SELECT site_family, cred_type, access_token, refresh_token, username, password,
	       external_user_id, user_id_header_name, token_expires_at, refresh_lock_key
  FROM collector_credentials WHERE channel_id=$1`
	if forUpdate {
		query += " FOR UPDATE"
	}
	err := conn.QueryRow(ctx, query, ch.ID).
		Scan(&family, &credType, &access, &refresh, &user, &pass,
			&extUID, &hdrName, &expiresAt, &lockKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return cred, fmt.Errorf("%w: 渠道 %d 未登记采集凭证", ErrNotFound, ch.ID)
	}
	if err != nil {
		return cred, fmt.Errorf("读渠道 %d 凭证: %w", ch.ID, err)
	}

	cred.ChannelID = ch.ID
	cred.Family = collector.Family(family)
	cred.CredType = credType
	cred.BaseURL = ch.BaseURL
	cred.AccessToken = deref(access)
	cred.RefreshToken = deref(refresh)
	cred.Username = deref(user)
	cred.Password = deref(pass)
	cred.ExternalUserID = deref(extUID)
	cred.UserIDHeaderName = deref(hdrName)
	cred.RefreshLockKey = deref(lockKey)
	if expiresAt != nil {
		cred.TokenExpiresAt = *expiresAt
	}
	// 留零值即"无到期时间"：NeedsRefresh 对零值返 false（NewAPI 本就不刷新）
	return cred, nil
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
