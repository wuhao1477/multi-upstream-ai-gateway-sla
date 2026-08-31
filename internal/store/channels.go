package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound 是通用的"记录不存在"。
var ErrNotFound = errors.New("store: 记录不存在")

// Channel 是一个上游渠道。
type Channel struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	SiteFamily     string     `json:"site_family"`
	BaseURL        string     `json:"base_url"`
	Status         string     `json:"status"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	DisabledUntil  *time.Time `json:"disabled_until,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// CreateChannel 登记一个渠道。
func CreateChannel(ctx context.Context, conn DBTX, c Channel) (int64, error) {
	if c.SiteFamily == "" {
		// 站型未知时显式写 unknown，而不是留空 —— CHECK 约束只认四个值，
		// 且 unknown 有明确语义（04 §7：走未知家族接入流程）
		c.SiteFamily = "unknown"
	}
	var id int64
	err := conn.QueryRow(ctx, `
INSERT INTO channels (name, site_family, base_url, status)
VALUES ($1,$2,$3,COALESCE(NULLIF($4,''),'enabled'))
RETURNING id`, c.Name, c.SiteFamily, c.BaseURL, c.Status).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("建渠道 %q: %w", c.Name, err)
	}
	return id, nil
}

// ListChannels 列出全部渠道。
func ListChannels(ctx context.Context, conn DBTX) ([]Channel, error) {
	rows, err := conn.Query(ctx, `
SELECT id, name, site_family, base_url, status,
       COALESCE(disabled_reason,''), disabled_until, created_at, updated_at
  FROM channels ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("列渠道: %w", err)
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.Name, &c.SiteFamily, &c.BaseURL, &c.Status,
			&c.DisabledReason, &c.DisabledUntil, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetChannel 取单个渠道。
func GetChannel(ctx context.Context, conn *pgx.Conn, id int64) (Channel, error) {
	var c Channel
	err := conn.QueryRow(ctx, `
SELECT id, name, site_family, base_url, status,
       COALESCE(disabled_reason,''), disabled_until, created_at, updated_at
  FROM channels WHERE id=$1`, id).Scan(&c.ID, &c.Name, &c.SiteFamily, &c.BaseURL,
		&c.Status, &c.DisabledReason, &c.DisabledUntil, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, fmt.Errorf("%w: 渠道 %d", ErrNotFound, id)
	}
	if err != nil {
		return c, fmt.Errorf("查渠道 %d: %w", id, err)
	}
	return c, nil
}

// UpdateChannel 更新渠道可变字段。
func UpdateChannel(ctx context.Context, conn *pgx.Conn, c Channel) error {
	tag, err := conn.Exec(ctx, `
UPDATE channels
   SET name = COALESCE(NULLIF($2,''), name),
       site_family = COALESCE(NULLIF($3,''), site_family),
       base_url = COALESCE(NULLIF($4,''), base_url),
       status = COALESCE(NULLIF($5,''), status),
       -- 停用原因/有效期**只随 status 变更而变**，不像上面几列那样"空则不动"：
       --   传 disabled → 用传入值（FR-095 要求填原因，由处理器强制非空）
       --   传 enabled  → 清空（留着上次的原因，界面上会显示"已启用"却带着停用理由）
       --   没传 status → 原样不动
       -- 原先这两列是无条件赋值，于是"只改个名字"的 PATCH 会把停用原因悄悄抹掉，
       -- 留下一个 status='disabled' 而没人知道为什么的渠道（库里无 CHECK 拦这个）。
       disabled_reason = CASE $5
                           WHEN 'disabled' THEN NULLIF($6,'')
                           WHEN 'enabled'  THEN NULL
                           ELSE disabled_reason END,
       disabled_until  = CASE $5
                           WHEN 'disabled' THEN $7
                           WHEN 'enabled'  THEN NULL
                           ELSE disabled_until END,
       updated_at = now()
 WHERE id = $1`, c.ID, c.Name, c.SiteFamily, c.BaseURL, c.Status,
		c.DisabledReason, c.DisabledUntil)
	if err != nil {
		return fmt.Errorf("更新渠道 %d: %w", c.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 渠道 %d", ErrNotFound, c.ID)
	}
	return nil
}

// Account 是一个上游账号。
type Account struct {
	ID              int64     `json:"id"`
	ChannelID       int64     `json:"channel_id"`
	ExternalUserID  string    `json:"external_user_id,omitempty"`
	BalanceGroupKey string    `json:"balance_group_key,omitempty"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

// CreateAccount 登记账号。
func CreateAccount(ctx context.Context, conn DBTX, a Account) (int64, error) {
	var id int64
	err := conn.QueryRow(ctx, `
INSERT INTO upstream_accounts (channel_id, external_user_id, balance_group_key, status)
VALUES ($1, NULLIF($2,''), NULLIF($3,''), COALESCE(NULLIF($4,''),'active'))
RETURNING id`, a.ChannelID, a.ExternalUserID, a.BalanceGroupKey, a.Status).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("建账号（渠道 %d）: %w", a.ChannelID, err)
	}
	return id, nil
}

// ListAccounts 列出某渠道的账号（channelID<=0 表示全部）。
func ListAccounts(ctx context.Context, conn *pgx.Conn, channelID int64) ([]Account, error) {
	rows, err := conn.Query(ctx, `
SELECT id, channel_id, COALESCE(external_user_id,''), COALESCE(balance_group_key,''),
       status, created_at
  FROM upstream_accounts
 WHERE ($1 <= 0 OR channel_id = $1) ORDER BY id`, channelID)
	if err != nil {
		return nil, fmt.Errorf("列账号: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.ChannelID, &a.ExternalUserID,
			&a.BalanceGroupKey, &a.Status, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Key 是一把上游 Key。**响应中永不含明文**（FR-094）。
type Key struct {
	ID               int64      `json:"id"`
	AccountID        int64      `json:"account_id"`
	ChannelID        int64      `json:"channel_id"`
	SecretPrefix     string     `json:"secret_prefix"`
	ExternalRef      string     `json:"external_ref,omitempty"`
	ChannelGroupID   *int64     `json:"channel_group_id,omitempty"`
	RemainQuotaUSD   *float64   `json:"remain_quota_usd,omitempty"`
	UsedQuotaUSD     *float64   `json:"used_quota_usd,omitempty"`
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

// ListKeys 列出 Key（脱敏）。
func ListKeys(ctx context.Context, conn *pgx.Conn, channelID int64) ([]Key, error) {
	rows, err := conn.Query(ctx, `
SELECT k.id, k.account_id, a.channel_id, `+secretPrefixExpr+`,
       COALESCE(k.external_ref,''), k.channel_group_id,
       k.remain_quota_usd, k.used_quota_usd, k.rpm_limit, k.concurrency_limit,
       k.status, k.expired_time, k.quota_synced_at, k.created_at
  FROM upstream_keys k
  JOIN upstream_accounts a ON a.id = k.account_id
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
			&k.ExternalRef, &k.ChannelGroupID, &k.RemainQuotaUSD, &k.UsedQuotaUSD,
			&k.RPMLimit, &k.ConcurrencyLimit, &k.Status, &k.ExpiredTime,
			&k.QuotaSyncedAt, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
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

// RotateKey 轮换：写入新 secret，旧值被覆盖。
//
// 一期不做"宽限期双活"（09 里 rotate 标 M1/P2）：那需要同时持有两个 secret
// 并按时间切换，而 P1 没有请求路径、无从判断"旧 key 是否还在被用"。
// 这里的轮换语义是"换掉凭证"，明文只在响应里返回一次。
func RotateKey(ctx context.Context, conn *pgx.Conn, id int64, newSecret string) error {
	if newSecret == "" {
		return fmt.Errorf("新 secret 不可为空")
	}
	tag, err := conn.Exec(ctx, `
UPDATE upstream_keys SET secret=$2, status='active', updated_at=now() WHERE id=$1`,
		id, newSecret)
	if err != nil {
		return fmt.Errorf("轮换 Key %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: Key %d", ErrNotFound, id)
	}
	return nil
}

// ChannelGroup 是一个分组（含可用模型数）。
type ChannelGroup struct {
	ID             int64     `json:"id"`
	ChannelID      int64     `json:"channel_id"`
	GroupRef       string    `json:"group_ref"`
	RateMultiplier *float64  `json:"rate_multiplier,omitempty"`
	ModelCount     int       `json:"model_count"`
	DataSource     string    `json:"data_source"`
	FetchedAt      time.Time `json:"fetched_at"`
}

// ListChannelGroups 列出分组。
func ListChannelGroups(ctx context.Context, conn *pgx.Conn, channelID int64) ([]ChannelGroup, error) {
	rows, err := conn.Query(ctx, `
SELECT g.id, g.channel_id, g.group_ref, g.rate_multiplier,
       (SELECT count(*) FROM group_models m WHERE m.channel_group_id = g.id),
       g.data_source, g.fetched_at
  FROM channel_groups g
 WHERE ($1 <= 0 OR g.channel_id = $1)
 ORDER BY g.id`, channelID)
	if err != nil {
		return nil, fmt.Errorf("列分组: %w", err)
	}
	defer rows.Close()
	var out []ChannelGroup
	for rows.Next() {
		var g ChannelGroup
		if err := rows.Scan(&g.ID, &g.ChannelID, &g.GroupRef, &g.RateMultiplier,
			&g.ModelCount, &g.DataSource, &g.FetchedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListGroupModels 列出某分组的可用模型（FR-124："这把 Key 能用哪些模型"）。
func ListGroupModels(ctx context.Context, conn *pgx.Conn, groupID int64) ([]string, error) {
	rows, err := conn.Query(ctx,
		`SELECT model_name FROM group_models WHERE channel_group_id=$1 ORDER BY model_name`,
		groupID)
	if err != nil {
		return nil, fmt.Errorf("列分组 %d 的模型: %w", groupID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CatalogEntry 是模型目录的一行。
type CatalogEntry struct {
	ModelName   string   `json:"model_name"`
	InputPrice  *float64 `json:"input_price,omitempty"`
	OutputPrice *float64 `json:"output_price,omitempty"`
	// BillingUnit 是上面两个价格的口径，**必须与价格一同展示**。
	// nil = 上游未声明，消费方按未知处理（02 §1.3bis）。
	BillingUnit *string   `json:"billing_unit,omitempty"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	// Stale 表示疑似下架（last_seen_at 落后于最新一轮采集）。
	Stale bool `json:"stale"`
}

// ListCatalog 列出渠道模型目录。
//
// stale 判据（FR-126）：`last_seen_at` 落后于该渠道最新采集时刻超过
// catalogMissingRounds 轮。这里用"最新 last_seen_at"作为"最近一轮"的代理 ——
// 采集周期可配，用轮数比用时长更稳（02 §1.3bis）。
func ListCatalog(
	ctx context.Context, conn *pgx.Conn, channelID int64,
	staleOnly bool, missingRounds int, intervalHours int,
) ([]CatalogEntry, error) {
	// ⚠️ 排序**先按 billing_unit 再按价格**：两种口径的数值区间重叠
	// （实测按次 0.004~7 vs 倍率 0.01~175），跨口径按价格排会把
	// $7/次 的视频模型排在"倍率 175"之前，读者据此选型必然选错。
	// 分段后同段内可比，段间由 unit 列显式隔开（02 §1.3bis）。
	//
	// 落后阈值 = 轮数 × 采集周期
	lag := time.Duration(missingRounds) * time.Duration(intervalHours) * time.Hour
	if lag <= 0 {
		lag = 36 * time.Hour // 默认 3 轮 × 12h
	}
	rows, err := conn.Query(ctx, `
WITH newest AS (
  SELECT max(last_seen_at) AS t FROM channel_model_catalog WHERE channel_id = $1
)
SELECT c.model_name, c.input_price, c.output_price, c.billing_unit,
       c.first_seen_at, c.last_seen_at,
       (n.t IS NOT NULL AND c.last_seen_at < n.t - $2::interval) AS stale
  FROM channel_model_catalog c CROSS JOIN newest n
 WHERE c.channel_id = $1
   AND ($3 = false OR (n.t IS NOT NULL AND c.last_seen_at < n.t - $2::interval))
 ORDER BY c.billing_unit NULLS LAST, c.input_price NULLS LAST, c.model_name`,
		channelID, lag.String(), staleOnly)
	if err != nil {
		return nil, fmt.Errorf("列渠道 %d 目录: %w", channelID, err)
	}
	defer rows.Close()
	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		if err := rows.Scan(&e.ModelName, &e.InputPrice, &e.OutputPrice,
			&e.BillingUnit, &e.FirstSeenAt, &e.LastSeenAt, &e.Stale); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
