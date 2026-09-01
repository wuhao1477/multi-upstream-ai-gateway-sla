package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── 分组与分组可用模型（FR-123/124，02 §1.3bis 写入语义）──

// GroupRow 是待写入的一个分组。
type GroupRow struct {
	ChannelID       int64
	GroupRef        string
	RateMultiplier  *float64 // 可空：采不到倍率时不写 0（0 倍率语义上是"免费"）
	AvailableModels []string
	// PreserveModels keeps the last complete list when this response omitted the model field.
	PreserveModels bool
	DataSource     string // auto_collect | manual
	FetchedAt      time.Time
	// Payload 是不落结构化列的字段（高峰倍率/独占/平台等，ISSUE-005 §3.1）。
	Payload map[string]any
}

// UpsertGroups 写入分组与其可用模型。
//
// **单事务**（09 §5.0bis：③ 分组一个事务）。两个语义各不相同：
//   - channel_groups：upsert（分组本身长期存在，倍率会变）
//   - group_models：完整响应按分组全量替换；缺少模型字段的 degraded 响应保留旧清单
func UpsertGroups(ctx context.Context, conn *pgx.Conn, rows []GroupRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开启分组事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var n int
	for _, g := range rows {
		if g.GroupRef == "" {
			// 空 group_ref 会撞 UNIQUE(channel_id, group_ref) 且没有意义，
			// 跳过而非报错：一个坏分组不该让整次采集失败
			continue
		}
		var gid int64
		err := tx.QueryRow(ctx, `
INSERT INTO channel_groups (channel_id, group_ref, rate_multiplier, data_source, fetched_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (channel_id, group_ref) DO UPDATE
   SET rate_multiplier = EXCLUDED.rate_multiplier,
       data_source     = EXCLUDED.data_source,
       fetched_at      = EXCLUDED.fetched_at
RETURNING id`, g.ChannelID, g.GroupRef, g.RateMultiplier, g.DataSource, g.FetchedAt).Scan(&gid)
		if err != nil {
			return n, fmt.Errorf("写分组 %s: %w", g.GroupRef, err)
		}

		if !g.PreserveModels {
			// 完整响应全量替换；显式空数组表示该分组当前没有可用模型。
			if _, err := tx.Exec(ctx,
				`DELETE FROM group_models WHERE channel_group_id = $1`, gid); err != nil {
				return n, fmt.Errorf("清空分组 %s 的模型: %w", g.GroupRef, err)
			}
			for _, name := range g.AvailableModels {
				if name == "" {
					continue
				}
				if _, err := tx.Exec(ctx, `
INSERT INTO group_models (channel_group_id, model_name, fetched_at)
VALUES ($1,$2,$3)
ON CONFLICT (channel_group_id, model_name) DO UPDATE SET fetched_at = EXCLUDED.fetched_at`,
					gid, name, g.FetchedAt); err != nil {
					return n, fmt.Errorf("写分组 %s 的模型 %s: %w", g.GroupRef, name, err)
				}
			}
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交分组事务: %w", err)
	}
	return n, nil
}

// GroupIDByRef 取某渠道下分组的主键，供 Key 落库时填 channel_group_id。
func GroupIDByRef(ctx context.Context, conn *pgx.Conn, channelID int64, ref string) (int64, bool, error) {
	var id int64
	err := conn.QueryRow(ctx,
		`SELECT id FROM channel_groups WHERE channel_id=$1 AND group_ref=$2`,
		channelID, ref).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查分组 %s: %w", ref, err)
	}
	return id, true, nil
}

// ── Key 用量（FR-122/125/127）──

// KeyUsageRow 是一把 Key 的采集结果。
type KeyUsageRow struct {
	// KeyID 是库中 upstream_keys.id。
	// **由调用方按 KeyRef 匹配得出** —— 采集侧拿不到明文，无法反查。
	KeyID            int64
	RemainQuotaUSD   *float64
	UsedQuotaUSD     *float64
	RPMLimit         *int
	ConcurrencyLimit *int
	ChannelGroupID   *int64
	ExpiredAt        *time.Time
	SyncedAt         time.Time
}

// UpdateKeyUsage 更新 Key 的用量与限流列。
//
// ⚠️ **只 UPDATE 不 INSERT**（02 §1.3bis）：Key 行由 /admin/keys 人工登记
// （我们持有的凭证不可能从上游"发现"）。采到库里没有的 Key **不自动插入** ——
// upstream_keys.secret 是明文凭证，上游列表接口通常只回前缀或掩码，
// 凭空插一行没有 secret 的 Key 会让它永远不可用且污染资产台账。
//
// 每把 Key 一个事务（09 §5.0bis）：单把失败不影响其它。
func UpdateKeyUsage(ctx context.Context, conn *pgx.Conn, row KeyUsageRow) error {
	tag, err := conn.Exec(ctx, `
UPDATE upstream_keys
   SET remain_quota_usd  = COALESCE($2, remain_quota_usd),
       used_quota_usd    = COALESCE($3, used_quota_usd),
       rpm_limit         = COALESCE($4, rpm_limit),
       concurrency_limit = COALESCE($5, concurrency_limit),
       channel_group_id  = COALESCE($6, channel_group_id),
       expired_time      = COALESCE($7, expired_time),
       quota_synced_at   = $8,
       updated_at        = now()
 WHERE id = $1`,
		row.KeyID, row.RemainQuotaUSD, row.UsedQuotaUSD,
		row.RPMLimit, row.ConcurrencyLimit, row.ChannelGroupID,
		row.ExpiredAt, row.SyncedAt)
	if err != nil {
		return fmt.Errorf("更新 Key %d 用量: %w", row.KeyID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("Key %d 不存在（采集不自动创建 Key，见 02 §1.3bis）", row.KeyID)
	}
	return nil
}

// KeyRefIndex 返回某账号下 KeyRef → upstream_keys.id 的映射。
//
// 采集侧只能拿到脱敏引用（上游的 key id/name），故用 external_ref 匹配。
// 匹配不上的 Key 计入 inventory 异常项"上游存在但库中未登记"。
func KeyRefIndex(ctx context.Context, db DBTX, accountID int64) (map[string]int64, error) {
	rows, err := db.Query(ctx, `
SELECT k.id, COALESCE(k.external_ref,'')
  FROM upstream_keys k
 WHERE k.account_id = $1`, accountID)
	if err != nil {
		return nil, fmt.Errorf("查账号 %d 的 Key: %w", accountID, err)
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var id int64
		var ref string
		if err := rows.Scan(&id, &ref); err != nil {
			return nil, err
		}
		if ref != "" {
			out[ref] = id
		}
	}
	return out, rows.Err()
}

// ── 采集快照（02 §7.1 的 payload 结构）──

// SnapshotRow 是一条采集快照。
type SnapshotRow struct {
	ChannelID  int64
	ScopeType  string // account | key | group | pricing（subscription 属 P4）
	ScopeID    string
	Payload    map[string]any
	DataSource string
	FetchedAt  time.Time
	ValidUntil *time.Time
}

// InsertSnapshot 写一条采集快照。
//
// payload 结构见 02 §7.1：**未采到的字段一律省略该键，不写 null** ——
// 便于区分"没采到"与"采到的值是 0"。这条纪律由调用方保证（构造 map 时
// 只放有值的键），本函数只负责序列化。
func InsertSnapshot(ctx context.Context, conn DBTX, row SnapshotRow) error {
	payload, err := json.Marshal(row.Payload)
	if err != nil {
		return fmt.Errorf("序列化 payload: %w", err)
	}
	_, err = conn.Exec(ctx, `
INSERT INTO collector_snapshots (id, channel_id, scope_type, scope_id,
                                 payload, data_source, fetched_at, valid_until)
VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8)`,
		NewUUIDv7(), row.ChannelID, row.ScopeType, row.ScopeID,
		string(payload), row.DataSource, row.FetchedAt, row.ValidUntil)
	if err != nil {
		return fmt.Errorf("写快照（%s/%s）: %w", row.ScopeType, row.ScopeID, err)
	}
	return nil
}

// KeyUsageHistory 返回某 Key 的用量时序（FR-125，供 /admin/keys/{id}/usage）。
//
// 读取契约见 02 §7.1：按 fetched_at 升序，**缺键的点位跳过该字段而非填 0**
// —— 填 0 会在曲线上造出假的"额度归零"。
type KeyUsagePoint struct {
	FetchedAt      time.Time `json:"fetched_at"`
	RemainQuotaUSD *float64  `json:"remain_quota_usd,omitempty"`
	UsedQuotaUSD   *float64  `json:"used_quota_usd,omitempty"`
	RequestCount   *int64    `json:"request_count,omitempty"`
}

// KeyUsageHistory 查询用量时序。
func KeyUsageHistory(
	ctx context.Context, conn *pgx.Conn, channelID int64, keyID int64,
	from, to time.Time,
) ([]KeyUsagePoint, error) {
	rows, err := conn.Query(ctx, `
SELECT fetched_at, payload
  FROM collector_snapshots
 WHERE channel_id = $1 AND scope_type = 'key' AND scope_id = $2
   AND fetched_at >= $3 AND fetched_at <= $4
 ORDER BY fetched_at ASC`,
		channelID, fmt.Sprint(keyID), from, to)
	if err != nil {
		return nil, fmt.Errorf("查 Key %d 用量时序: %w", keyID, err)
	}
	defer rows.Close()

	var out []KeyUsagePoint
	for rows.Next() {
		var at time.Time
		var raw []byte
		if err := rows.Scan(&at, &raw); err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			// 单点损坏不该让整条曲线查不出来
			continue
		}
		p := KeyUsagePoint{FetchedAt: at}
		// 只在键存在时赋值 —— 缺键跳过而不是填 0（02 §7.1）
		if v, ok := m["remain_quota_usd"]; ok {
			if f, ok2 := toFloat(v); ok2 {
				p.RemainQuotaUSD = &f
			}
		}
		if v, ok := m["used_quota_usd"]; ok {
			if f, ok2 := toFloat(v); ok2 {
				p.UsedQuotaUSD = &f
			}
		}
		if v, ok := m["request_count"]; ok {
			if f, ok2 := toFloat(v); ok2 {
				n := int64(f)
				p.RequestCount = &n
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}
