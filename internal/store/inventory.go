package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Inventory 是渠道资产总览（FR-128/129）。
type Inventory struct {
	Accounts      int
	Keys          int
	Groups        int
	CatalogModels int
	QuotaTotalUSD float64
	LastSyncedAt  *time.Time
	Anomalies     []Anomaly
}

// Anomaly 是一类异常项。
type Anomaly struct {
	Kind  string
	Count int
	Hint  string
	// Items 是可下钻的具体对象（截断到前若干个，避免响应膨胀）。
	Items []string
}

// anomalyItemCap 限制每类异常回传的明细条数。
// 目的是让运维知道"是哪些"，而不是把整张表倒出来。
const anomalyItemCap = 20

// BuildInventory 汇总资产与**六类异常项**（09 §5.0）。
//
// 六类逐一给计数与可下钻列表，**不合并成一个总数** ——
// 否则运维看到"异常 7"却不知道该修什么。
func BuildInventory(
	ctx context.Context, conn *pgx.Conn, ch Channel, catalogMissingRounds int,
) (*Inventory, error) {
	inv := &Inventory{}

	if err := conn.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM upstream_accounts WHERE channel_id=$1),
  (SELECT count(*) FROM upstream_keys k JOIN upstream_accounts a ON a.id=k.account_id
    WHERE a.channel_id=$1),
  (SELECT count(*) FROM channel_groups WHERE channel_id=$1),
  (SELECT count(*) FROM channel_model_catalog WHERE channel_id=$1),
  (SELECT COALESCE(sum(k.remain_quota_usd),0) FROM upstream_keys k
     JOIN upstream_accounts a ON a.id=k.account_id WHERE a.channel_id=$1),
  (SELECT max(k.quota_synced_at) FROM upstream_keys k
     JOIN upstream_accounts a ON a.id=k.account_id WHERE a.channel_id=$1)
`, ch.ID).Scan(&inv.Accounts, &inv.Keys, &inv.Groups, &inv.CatalogModels,
		&inv.QuotaTotalUSD, &inv.LastSyncedAt); err != nil {
		return nil, fmt.Errorf("汇总渠道 %d 资产: %w", ch.ID, err)
	}

	add := func(kind, hint string, items []string) {
		if len(items) == 0 {
			return
		}
		a := Anomaly{Kind: kind, Count: len(items), Hint: hint}
		if len(items) > anomalyItemCap {
			a.Items = items[:anomalyItemCap]
		} else {
			a.Items = items
		}
		inv.Anomalies = append(inv.Anomalies, a)
	}

	// ① 数据陈旧：quota_synced_at 超采集周期的 2 倍。
	// 用 2 倍而非 1 倍：正好卡在周期边界的抖动不该报异常。
	stale, err := queryStrings(ctx, conn, `
SELECT 'key#' || k.id
  FROM upstream_keys k JOIN upstream_accounts a ON a.id=k.account_id
 WHERE a.channel_id=$1
   AND (k.quota_synced_at IS NULL OR k.quota_synced_at < now() - interval '1 hour')
 ORDER BY k.id`, ch.ID)
	if err != nil {
		return nil, err
	}
	add("stale_data", "Key 用量数据陈旧或从未采集，跑一次 sync 刷新", stale)

	// ② Degraded 结果的待人工补录（04 §3.4bis / FR-011）：
	// 目录里价格为空的模型即"采不到单价"，需人工录入。
	missing, err := queryStrings(ctx, conn, `
SELECT model_name FROM channel_model_catalog
 WHERE channel_id=$1 AND (input_price IS NULL OR output_price IS NULL)
 ORDER BY model_name`, ch.ID)
	if err != nil {
		return nil, err
	}
	add("degraded_missing_fields",
		"该站型未提供逐模型单价（degraded），需人工录入（FR-011）", missing)

	// ③ 上游存在但库中未登记的 Key（02 §1.3bis）。
	// 判据：采集写过快照（scope_type='key'）但库中无对应 external_ref。
	unreg, err := queryStrings(ctx, conn, `
SELECT DISTINCT s.scope_id
  FROM collector_snapshots s
 WHERE s.channel_id=$1 AND s.scope_type='key'
	AND s.payload @> '{"unregistered":true}'::jsonb
   AND NOT EXISTS (
	 SELECT 1 FROM upstream_keys k JOIN upstream_accounts a ON a.id=k.account_id
	  WHERE a.channel_id=$1 AND k.external_ref = s.scope_id)
 ORDER BY s.scope_id`, ch.ID)
	if err != nil {
		return nil, err
	}
	add("unregistered_key",
		"上游存在但库中未登记的 Key，需经 /admin/keys 补登记（采集不自动建 Key）", unreg)

	// ④ 疑似下架模型（FR-126）：复用目录查询的真实连续轮次判据。
	staleCatalog, err := ListCatalog(ctx, conn, ch.ID, true, catalogMissingRounds)
	if err != nil {
		return nil, err
	}
	delisted := make([]string, 0, len(staleCatalog))
	for _, model := range staleCatalog {
		delisted = append(delisted, model.ModelName)
	}
	add("delisted_model", "模型连续多轮未出现，疑似上游已下架", delisted)

	// ⑤ 凭证状态非 valid（04 §5 状态机）。
	badCred, err := queryStrings(ctx, conn, `
SELECT cred_type || '(' || status || ')'
  FROM collector_credentials WHERE channel_id=$1 AND status <> 'valid'
 ORDER BY id`, ch.ID)
	if err != nil {
		return nil, err
	}
	add("credential_invalid", "采集凭证已失效或需重登，采集会失败（04 §5）", badCred)

	// ⑤bis **完全没有采集凭证** —— 浏览器验收时发现的真缺口：
	// 一个刚建的渠道没凭证、没 Key，六类异常一个都不报（"✓ 无异常项"），
	// 而它其实**根本采不了**。这恰恰是最常见的"为什么不工作"，
	// 却是唯一没被 inventory 覆盖的情形 —— 前五类都假定"已经采过一轮"。
	var credCount int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM collector_credentials WHERE channel_id=$1`,
		ch.ID).Scan(&credCount); err != nil {
		return nil, fmt.Errorf("查渠道 %d 凭证数: %w", ch.ID, err)
	}
	if credCount == 0 {
		add("credential_missing",
			"尚未登记采集凭证，无法采集：请先登记凭证（04 §5 按站型给不同字段）",
			[]string{"channel#" + fmt.Sprint(ch.ID)})
	}

	// ⑤ter 站型未知 → 无适配器可用，采集必然失败（04 §7）。
	if ch.SiteFamily == "unknown" || ch.SiteFamily == "" {
		add("site_family_unknown",
			"站型未识别，没有对应适配器：需重新探测，或按 04 §7 新建专属适配器",
			[]string{"channel#" + fmt.Sprint(ch.ID)})
	}

	// ⑤quater 已有凭证但从未采集过 —— 区别于"数据陈旧"：
	// 后者说明采过但旧了，前者说明一次都没成功，处置动作不同。
	if credCount > 0 && inv.Keys == 0 && inv.Groups == 0 && inv.CatalogModels == 0 {
		add("never_collected",
			"已有凭证但从未采集成功：点“立即采集”并查看逐项结果定位失败原因",
			[]string{"channel#" + fmt.Sprint(ch.ID)})
	}

	// ⑥ Key 状态非 active 或已过期。
	badKey, err := queryStrings(ctx, conn, `
SELECT 'key#' || k.id || '(' || k.status || ')'
  FROM upstream_keys k JOIN upstream_accounts a ON a.id=k.account_id
 WHERE a.channel_id=$1
   AND (k.status <> 'active'
        OR (k.expired_time IS NOT NULL AND k.expired_time < now()))
 ORDER BY k.id`, ch.ID)
	if err != nil {
		return nil, err
	}
	add("key_unusable", "Key 已停用或已过期，不能承接请求（FR-031）", badKey)

	if inv.Anomalies == nil {
		inv.Anomalies = []Anomaly{}
	}
	return inv, nil
}

func queryStrings(ctx context.Context, conn *pgx.Conn, sql string, args ...any) ([]string, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("查询异常项: %w", err)
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
