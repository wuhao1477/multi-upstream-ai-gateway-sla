package store

import (
	"context"
	"fmt"
	"time"

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
	ctx context.Context, channelID int64, a collector.Account,
) error {
	c, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	// 找到该渠道的账号行（P1 一渠道通常一账号；多账号时按 external_user_id 匹配）
	accounts, err := ListAccounts(ctx, c.Conn, channelID)
	if err != nil {
		return err
	}
	var accountID int64
	for _, acc := range accounts {
		if a.UserID != "" && acc.ExternalUserID == a.UserID {
			accountID = acc.ID
			break
		}
	}
	if accountID == 0 && len(accounts) == 1 {
		// 只有一个账号时直接用它 —— 上游的 UserID 可能与登记值格式不同
		accountID = accounts[0].ID
	}

	// 余额信号（FR-020/024/026）。
	// ⚠️ **只写 last_confirmed_balance / confirmed_at / balance_state**，
	// 不写 known_consumption_since 与 conservative_floor —— 那两列归 P3 的
	// 余额下限 worker（04 §4 列级写入归属）：采集器后写会把保守值抹回标称值，
	// 余额只剩 $2、在途 $5 时 selector 会看到正数下限继续放行付费请求。
	if accountID > 0 && !a.Meta.Degraded {
		if _, err := c.Conn.Exec(ctx, `
INSERT INTO balance_signals (account_id, balance_state, last_confirmed_balance,
                             confirmed_at, updated_at)
VALUES ($1,'normal',$2,$3,now())`,
			accountID, a.BalanceUSD, a.Meta.FetchedAt); err != nil {
			return fmt.Errorf("写余额信号: %w", err)
		}
	}

	// 快照 payload：**未采到的字段省略该键**（02 §7.1）
	payload := map[string]any{}
	if !a.Meta.Degraded {
		payload["balance_usd"] = a.BalanceUSD
		payload["used_usd"] = a.UsedUSD
	}
	if a.UserID != "" {
		payload["external_user_id"] = a.UserID
	}
	scopeID := a.UserID
	if scopeID == "" && accountID > 0 {
		scopeID = fmt.Sprint(accountID)
	}
	return InsertSnapshot(ctx, c.Conn, SnapshotRow{
		ChannelID: channelID, ScopeType: "account", ScopeID: scopeID,
		Payload: payload, DataSource: "auto_collect", FetchedAt: a.Meta.FetchedAt,
	})
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

	rows := make([]GroupRow, 0, len(gs))
	for _, g := range gs {
		r := GroupRow{
			ChannelID: channelID, GroupRef: g.GroupRef,
			AvailableModels: g.AvailableModels,
			DataSource:      "auto_collect", FetchedAt: g.Meta.FetchedAt,
		}
		// 倍率为 0 时不写：0 倍率语义上是"免费"，而采不到应是"未知"
		if g.RateMultiplier > 0 {
			v := g.RateMultiplier
			r.RateMultiplier = &v
		}
		rows = append(rows, r)
	}
	n, err := UpsertGroups(ctx, c.Conn, rows)
	if err != nil {
		return n, err
	}

	// 不落结构化列的字段进快照（ISSUE-005 §3.1：P1 无消费者，需要时回填）
	for _, g := range gs {
		payload := map[string]any{}
		if g.RateMultiplier > 0 {
			payload["rate_multiplier"] = g.RateMultiplier
		}
		if g.PeakEnabled {
			payload["peak_enabled"] = true
			if g.PeakMultiplier > 0 {
				payload["peak_rate_multiplier"] = g.PeakMultiplier
			}
			if g.PeakStart != "" {
				payload["peak_start"] = g.PeakStart
			}
			if g.PeakEnd != "" {
				payload["peak_end"] = g.PeakEnd
			}
		}
		if g.IsExclusive {
			payload["is_exclusive"] = true
		}
		if g.Platform != "" {
			payload["platform"] = g.Platform
		}
		if g.SubscriptionType != "" {
			payload["subscription_type"] = g.SubscriptionType
		}
		if g.RPMLimit > 0 {
			payload["rpm_limit"] = g.RPMLimit
		}
		if len(payload) == 0 {
			continue
		}
		if err := InsertSnapshot(ctx, c.Conn, SnapshotRow{
			ChannelID: channelID, ScopeType: "group", ScopeID: g.GroupRef,
			Payload: payload, DataSource: "auto_collect", FetchedAt: g.Meta.FetchedAt,
		}); err != nil {
			return n, err
		}
	}
	return n, nil
}

// SaveKey 写一把 Key 的用量（每把一个事务，09 §5.0bis）。
//
// **只 UPDATE 不 INSERT**：库中无该 Key 时返回 collector.ErrKeyNotRegistered
// （02 §1.3bis：secret 是明文凭证、上游只回掩码，凭空插一行会让它永远不可用）。
func (s *CollectorSink) SaveKey(
	ctx context.Context, channelID int64, k collector.Key,
) error {
	c, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	idx, err := KeyRefIndex(ctx, c.Conn, channelID)
	if err != nil {
		return err
	}
	keyID, ok := idx[k.KeyRef]
	if !ok {
		return fmt.Errorf("%w: external_ref=%s", collector.ErrKeyNotRegistered, k.KeyRef)
	}

	row := KeyUsageRow{KeyID: keyID, SyncedAt: k.Meta.FetchedAt, ExpiredAt: k.ExpiredAt}
	if k.RemainQuotaUSD > 0 || k.UsedQuotaUSD > 0 {
		rq, uq := k.RemainQuotaUSD, k.UsedQuotaUSD
		row.RemainQuotaUSD, row.UsedQuotaUSD = &rq, &uq
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
		if gid, found, err := GroupIDByRef(ctx, c.Conn, channelID, k.GroupRef); err != nil {
			return err
		} else if found {
			row.ChannelGroupID = &gid
		}
	}
	if err := UpdateKeyUsage(ctx, c.Conn, row); err != nil {
		return err
	}

	// 用量历史进快照（FR-125）——**省略未采到的键**（02 §7.1）
	payload := map[string]any{}
	if k.RemainQuotaUSD > 0 || k.UsedQuotaUSD > 0 {
		payload["remain_quota_usd"] = k.RemainQuotaUSD
		payload["used_quota_usd"] = k.UsedQuotaUSD
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
		return nil
	}
	return InsertSnapshot(ctx, c.Conn, SnapshotRow{
		ChannelID: channelID, ScopeType: "key", ScopeID: fmt.Sprint(keyID),
		Payload: payload, DataSource: "auto_collect", FetchedAt: k.Meta.FetchedAt,
	})
}

// SavePricing 写价格版本（append-only，FR-012 不可覆盖）。
func (s *CollectorSink) SavePricing(
	ctx context.Context, channelID int64, p collector.Pricing,
) (int, error) {
	if len(p.Models) == 0 {
		// degraded 站型只有分组倍率、无逐模型价格 —— 不是错误
		return 0, nil
	}
	c, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()

	var n int
	for _, mp := range p.Models {
		// 价格版本按 (channel, model) 作用域，需要 models.id。
		// **不自动创建 models 行**：那张表要求 token 上界必填（02 §2bis
		// 预留上界的硬前置），而目录阶段拿不到。故只为**已登记**的模型写价格版本。
		var modelID int64
		err := c.Conn.QueryRow(ctx,
			`SELECT id FROM models WHERE canonical_name=$1`, mp.ModelName).Scan(&modelID)
		if err != nil {
			continue // 未登记为可路由模型 → 跳过，其价格已在目录里
		}
		if _, err := c.Conn.Exec(ctx, `
INSERT INTO price_versions (id, channel_id, model_id, input_price, output_price,
                            cache_price, billing_unit, currency, data_source,
                            queried_at, effective_at)
VALUES ($1,$2,$3,$4,$5,$6,COALESCE(NULLIF($7,''),'per_1m_token'),'USD',
        'auto_collect',$8,$8)`,
			NewUUIDv7(), channelID, modelID, mp.InputPrice, mp.OutputPrice,
			nullFloat(mp.CachePrice), mp.BillingUnit, p.Meta.FetchedAt); err != nil {
			return n, fmt.Errorf("写模型 %s 价格版本: %w", mp.ModelName, err)
		}
		n++
	}
	return n, nil
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

	var n int
	for _, m := range cs {
		if m.ModelName == "" {
			continue
		}
		// billing_unit 用 NULLIF 落空而非补默认值：degraded 站型无价也无口径，
		// 补 'per_1m_token' 会把"未声明"伪装成"已知按 token 计价"（02 §1.3bis）。
		if _, err := tx.Exec(ctx, `
INSERT INTO channel_model_catalog (channel_id, model_name, input_price, output_price,
                                   billing_unit, first_seen_at, last_seen_at)
VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$6)
ON CONFLICT (channel_id, model_name) DO UPDATE
   SET input_price  = EXCLUDED.input_price,
       output_price = EXCLUDED.output_price,
       billing_unit = EXCLUDED.billing_unit,
       last_seen_at = EXCLUDED.last_seen_at`,
			channelID, m.ModelName, nullFloat(m.InputPrice), nullFloat(m.OutputPrice),
			m.BillingUnit, m.Meta.FetchedAt); err != nil {
			return n, fmt.Errorf("写目录条目 %s: %w", m.ModelName, err)
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交目录事务: %w", err)
	}
	return n, nil
}

// nullFloat 把 0 转成 NULL：目录与价格的 0 值语义上是"未采到"而非"免费"。
func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

var _ collector.Sink = (*CollectorSink)(nil)
