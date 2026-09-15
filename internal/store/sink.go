package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// CollectorSink 把采集结果落到 PG，实现 collector.Sink。
//
// 每个方法自己管事务边界（09 §5.0bis：逐项独立提交）—— sink 不持有跨项事务，
// 因为"价格采到了但目录超时"没有理由把价格也回滚。
type CollectorSink struct {
	Pool *Pool
	// ValidFor 是人工录入数据的有效期（FR-011：7 天）。自动采集不设。
	ValidFor time.Duration
}

// NewCollectorSink 构造 sink。
func NewCollectorSink(p *Pool) *CollectorSink {
	return &CollectorSink{Pool: p, ValidFor: collector.ManualValidity}
}

func (s *CollectorSink) acquire(ctx context.Context) (*pgxConn, error) {
	c, rel, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxConn{Conn: c, release: rel}, nil
}

// SaveAccount 写余额信号 + 账号快照。
func (s *CollectorSink) SaveAccount(
	ctx context.Context, channelID, accountID int64, a collector.Account,
) error {
	c, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	tx, err := c.Conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启账号事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 余额信号（FR-020/024/026）。
	// ⚠️ **只写 last_confirmed_balance / confirmed_at / balance_state**，
	// 不写 known_consumption_since 与 conservative_floor —— 那两列归 P3 的
	// 余额下限 worker（04 §4 列级写入归属）：采集器后写会把保守值抹回标称值，
	// 余额只剩 $2、在途 $5 时 selector 会看到正数下限继续放行付费请求。
	if !a.Meta.Degraded {
		if _, err := tx.Exec(ctx, `
INSERT INTO balance_signals (account_id, balance_state, last_confirmed_balance,
                             confirmed_at, updated_at)
VALUES ($1,'normal',$2,$3,now())`,
			accountID, a.BalanceUSD, a.Meta.FetchedAt); err != nil {
			return fmt.Errorf("写余额信号: %w", err)
		}
	}

	// 账号的默认分组。空串不写（NULLIF）—— NULL 是"没采到"，
	// 而 '' 会被下面那些 JOIN 当成一个叫空串的分组去匹配。
	if _, err := tx.Exec(ctx,
		`UPDATE upstream_accounts SET account_group = NULLIF($2,'') WHERE id=$1`,
		accountID, a.GroupRef); err != nil {
		return fmt.Errorf("写账号默认分组: %w", err)
	}

	// 快照 payload：**未采到的字段省略该键**（02 §7.1）
	payload := map[string]any{}
	if !a.Meta.Degraded {
		payload["balance_usd"] = a.BalanceUSD
		payload["used_usd"] = a.UsedUSD
	}
	if a.GroupRef != "" {
		payload["account_group"] = a.GroupRef
	}
	if a.UserID != "" {
		payload["external_user_id"] = a.UserID
	}
	if err := InsertSnapshot(ctx, tx, SnapshotRow{
		ChannelID: channelID, ScopeType: "account", ScopeID: fmt.Sprint(accountID),
		Payload: payload, DataSource: "auto_collect", FetchedAt: a.Meta.FetchedAt,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交账号事务: %w", err)
	}
	return nil
}

// SaveGroups 写分组与分组可用模型（单事务，02 §1.3bis 全量替换）。
func (s *CollectorSink) SaveGroups(
	ctx context.Context, channelID int64, gs []collector.Group,
) (int, error) {
	if len(gs) == 0 {
		return 0, nil
	}
	c, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	tx, err := c.Conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开启分组事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows := make([]GroupRow, 0, len(gs))
	for _, g := range gs {
		r := GroupRow{
			ChannelID: channelID, GroupRef: g.GroupRef,
			AvailableModels:   g.AvailableModels,
			RateDynamic:       g.RateDynamic,
			DynamicCandidates: g.DynamicCandidates,
			PreserveModels:    g.Meta.Partial || slices.Contains(g.Meta.MissingFields, "available_models"),
			DataSource:        "auto_collect", FetchedAt: g.Meta.FetchedAt,
		}
		// 倍率为 0 时不写：0 倍率语义上是"免费"，而采不到应是"未知"。
		// 动态倍率的组照样写它上游给的那个数（那是事实），但 RateDynamic 已
		// 标出来，消费方必须先看那一位再决定要不要拿它算钱。
		if g.RateMultiplier > 0 {
			v := g.RateMultiplier
			r.RateMultiplier = &v
		}
		rows = append(rows, r)
	}
	n, err := upsertGroups(ctx, tx, rows)
	if err != nil {
		return n, err
	}
	for _, g := range gs {
		payload := map[string]any{}
		if g.RateMultiplier > 0 {
			payload["rate_multiplier"] = g.RateMultiplier
		}
		if len(payload) == 0 {
			continue
		}
		if err := InsertSnapshot(ctx, tx, SnapshotRow{
			ChannelID: channelID, ScopeType: "group", ScopeID: g.GroupRef,
			Payload: payload, DataSource: "auto_collect", FetchedAt: g.Meta.FetchedAt,
		}); err != nil {
			return n, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交分组事务: %w", err)
	}
	return n, nil
}

// SaveKey 写一把 Key 的用量（每把一个事务，09 §5.0bis）。
//
// **只 UPDATE 不 INSERT**：库中无该 Key 时返回 collector.ErrKeyNotRegistered
// （02 §1.3bis：secret 是明文凭证、上游只回掩码，凭空插一行会让它永远不可用）。
func (s *CollectorSink) SaveKey(
	ctx context.Context, channelID, accountID int64, k collector.Key,
) error {
	c, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	tx, err := c.Conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启 Key 事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	idx, err := KeyRefIndex(ctx, tx, accountID)
	if err != nil {
		return err
	}
	keyID, ok := idx[k.KeyRef]
	if !ok {
		if err := InsertSnapshot(ctx, tx, SnapshotRow{
			ChannelID: channelID, ScopeType: "key", ScopeID: k.KeyRef,
			Payload:    map[string]any{"unregistered": true},
			DataSource: "auto_collect", FetchedAt: k.Meta.FetchedAt,
		}); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("提交未登记 Key 快照: %w", err)
		}
		return fmt.Errorf("%w: external_ref=%s", collector.ErrKeyNotRegistered, k.KeyRef)
	}

	row := KeyUsageRow{
		KeyID: keyID, SyncedAt: k.Meta.FetchedAt, ExpiredAt: k.ExpiredAt,
		RemainQuotaUSD: k.RemainQuotaUSD, UsedQuotaUSD: k.UsedQuotaUSD,
		Unlimited: k.Unlimited,
	}
	if k.RateLimit.RPM > 0 {
		v := k.RateLimit.RPM
		row.RPMLimit = &v
	}
	if k.RateLimit.Concurrency > 0 {
		v := k.RateLimit.Concurrency
		row.ConcurrencyLimit = &v
	}
	if k.GroupRef != "" {
		if gid, found, err := GroupIDByRef(ctx, tx, channelID, k.GroupRef); err != nil {
			return err
		} else if found {
			row.ChannelGroupID = &gid
		}
	}
	if err := UpdateKeyUsage(ctx, tx, row); err != nil {
		return err
	}

	// 用量历史进快照（FR-125）——**省略未采到的键**（02 §7.1）
	payload := map[string]any{}
	if k.RemainQuotaUSD != nil {
		payload["remain_quota_usd"] = *k.RemainQuotaUSD
	}
	if k.UsedQuotaUSD != nil {
		payload["used_quota_usd"] = *k.UsedQuotaUSD
	}
	if k.RequestCount > 0 {
		payload["request_count"] = k.RequestCount
	}
	if len(k.RateLimit.Windows) > 0 {
		w := map[string]any{}
		for name, u := range k.RateLimit.Windows {
			w[name] = map[string]any{
				"limit_usd": u.LimitUSD, "usage_usd": u.UsageUSD,
				"window_start": u.WindowStart,
			}
		}
		payload["window_usage"] = w
	}
	if k.RateLimit.Concurrency > 0 {
		payload["current_concurrency"] = k.RateLimit.Concurrency
	}
	if len(payload) == 0 {
		return tx.Commit(ctx)
	}
	if err := InsertSnapshot(ctx, tx, SnapshotRow{
		ChannelID: channelID, ScopeType: "key", ScopeID: fmt.Sprint(keyID),
		Payload: payload, DataSource: "auto_collect", FetchedAt: k.Meta.FetchedAt,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SavePricing 刷新已有目录价格，并为已登记模型追加价格版本。
func (s *CollectorSink) SavePricing(
	ctx context.Context, channelID int64, p collector.Pricing,
) (collector.PricingWriteResult, error) {
	if len(p.Models) == 0 {
		// degraded 站型只有分组倍率、无逐模型价格 —— 不是错误
		return collector.PricingWriteResult{}, nil
	}
	c, err := s.acquire(ctx)
	if err != nil {
		return collector.PricingWriteResult{}, err
	}
	defer c.Close()
	tx, err := c.Conn.Begin(ctx)
	if err != nil {
		return collector.PricingWriteResult{}, fmt.Errorf("开启价格事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var result collector.PricingWriteResult
	for _, mp := range p.Models {
		tag, err := tx.Exec(ctx, `
UPDATE channel_model_catalog
   SET input_price=$3, output_price=$4, billing_unit=NULLIF($5,'')
 WHERE channel_id=$1 AND model_name=$2`,
			channelID, mp.ModelName, nullFloat(mp.InputPrice), nullFloat(mp.OutputPrice),
			mp.BillingUnit)
		if err != nil {
			return collector.PricingWriteResult{}, fmt.Errorf("刷新模型 %s 目录价格: %w", mp.ModelName, err)
		}
		result.CatalogUpdates += int(tag.RowsAffected())

		payload := map[string]any{"input_price": mp.InputPrice, "output_price": mp.OutputPrice}
		if mp.CachePrice != 0 {
			payload["cache_price"] = mp.CachePrice
		}
		if mp.BillingUnit != "" {
			payload["billing_unit"] = mp.BillingUnit
		}
		if err := InsertSnapshot(ctx, tx, SnapshotRow{
			ChannelID: channelID, ScopeType: "pricing", ScopeID: mp.ModelName,
			Payload: payload, DataSource: "auto_collect", FetchedAt: p.Meta.FetchedAt,
		}); err != nil {
			return collector.PricingWriteResult{}, fmt.Errorf("写模型 %s 价格快照: %w", mp.ModelName, err)
		}
		result.Snapshots++

		// 价格版本按 (channel, model) 作用域，需要 models.id。
		// **不自动创建 models 行**：那张表要求 token 上界必填（02 §2bis
		// 预留上界的硬前置），而目录阶段拿不到。故只为**已登记**的模型写价格版本。
		var modelID int64
		err = tx.QueryRow(ctx,
			`SELECT id FROM models WHERE canonical_name=$1`, mp.ModelName).Scan(&modelID)
		if errors.Is(err, pgx.ErrNoRows) {
			result.UnregisteredModels++
			continue
		}
		if err != nil {
			return collector.PricingWriteResult{}, fmt.Errorf("查模型 %s: %w", mp.ModelName, err)
		}
		// billing_unit 用 NULLIF 落空，与 SaveCatalog 同一条规则（02 §1.3bis）：
		// 缺失表示上游未声明，补 'per_1m_token' 会把它伪装成"已知按 token 计价"，
		// 而成本公式按该口径要除以 1,000,000。
		//
		// ⚠️ 这里原本写的是 `COALESCE(NULLIF($7,''),'per_1m_token')` —— **不是
		//    实现者的选择，是 006 的列定义（NOT NULL DEFAULT）逼出来的**：那张表
		//    不接受 NULL，于是同一份写入代码在目录表上遵守规则、在权威价格表上
		//    违反它。018 把列改成「可空 + CHECK」后这句才写得出来。
		if _, err := tx.Exec(ctx, `
INSERT INTO price_versions (id, channel_id, model_id, input_price, output_price,
                            cache_price, billing_unit, currency, data_source,
                            queried_at, effective_at)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),'USD',
        'auto_collect',$8,$8)`,
			NewUUIDv7(), channelID, modelID, mp.InputPrice, mp.OutputPrice,
			nullFloat(mp.CachePrice), mp.BillingUnit, p.Meta.FetchedAt); err != nil {
			return collector.PricingWriteResult{}, fmt.Errorf("写模型 %s 价格版本: %w", mp.ModelName, err)
		}
		result.PriceVersions++
	}
	if err := tx.Commit(ctx); err != nil {
		return collector.PricingWriteResult{}, fmt.Errorf("提交价格事务: %w", err)
	}
	return result, nil
}

// SaveCatalog 写模型目录（upsert，**first_seen_at 不覆盖**，02 §1.3bis）。
func (s *CollectorSink) SaveCatalog(
	ctx context.Context, channelID int64, cs []collector.CatalogModel,
) (int, error) {
	if len(cs) == 0 {
		return 0, nil
	}
	c, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()

	tx, err := c.Conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开启目录事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var syncSeq int64
	if catalogPresenceReliable(cs) {
		err = tx.QueryRow(ctx, `
UPDATE channels SET catalog_sync_seq = catalog_sync_seq + 1
 WHERE id=$1 RETURNING catalog_sync_seq`, channelID).Scan(&syncSeq)
	} else {
		err = tx.QueryRow(ctx,
			`SELECT catalog_sync_seq FROM channels WHERE id=$1 FOR UPDATE`, channelID).
			Scan(&syncSeq)
	}
	if err != nil {
		return 0, fmt.Errorf("更新渠道 %d 目录轮次: %w", channelID, err)
	}

	var n int
	for _, m := range cs {
		if m.ModelName == "" {
			continue
		}
		// billing_unit 用 NULLIF 落空而非补默认值：degraded 站型无价也无口径，
		// 补 'per_1m_token' 会把"未声明"伪装成"已知按 token 计价"（02 §1.3bis）。
		if _, err := tx.Exec(ctx, `
INSERT INTO channel_model_catalog (channel_id, model_name, input_price, output_price,
                                   billing_unit, vendor_name, vendor_icon, endpoint_types,
                                   first_seen_at, last_seen_at, last_seen_seq)
VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),$8,$9,$9,$10)
ON CONFLICT (channel_id, model_name) DO UPDATE
   SET input_price  = EXCLUDED.input_price,
       output_price = EXCLUDED.output_price,
       billing_unit = EXCLUDED.billing_unit,
       vendor_name  = EXCLUDED.vendor_name,
       vendor_icon  = EXCLUDED.vendor_icon,
       endpoint_types = EXCLUDED.endpoint_types,
       last_seen_at = EXCLUDED.last_seen_at,
       last_seen_seq = EXCLUDED.last_seen_seq`,
			channelID, m.ModelName, nullFloat(m.InputPrice), nullFloat(m.OutputPrice),
			m.BillingUnit, m.VendorName, m.VendorIcon, nullStrings(m.EndpointTypes),
			m.Meta.FetchedAt, syncSeq); err != nil {
			return n, fmt.Errorf("写目录条目 %s: %w", m.ModelName, err)
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交目录事务: %w", err)
	}
	return n, nil
}

func catalogPresenceReliable(models []collector.CatalogModel) bool {
	validModels := 0
	for _, model := range models {
		if model.Meta.Partial {
			return false
		}
		if model.ModelName != "" {
			validModels++
		}
		for _, field := range model.Meta.MissingFields {
			if field == "available_models" || field == "model_catalog" {
				return false
			}
		}
	}
	return validModels > 0
}

// nullFloat 把 0 转成 NULL：目录与价格的 0 值语义上是"未采到"而非"免费"。
func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// nullStrings 把空切片落成 NULL 而不是 '{}'。
//
// 两者在库里不是一回事，而这一列的语义恰好卡在这个区别上：NULL = 上游没声明
// 支持哪些端点（老版本站点就没这个字段），'{}' = 上游明说"一个都不支持"。
// 把前者写成后者，界面上会出现一批"不支持任何端点"的模型，而它们其实好好的。
func nullStrings(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s
}

var _ collector.Sink = (*CollectorSink)(nil)
