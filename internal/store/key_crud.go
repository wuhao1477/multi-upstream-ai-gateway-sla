package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Key 是一把上游 Key。**响应中永不含明文**（FR-094）。
type Key struct {
	ID             int64    `json:"id"`
	AccountID      int64    `json:"account_id"`
	ChannelID      int64    `json:"channel_id"`
	SecretPrefix   string   `json:"secret_prefix"`
	ExternalRef    string   `json:"external_ref,omitempty"`
	ChannelGroupID *int64   `json:"channel_group_id,omitempty"`
	GroupRef       string   `json:"group_ref,omitempty"`
	RateMultiplier *float64 `json:"rate_multiplier,omitempty"`
	// GroupInherited 为真时，上面那个分组不是这把 Key 自己定的，而是**跟账号走**
	// （上游 /api/token 的 group 是空串 → 调用走 /api/user/self 的 group）。
	//
	// 必须与"自己定的"分开：两者的倍率同样真实，但改法不同 —— 账号分组变了，
	// 这一把跟着变；自己定的那把不变。界面上混成一样会让人改错地方。
	GroupInherited bool     `json:"group_inherited"`
	RemainQuotaUSD *float64 `json:"remain_quota_usd,omitempty"`
	UsedQuotaUSD   *float64 `json:"used_quota_usd,omitempty"`
	// UnlimitedQuota 为真时 remain_quota_usd 无意义（上游对无限额 Key 回 0）。
	// 不加 omitempty：false 也必须出现在响应里，否则前端分不清
	// "这把 Key 有限额" 和 "后端版本旧、还没这个字段"。
	UnlimitedQuota   bool       `json:"unlimited_quota"`
	RPMLimit         *int       `json:"rpm_limit,omitempty"`
	ConcurrencyLimit *int       `json:"concurrency_limit,omitempty"`
	Status           string     `json:"status"`
	ExpiredTime      *time.Time `json:"expired_time,omitempty"`
	QuotaSyncedAt    *time.Time `json:"quota_synced_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

// CreateKey 登记一把 Key。明文只在此处接收，之后永不回显。
func CreateKey(ctx context.Context, conn *pgx.Conn, accountID int64,
	secret, externalRef string, groupID *int64) (int64, error) {
	if secret == "" {
		return 0, fmt.Errorf("secret 不可为空")
	}
	var id int64
	err := conn.QueryRow(ctx, `
INSERT INTO upstream_keys (account_id, secret, external_ref, channel_group_id, status)
VALUES ($1,$2,NULLIF($3,''),$4,'active')
RETURNING id`, accountID, secret, externalRef, groupID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("建 Key（账号 %d）: %w", accountID, err)
	}
	return id, nil
}

// secretPrefixExpr 是脱敏展示表达式。
//
// **只取前 8 位**（09：列表只显示 secret_prefix，永不回显完整凭证）。
// 用 SQL 表达式而非 Go 侧截断：这样完整 secret 根本不出库，
// 少一处可能被日志/错误信息带出去的路径。
const secretPrefixExpr = `left(secret, 8) || '…'`

// accountGroupJoin 解析「跟账号走」的那一档分组。
//
// 上游 /api/token 的 `group` 为空串时，调用实际走的是**账号自己的分组**
// （/api/user/self 的 group，实测两个真站点都是 "default"）。所以这种 Key
// 不是"未归组"，只是没有自己的分组 —— 它有明确的倍率，只是要去账号上取。
//
// 按**名字**在同渠道里找，不是按外键：账号分组由上游随时可改，而我方采到的
// channel_groups 只覆盖 group_ratio 里列出的那些。实测有站点的账号分组叫
// default，但它的 group_ratio 里没有 default —— 那时这个 join 匹配不上，
// GroupRef 仍为空，界面照旧显示未归组。这是对的：名字有、倍率没采到，
// 不能替它编一个。
const accountGroupJoin = `LEFT JOIN channel_groups ga
         ON ga.channel_id = a.channel_id AND ga.group_ref = a.account_group`

// ListKeys 列出 Key（脱敏）。
func ListKeys(ctx context.Context, conn *pgx.Conn, channelID int64) ([]Key, error) {
	rows, err := conn.Query(ctx, `
SELECT k.id, k.account_id, a.channel_id, `+secretPrefixExpr+`,
       COALESCE(k.external_ref,''), k.channel_group_id,
       COALESCE(g.group_ref, ga.group_ref, ''),
       COALESCE(g.rate_multiplier, ga.rate_multiplier),
       (k.channel_group_id IS NULL AND ga.id IS NOT NULL) AS group_inherited,
       k.remain_quota_usd, k.used_quota_usd, COALESCE(k.unlimited_quota,false),
       k.rpm_limit, k.concurrency_limit,
       k.status, k.expired_time, k.quota_synced_at, k.created_at
  FROM upstream_keys k
  JOIN upstream_accounts a ON a.id = k.account_id
  LEFT JOIN channel_groups g ON g.id = k.channel_group_id
  `+accountGroupJoin+`
 WHERE ($1 <= 0 OR a.channel_id = $1)
 ORDER BY k.id`, channelID)
	if err != nil {
		return nil, fmt.Errorf("列 Key: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.AccountID, &k.ChannelID, &k.SecretPrefix,
			&k.ExternalRef, &k.ChannelGroupID, &k.GroupRef, &k.RateMultiplier,
			&k.GroupInherited,
			&k.RemainQuotaUSD, &k.UsedQuotaUSD, &k.UnlimitedQuota,
			&k.RPMLimit, &k.ConcurrencyLimit, &k.Status, &k.ExpiredTime,
			&k.QuotaSyncedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKey 取单把 Key（脱敏）。
func GetKey(ctx context.Context, conn *pgx.Conn, id int64) (Key, error) {
	var k Key
	err := conn.QueryRow(ctx, `
SELECT k.id, k.account_id, a.channel_id, `+secretPrefixExpr+`,
       COALESCE(k.external_ref,''), k.channel_group_id,
       COALESCE(g.group_ref, ga.group_ref, ''),
       COALESCE(g.rate_multiplier, ga.rate_multiplier),
       (k.channel_group_id IS NULL AND ga.id IS NOT NULL) AS group_inherited,
       k.remain_quota_usd, k.used_quota_usd, COALESCE(k.unlimited_quota,false),
       k.rpm_limit, k.concurrency_limit,
       k.status, k.expired_time, k.quota_synced_at, k.created_at
  FROM upstream_keys k
  JOIN upstream_accounts a ON a.id = k.account_id
  LEFT JOIN channel_groups g ON g.id = k.channel_group_id
  `+accountGroupJoin+`
 WHERE k.id = $1`, id).Scan(&k.ID, &k.AccountID, &k.ChannelID, &k.SecretPrefix,
		&k.ExternalRef, &k.ChannelGroupID, &k.GroupRef, &k.RateMultiplier,
		&k.GroupInherited,
		&k.RemainQuotaUSD, &k.UsedQuotaUSD, &k.UnlimitedQuota,
		&k.RPMLimit, &k.ConcurrencyLimit, &k.Status, &k.ExpiredTime,
		&k.QuotaSyncedAt, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, fmt.Errorf("%w: Key %d", ErrNotFound, id)
	}
	if err != nil {
		return k, fmt.Errorf("查 Key %d: %w", id, err)
	}
	return k, nil
}

// KeyPatch 是 Key 的局部更新。
type KeyPatch struct {
	ID                int64
	Secret            *string
	ExternalRef       *string
	ChannelGroupID    *int64
	ChannelGroupIDSet bool
	RemainQuotaUSD    *float64
	UsedQuotaUSD      *float64
	RPMLimit          *int
	ConcurrencyLimit  *int
	Status            *string
	ExpiredTime       *time.Time
}

// UpdateKey 更新 Key 可变字段。读路径仍只暴露脱敏前缀。
func UpdateKey(ctx context.Context, conn *pgx.Conn, p KeyPatch) error {
	tag, err := conn.Exec(ctx, `
UPDATE upstream_keys
   SET secret = COALESCE($2, secret),
	       external_ref = CASE WHEN $3::text IS NULL THEN external_ref ELSE NULLIF($3,'') END,
	       channel_group_id = CASE WHEN $4 THEN $5 ELSE channel_group_id END,
	       remain_quota_usd = COALESCE($6, remain_quota_usd),
	       used_quota_usd = COALESCE($7, used_quota_usd),
	       rpm_limit = COALESCE($8, rpm_limit),
	       concurrency_limit = COALESCE($9, concurrency_limit),
	       status = COALESCE($10, status),
	       expired_time = COALESCE($11, expired_time),
       updated_at = now()
	 WHERE id = $1`, p.ID, p.Secret, p.ExternalRef, p.ChannelGroupIDSet, p.ChannelGroupID,
		p.RemainQuotaUSD, p.UsedQuotaUSD, p.RPMLimit, p.ConcurrencyLimit,
		p.Status, p.ExpiredTime)
	if err != nil {
		return asDuplicate(fmt.Errorf("更新 Key %d: %w", p.ID, err),
			"该账号下 external_ref 已存在")
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: Key %d", ErrNotFound, p.ID)
	}
	return nil
}

// DisableKey 停用一把 Key（FR-004/095 的 Key 层）。
func DisableKey(ctx context.Context, conn *pgx.Conn, id int64) error {
	tag, err := conn.Exec(ctx,
		`UPDATE upstream_keys SET status='revoked', updated_at=now() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("停用 Key %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: Key %d", ErrNotFound, id)
	}
	return nil
}

// DeleteKey 删除一把 Key。
func DeleteKey(ctx context.Context, conn *pgx.Conn, id int64) error {
	tag, err := conn.Exec(ctx, `DELETE FROM upstream_keys WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("删除 Key %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: Key %d", ErrNotFound, id)
	}
	return nil
}
