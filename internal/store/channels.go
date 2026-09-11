package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound 是通用的"记录不存在"。
var ErrNotFound = errors.New("store: 记录不存在")

// ErrDuplicate 是"唯一约束冲突"。
//
// 存在的理由：019 给 channels.base_url 加了唯一约束之后，"同地址已存在"从
// 应用层的 read-then-insert 判断变成库级冲突。调用方要能把它与"请求写错了"
// 分开 —— 前者该返 409（去改那一条），后者返 400（改自己的请求）。
var ErrDuplicate = errors.New("store: 已存在")

// TryChannelSyncLock prevents manual and periodic collection from hitting one channel concurrently.
func TryChannelSyncLock(ctx context.Context, conn DBTX, channelID int64) (bool, error) {
	var locked bool
	err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext('sync:' || $1::bigint::text))`, channelID).
		Scan(&locked)
	return locked, err
}

// UnlockChannelSync releases the session-level lock acquired by TryChannelSyncLock.
func UnlockChannelSync(ctx context.Context, conn DBTX, channelID int64) error {
	var unlocked bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_advisory_unlock(hashtext('sync:' || $1::bigint::text))`, channelID).
		Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked {
		return fmt.Errorf("渠道 %d 未持有采集锁", channelID)
	}
	return nil
}

// asDuplicate 把 PG 的唯一约束冲突（23505）翻成 ErrDuplicate。
// 其它错误原样返回。
func asDuplicate(err error, msg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrDuplicate, msg)
	}
	return err
}

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
		// 019 的唯一约束是"同地址只一个渠道"的**唯一**真实防线（read-then-insert
		// 防不住并发）。撞上去时要能被上层区分出来，否则并发导入只会得到一句
		// PG 原文，而正确的处置是"去用已有那条"。
		return 0, asDuplicate(fmt.Errorf("建渠道 %q: %w", c.Name, err),
			"已有渠道使用地址 "+c.BaseURL)
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
func UpdateChannel(ctx context.Context, conn DBTX, c Channel) error {
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
		// PATCH 也能改 base_url，故同样会撞 019 的唯一约束。
		return asDuplicate(fmt.Errorf("更新渠道 %d: %w", c.ID, err),
			"已有渠道使用地址 "+c.BaseURL)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 渠道 %d", ErrNotFound, c.ID)
	}
	return nil
}

// Account 是一个上游账号。
//
// 后四个字段是**读路径派生**的，不在 upstream_accounts 表上：
// 余额来自 balance_signals（采集器写，见 sink.go SaveAccount），
// Key 数来自 upstream_keys。它们只由 ListAccounts 填充，
// CreateAccount/UpdateAccount 不碰。
type Account struct {
	ID              int64      `json:"id"`
	ChannelID       int64      `json:"channel_id"`
	ExternalUserID  string     `json:"external_user_id,omitempty"`
	BalanceGroupKey string     `json:"balance_group_key,omitempty"`
	Status          string     `json:"status"`
	DisabledReason  string     `json:"disabled_reason,omitempty"`
	DisabledUntil   *time.Time `json:"disabled_until,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`

	// BalanceUSD 是最近一次确认的账号余额（归一美元，FR-018 一期 1:1）。
	//
	// **nil = 从未采到，不是 0**（FR-020/026）。采集器只在非 degraded 时写
	// balance_signals，所以"这个站型不给余额"和"余额确实是 0"在这里是
	// 两个不同的值，界面必须分开渲染 —— 把 nil 填成 0 会让一个查不到余额的
	// 账号看起来像已耗尽。
	BalanceUSD *float64 `json:"balance_usd,omitempty"`
	// BalanceState 是余额五态之一（normal/critical/unknown/exhausted/abnormal）。
	// 空串 = 没有任何余额信号行。
	BalanceState string `json:"balance_state,omitempty"`
	// BalanceConfirmedAt 是上面这个余额的确认时刻。
	// **必须与金额一同展示**：一个三天前的余额和五分钟前的余额，
	// 对"还能不能发付费请求"是完全不同的结论。
	BalanceConfirmedAt *time.Time `json:"balance_confirmed_at,omitempty"`

	// KeysTotal / KeysActive 是该账号下的 Key 数与其中 active 的个数。
	// 停用账号时要在确认框里写清影响面（"将影响 N 把 Key"），靠它。
	KeysTotal  int `json:"keys_total"`
	KeysActive int `json:"keys_active"`
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

// ListAccounts 列出某渠道的账号（channelID<=0 表示全部），并带上最近一次
// 确认的余额与该账号下的 Key 计数。
//
// 余额取 balance_signals 的**最新一行**而不是聚合：那张表是 append-only 的
// 采集流水，每轮成功采集追加一条。按 confirmed_at 降序取一条 = "最近一次
// 可信余额"（FR-026 的输入）。一次采集失败不会写行，所以上一次成功值仍在，
// 这正是 FR-026 要的"刷新失败不覆盖最后一次成功余额"—— 不需要额外代码。
//
// 用两个 LEFT JOIN LATERAL 而不是给每个账号发一条查询：账号页要列全部
// 渠道的账号（channelID<=0），N+1 会在 65 个渠道上变成上百次往返。
func ListAccounts(ctx context.Context, conn *pgx.Conn, channelID int64) ([]Account, error) {
	rows, err := conn.Query(ctx, `
SELECT a.id, a.channel_id, COALESCE(a.external_user_id,''),
       COALESCE(a.balance_group_key,''),
       a.status, COALESCE(a.disabled_reason,''), a.disabled_until, a.created_at,
       b.last_confirmed_balance, COALESCE(b.balance_state,''), b.confirmed_at,
       k.total, k.active
  FROM upstream_accounts a
  LEFT JOIN LATERAL (
    SELECT last_confirmed_balance, balance_state, confirmed_at
      FROM balance_signals
     WHERE account_id = a.id
     -- id 兜底：同一轮里 confirmed_at 可能相同（采集器用同一个 FetchedAt）
     ORDER BY confirmed_at DESC NULLS LAST, id DESC
     LIMIT 1
  ) b ON true
  LEFT JOIN LATERAL (
    SELECT count(*) AS total,
           count(*) FILTER (WHERE status = 'active') AS active
      FROM upstream_keys WHERE account_id = a.id
  ) k ON true
 WHERE ($1 <= 0 OR a.channel_id = $1) ORDER BY a.id`, channelID)
	if err != nil {
		return nil, fmt.Errorf("列账号: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.ChannelID, &a.ExternalUserID,
			&a.BalanceGroupKey, &a.Status, &a.DisabledReason,
			&a.DisabledUntil, &a.CreatedAt,
			&a.BalanceUSD, &a.BalanceState, &a.BalanceConfirmedAt,
			&a.KeysTotal, &a.KeysActive); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccountPatch 是账号局部更新；非 nil 字段即使为空也会被清除。
type AccountPatch struct {
	ID              int64
	ExternalUserID  *string
	BalanceGroupKey *string
	Status          *string
	DisabledReason  *string
	DisabledUntil   *time.Time
}

// UpdateAccount 更新账号可变字段。
func UpdateAccount(ctx context.Context, conn *pgx.Conn, a AccountPatch) error {
	tag, err := conn.Exec(ctx, `
UPDATE upstream_accounts
   SET external_user_id = CASE WHEN $2::text IS NULL THEN external_user_id ELSE NULLIF($2,'') END,
       balance_group_key = CASE WHEN $3::text IS NULL THEN balance_group_key ELSE NULLIF($3,'') END,
       status = CASE WHEN $4::text IS NULL THEN status ELSE NULLIF($4,'') END,
       disabled_reason = CASE $4
                           WHEN 'disabled' THEN NULLIF($5,'')
                           WHEN 'active'   THEN NULL
                           ELSE disabled_reason END,
       disabled_until  = CASE $4
                           WHEN 'disabled' THEN $6
                           WHEN 'active'   THEN NULL
                           ELSE disabled_until END
 WHERE id = $1`, a.ID, a.ExternalUserID, a.BalanceGroupKey, a.Status,
		a.DisabledReason, a.DisabledUntil)
	if err != nil {
		return fmt.Errorf("更新账号 %d: %w", a.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 账号 %d", ErrNotFound, a.ID)
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
	// Stale 表示连续缺席轮次已达到配置阈值。
	Stale bool `json:"stale"`
}

// ListCatalog 列出渠道模型目录。
//
// stale 判据（FR-126）：channel.catalog_sync_seq - last_seen_seq 达到
// catalogMissingRounds。只计算可靠成功轮次，不受任务延迟、手动刷新或改周期影响。
func ListCatalog(
	ctx context.Context, conn *pgx.Conn, channelID int64,
	staleOnly bool, missingRounds int,
) ([]CatalogEntry, error) {
	// ⚠️ 排序**先按 billing_unit 再按价格**：两种口径的数值区间重叠
	// （实测按次 0.004~7 vs 倍率 0.01~175），跨口径按价格排会把
	// $7/次 的视频模型排在"倍率 175"之前，读者据此选型必然选错。
	// 分段后同段内可比，段间由 unit 列显式隔开（02 §1.3bis）。
	//
	if missingRounds <= 0 {
		missingRounds = 3
	}
	rows, err := conn.Query(ctx, `
SELECT c.model_name, c.input_price, c.output_price, c.billing_unit,
       c.first_seen_at, c.last_seen_at,
       (ch.catalog_sync_seq - c.last_seen_seq >= $2::bigint) AS stale
  FROM channel_model_catalog c JOIN channels ch ON ch.id = c.channel_id
 WHERE c.channel_id = $1
   AND ($3 = false OR ch.catalog_sync_seq - c.last_seen_seq >= $2::bigint)
 ORDER BY c.billing_unit NULLS LAST, c.input_price NULLS LAST, c.model_name`,
		channelID, missingRounds, staleOnly)
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
