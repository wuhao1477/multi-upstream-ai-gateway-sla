package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 跨渠道的模型目录聚合（「这个模型哪些渠道有」）。
//
// 与 ListCatalog 的分工：那个回答"这个渠道有什么模型"，是**建站视角**；
// 这里回答"这个模型在哪些渠道上"，是**选型视角**。同一张
// `channel_model_catalog`，两个方向的问题，SQL 形状完全不同 ——
// 前者按 channel_id 取一片（走 PK 前缀），后者按 model_name 分组扫全表。
//
// ⚠️ 仍然是**目录**，不是「可路由模型」（`models` 表，02 §1.3）。
// 目录说的是"上游声明它有"，与"我方已登记为可路由"是两层，端点因此叫
// /admin/catalog 而不是 /admin/models —— 后者留给那张表。

// ModelChannel 是"某模型在某渠道上"的一行。
type ModelChannel struct {
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	// ChannelStatus 必须带上：已停用的渠道照定义不采集也不承接请求
	// （disabledSyncMessage），它的目录行是停用前留下的存量。
	// 不给状态的话，"5 个渠道支持"里混进两个死渠道，而界面上看不出来。
	ChannelStatus string    `json:"channel_status"`
	InputPrice    *float64  `json:"input_price,omitempty"`
	OutputPrice   *float64  `json:"output_price,omitempty"`
	BillingUnit   *string   `json:"billing_unit,omitempty"`
	Stale         bool      `json:"stale"`
	LastSeenAt    time.Time `json:"last_seen_at"`
}

// ModelEntry 是全局模型目录的一行：一个模型名 + 有它的全部渠道。
//
// 三个计数各自独立，**不做减法**：`channel_count` 是有这个模型的渠道总数，
// 另两个是其中的子集且可以重叠（一个渠道既可能停用又可能陈旧）。
// 合成一个"可用渠道数"需要定义"可用"，而那个定义会随 P2 的判闸逻辑变 ——
// 这里只报事实，怎么算可用交给看的人。
type ModelEntry struct {
	ModelName     string         `json:"model_name"`
	ChannelCount  int            `json:"channel_count"`
	StaleCount    int            `json:"stale_count"`
	DisabledCount int            `json:"disabled_count"`
	Channels      []ModelChannel `json:"channels"`
}

// GlobalCatalogPage 是一页聚合结果。
type GlobalCatalogPage struct {
	Items []ModelEntry
	// Total 是**筛选后的模型数**（不是目录行数）：一个模型在 30 个渠道上
	// 仍然只算一个。翻页的单位是模型，总数也必须是模型，否则页码对不上。
	Total int
	// Units 是各计价口径的模型数，**在 unit 分段筛选之前**统计。
	// 理由同 filterCatalog：筛完再数就只剩当前段，切进去就出不来了。
	Units map[string]int
	// Whole 是不分段时的模型数（只受 q 影响）。
	//
	// ⚠️ **不能拿 Units 的各项相加代替**：同一个模型可能在 A 站按倍率、在 B 站
	// 按次计价，于是它在两个分段里各算一次。实测两个真站点合起来
	// 1397 个模型，而分段相加是 1398 —— 差的就是那一个两种口径都有的模型。
	// 界面上「全部」那个数若用相加，会比逐段翻完能看到的多出几个，对不上。
	Whole int
}

// ListGlobalCatalog 按模型名聚合全部渠道的目录。
//
// q 是模型名子串（大小写不敏感）；unit 是计价口径分段（空 = 不筛，
// "unknown" 选 billing_unit IS NULL）；missingRounds 同 ListCatalog。
//
// 用 `position(lower(q) in lower(model_name))` 而不是 ILIKE '%q%'：
// 后者要对用户输入里的 % 和 _ 做转义，漏一处就是"输入 % 匹配全部"。
// 语义与 filterCatalog 的 strings.Contains 逐字相同，不需要第二套解释。
//
// 不建索引：真库 64 个渠道 × 约 1400 个模型 ≈ 9 万行，分组扫全表在 PG 上是
// 毫秒级，而子串匹配本来也用不上 B-tree。要建也该是 trigram，等它真慢了再说。
func ListGlobalCatalog(
	ctx context.Context, conn *pgx.Conn,
	q, unit string, missingRounds, limit, offset int,
) (GlobalCatalogPage, error) {
	if missingRounds <= 0 {
		missingRounds = 3
	}
	page := GlobalCatalogPage{Items: []ModelEntry{}, Units: map[string]int{}}

	// ① 分段规模。先算，且**不受 unit 影响** —— 界面的分段按钮读它。
	rows, err := conn.Query(ctx, `
SELECT COALESCE(billing_unit, 'unknown'), count(DISTINCT model_name)::int
  FROM channel_model_catalog
 WHERE ($1 = '' OR position(lower($1) in lower(model_name)) > 0)
 GROUP BY 1`, q)
	if err != nil {
		return page, fmt.Errorf("统计目录计价口径: %w", err)
	}
	for rows.Next() {
		var u string
		var n int
		if err := rows.Scan(&u, &n); err != nil {
			rows.Close()
			return page, err
		}
		page.Units[u] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}

	// ①bis 不分段时的模型数。单独一条，理由见 Whole 的注释（相加会多算）。
	if err := conn.QueryRow(ctx, `
SELECT count(DISTINCT model_name)::int
  FROM channel_model_catalog
 WHERE ($1 = '' OR position(lower($1) in lower(model_name)) > 0)`,
		q).Scan(&page.Whole); err != nil {
		return page, fmt.Errorf("统计目录模型总数: %w", err)
	}

	// ② 本页的模型名与三个计数。
	//
	// `count(*) OVER ()` 在分组之后、LIMIT 之前求值，所以它数的是**组数**
	// ——正是我们要的 Total。单独再发一条 count(DISTINCT) 也行，但那条要把
	// 同样的 WHERE 再写一遍，两处迟早会岔开。
	//
	// 排序按"支持的渠道数"降序：这一页要回答的是"哪些模型到处都有"。
	// 同数再按名字，保证翻页稳定 —— 只按计数排的话同计数的行在两次查询间
	// 顺序可以不同，翻页会漏行也会重复行。
	rows, err = conn.Query(ctx, `
SELECT c.model_name,
       count(*)::int,
       count(*) FILTER (
         WHERE ch.catalog_sync_seq - c.last_seen_seq >= $3::bigint)::int,
       count(*) FILTER (WHERE ch.status = 'disabled')::int,
       count(*) OVER ()::int
  FROM channel_model_catalog c JOIN channels ch ON ch.id = c.channel_id
 WHERE ($1 = '' OR position(lower($1) in lower(c.model_name)) > 0)
   AND ($2 = '' OR COALESCE(c.billing_unit, 'unknown') = $2)
 GROUP BY c.model_name
 ORDER BY count(*) DESC, c.model_name
 LIMIT $4 OFFSET $5`, q, unit, missingRounds, limit, offset)
	if err != nil {
		return page, fmt.Errorf("列全局模型目录: %w", err)
	}
	names := make([]string, 0, limit)
	byName := map[string]*ModelEntry{}
	for rows.Next() {
		var e ModelEntry
		if err := rows.Scan(&e.ModelName, &e.ChannelCount,
			&e.StaleCount, &e.DisabledCount, &page.Total); err != nil {
			rows.Close()
			return page, err
		}
		e.Channels = []ModelChannel{}
		page.Items = append(page.Items, e)
		names = append(names, e.ModelName)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	for i := range page.Items {
		byName[page.Items[i].ModelName] = &page.Items[i]
	}
	if len(names) == 0 {
		return page, nil
	}

	// ③ 本页这些模型的逐渠道明细。
	//
	// 只取本页的模型名（= ANY），不是把全表拉回来在 Go 里分组：真库九万行
	// 一次请求拉回来是几 MB，而筛选框每敲一次（防抖后）就要发一次。
	//
	// unit 筛选**必须在这里也加一遍**：切到"按次"那一段时，若明细不筛，
	// 展开后会看见一堆按倍率计价的渠道行，与上面那个计数对不上。
	rows, err = conn.Query(ctx, `
SELECT c.model_name, ch.id, ch.name, ch.status,
       c.input_price, c.output_price, c.billing_unit,
       (ch.catalog_sync_seq - c.last_seen_seq >= $2::bigint) AS stale,
       c.last_seen_at
  FROM channel_model_catalog c JOIN channels ch ON ch.id = c.channel_id
 WHERE c.model_name = ANY($1)
   AND ($3 = '' OR COALESCE(c.billing_unit, 'unknown') = $3)
 ORDER BY c.model_name, ch.name, ch.id`, names, missingRounds, unit)
	if err != nil {
		return page, fmt.Errorf("列模型的渠道明细: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var mc ModelChannel
		if err := rows.Scan(&name, &mc.ChannelID, &mc.ChannelName, &mc.ChannelStatus,
			&mc.InputPrice, &mc.OutputPrice, &mc.BillingUnit,
			&mc.Stale, &mc.LastSeenAt); err != nil {
			return page, err
		}
		if e, ok := byName[name]; ok {
			e.Channels = append(e.Channels, mc)
		}
	}
	return page, rows.Err()
}
