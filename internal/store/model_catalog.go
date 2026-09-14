package store

import (
	"context"
	"fmt"
	"strconv"
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
	// VendorName 是上游声明的发行方。nil = 未声明，不是"无供应商"。
	VendorName *string `json:"vendor_name,omitempty"`
	// EndpointTypes 是上游声明支持的端点类型。nil = 未声明，不是"不支持任何端点"。
	EndpointTypes []string `json:"endpoint_types,omitempty"`
	// QuotaPerUnit 是这个站点的**额度换算基数**（/api/status 的 quota_per_unit，
	// 由 Detect 采到）。没有它，倍率就只是个相对数，跨站点不可比。
	//
	// 换算：`每 1M token 美元价 = 模型倍率 × 分组倍率 × 1e6 / quota_per_unit`。
	// 实测两个真站点都是 500000（于是乘 2，对应 NewAPI 的 $0.002/1K 基准价），
	// 但**不可写死**：站点可以改它，改了之后同一个"×1"就是另一个价格 ——
	// 这正是"有些中转站把倍率设成 ×1 但实际价是官方的 0.5 或 2 倍"的来源。
	// nil = 该渠道没采到基数（老库或 Detect 失败），界面只能显示倍率、不能折算。
	QuotaPerUnit *float64 `json:"quota_per_unit,omitempty"`
	// Groups 是这个渠道下**能调到这个模型**的分组，带各自的分组倍率。
	//
	// 为什么它必须在这儿：上面那个 InputPrice 是模型自己的倍率，而实际计费是
	// **模型倍率 × 分组倍率**，分组倍率由这把 Key 所在的分组决定。实测两个真
	// 站点的分组倍率跨度是 0.12~1.5 与 0.26~3.5 —— 十倍以上。只看模型倍率选站，
	// 看到的是一个与账单差一个数量级的数字。
	//
	// 空数组 = 该渠道还没采到分组（或采到了但没有一个分组 enable 这个模型）。
	// **不要把空当成"倍率 1"** —— 那是"不知道"，不是"不打折"。
	Groups []ModelGroup `json:"groups"`
}

// ModelGroup 是一个分组及其倍率（channel_groups 的一行，按模型过滤后）。
type ModelGroup struct {
	GroupRef string `json:"group_ref"`
	// RateMultiplier 缺席 = 上游没给这个分组的倍率，同样不可当 1。
	RateMultiplier *float64 `json:"rate_multiplier,omitempty"`
	// RateDynamic 为真时上面那个倍率**不是计费倍率**：这一组是「自动选组」
	// （NewAPI 的 auto），实际倍率由运行时命中的候选组决定。消费方必须先看它。
	RateDynamic bool `json:"rate_dynamic"`
	// RateMin/RateMax 是候选分组倍率的区间（只有 RateDynamic 时有意义）。
	// 都缺席 = 候选一个都没采到倍率：只能说"动态"，说不出范围。
	RateMin *float64 `json:"rate_min,omitempty"`
	RateMax *float64 `json:"rate_max,omitempty"`
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

// CatalogFilter 是模型目录的筛选条件。空切片/空串一律表示"这一维不筛"。
type CatalogFilter struct {
	// Q 是模型名子串（大小写不敏感）。
	Q string
	// Unit 是计价口径（'per_1m_token' / 'per_call' / 'unknown'）。
	Unit string
	// ChannelIDs 限定渠道。
	ChannelIDs []int64
	// Vendors 限定发行方（上游 vendors[].name，逐字匹配，跨站点不归一）。
	Vendors []string
	// Endpoints 限定端点类型，语义是**任一命中**（数组重叠），不是全部命中：
	// 一个模型同时支持 openai 与 gemini 时，两个筛选下都该看得见它。
	Endpoints []string
	// KeyIDs 限定"**这几把 Key 调得到**"。语义与上面几维不同，值得单列：
	//
	// 它不是按目录行的某个字段筛，而是按"Key 所在分组的可用模型清单"
	// （group_models，FR-124 采的那份）筛 —— 一把 Key 固定在一个分组里，
	// 而分组决定了它能调哪些模型、以及每次调用被乘多少倍率。
	//
	// 多把 Key 取**并集**（"我这几把加起来能覆盖哪些模型"）。
	//
	// Key 自己没有分组时**回落到账号的默认分组**（upstream_accounts.account_group，
	// 来自上游 /api/user/self 的 group）—— 上游 /api/token 的 group 为空串时，
	// 调用走的就是账号那一档，所以那种 Key 不是"未归组"。两级都解析不出来时
	// （账号分组也没采到、或它不在 group_ratio 里）才真的什么都匹配不到，
	// 界面据此把这种 Key 标出来而不是让它悄悄筛出空列表。
	KeyIDs []int64
}

// GlobalCatalogPage 是一页聚合结果。
type GlobalCatalogPage struct {
	Items []ModelEntry
	// Total 是**筛选后的模型数**（不是目录行数）：一个模型在 30 个渠道上
	// 仍然只算一个。翻页的单位是模型，总数也必须是模型，否则页码对不上。
	Total int
	// 下面四个是分面计数，每一个都**在除自己以外的全部筛选之下**统计。
	//
	// 这条规则不是随手定的：分面若把自己那一维也算进去，选中一个供应商之后
	// 其余供应商全变 0 或消失，于是**换不了**供应商 —— 只能先清空再重选。
	// 反过来若一维都不施加，选了渠道之后供应商列表仍列着别的渠道才有的公司，
	// 点下去是空结果。排除自己这一维，两个毛病都没有。
	Units     map[string]int
	Vendors   map[string]int
	Endpoints map[string]int
	// Channels 的键是渠道 id 的十进制串（JSON 对象的键只能是字符串）。
	Channels map[string]int
	// ChannelNames 给上面那些 id 配名字，省得界面为了画一行分面去翻渠道列表
	// ——它未必已经拉过 /admin/channels。
	ChannelNames map[string]string
	// Whole 是**不受 Unit 影响**时的模型数（其余筛选照常施加）。
	//
	// ⚠️ **不能拿 Units 的各项相加代替**：同一个模型可能在 A 站按倍率、在 B 站
	// 按次计价，于是它在两个分段里各算一次。实测两个真站点合起来
	// 1397 个模型，而分段相加是 1398 —— 差的就是那一个两种口径都有的模型。
	// 界面上「全部」那个数若用相加，会比逐段翻完能看到的多出几个，对不上。
	Whole int
}

// catalogWhere 拼筛选条件。excl 指定**不施加**哪一维（分面自己那一维）。
//
// 参数位置固定为 $1..$6（q / channels / vendors / endpoints / unit / keys），
// 排除某一维时把该位置传成空值，让那一行条件自己短路 —— 这样每条查询的
// 占位符编号都一样，不用为每种组合重排参数。手工拼编号是这类代码最常见的
// 错法，而错了之后查询照样能跑，只是筛错了维度。
//
// vendor_name IS NULL 的模型不出现在任何供应商分面里，也就选不到 ——
// 与上游自己的定价页一致，那里也没有"未声明供应商"这一项。endpoint_types
// 同理。它们仍然计入「全部」。
func catalogWhere(f CatalogFilter, excl string) (string, []any) {
	args := []any{
		f.Q,
		int64Slice(f.ChannelIDs),
		strSlice(f.Vendors),
		strSlice(f.Endpoints),
		f.Unit,
		int64Slice(f.KeyIDs),
	}
	switch excl {
	case "q":
		args[0] = ""
	case "channel":
		args[1] = []int64{}
	case "vendor":
		args[2] = []string{}
	case "endpoint":
		args[3] = []string{}
	case "unit":
		args[4] = ""
	case "key":
		args[5] = []int64{}
	}
	// Key 那一维走 EXISTS 而不是 join：一个模型可能被选中的多把 Key 同时覆盖，
	// join 会让它在结果里出现多次，而上面那些 count(*) 数的是渠道数 ——
	// 一把 Key 就能把某个渠道的计数翻倍，且看起来只是"这个模型渠道多"。
	return `($1 = '' OR position(lower($1) in lower(c.model_name)) > 0)
   AND (cardinality($2::bigint[]) = 0 OR c.channel_id = ANY($2))
   AND (cardinality($3::text[]) = 0 OR c.vendor_name = ANY($3))
   AND (cardinality($4::text[]) = 0 OR c.endpoint_types && $4)
   AND ($5 = '' OR COALESCE(c.billing_unit, 'unknown') = $5)
   AND (cardinality($6::bigint[]) = 0 OR EXISTS (
         SELECT 1 FROM upstream_keys k
           JOIN upstream_accounts ua ON ua.id = k.account_id
           JOIN channel_groups g
             ON g.id = COALESCE(k.channel_group_id, (
                  SELECT g2.id FROM channel_groups g2
                   WHERE g2.channel_id = ua.channel_id
                     AND g2.group_ref = ua.account_group))
           JOIN group_models gm ON gm.channel_group_id = g.id
          WHERE k.id = ANY($6)
            AND g.channel_id = c.channel_id
            AND gm.model_name = c.model_name))`, args
}

// nil 切片传进 pgx 会变成 NULL 而不是空数组，而 cardinality(NULL) 是 NULL、
// 不是 0 —— 于是那一行条件整体成 NULL，WHERE 直接把所有行滤掉。空结果，无报错。
func int64Slice(v []int64) []int64 {
	if v == nil {
		return []int64{}
	}
	return v
}

func strSlice(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ListGlobalCatalog 按模型名聚合全部渠道的目录。
//
// 筛选见 CatalogFilter；missingRounds 同 ListCatalog。
//
// 用 `position(lower(q) in lower(model_name))` 而不是 ILIKE '%q%'：
// 后者要对用户输入里的 % 和 _ 做转义，漏一处就是"输入 % 匹配全部"。
// 语义与 filterCatalog 的 strings.Contains 逐字相同，不需要第二套解释。
//
// 不建索引：真库 64 个渠道 × 约 1400 个模型 ≈ 9 万行，分组扫全表在 PG 上是
// 毫秒级，而子串匹配本来也用不上 B-tree。要建也该是 trigram，等它真慢了再说。
func ListGlobalCatalog(
	ctx context.Context, conn *pgx.Conn,
	f CatalogFilter, missingRounds, limit, offset int,
) (GlobalCatalogPage, error) {
	if missingRounds <= 0 {
		missingRounds = 3
	}
	page := GlobalCatalogPage{
		Items: []ModelEntry{}, Units: map[string]int{},
		Vendors: map[string]int{}, Endpoints: map[string]int{},
		Channels: map[string]int{}, ChannelNames: map[string]string{},
	}

	// ① 四个分面。每个都排除自己那一维（见 GlobalCatalogPage 的注释）。
	//
	// 统一 count(DISTINCT model_name)：分面上的数是**模型数**，与列表的总数
	// 同口径。数目录行的话，一个在 30 个渠道都有的模型会被算 30 次，于是
	// 分面上的数字远大于点进去看到的行数。
	facets := []struct {
		into map[string]int
		excl string
		sql  string
		what string
	}{
		{page.Units, "unit", `
SELECT COALESCE(c.billing_unit, 'unknown'), count(DISTINCT c.model_name)::int
  FROM channel_model_catalog c WHERE %s GROUP BY 1`, "计价口径"},
		{page.Vendors, "vendor", `
SELECT c.vendor_name, count(DISTINCT c.model_name)::int
  FROM channel_model_catalog c WHERE c.vendor_name IS NOT NULL AND %s GROUP BY 1`, "发行方"},
		{page.Endpoints, "endpoint", `
SELECT e, count(DISTINCT c.model_name)::int
  FROM channel_model_catalog c, unnest(c.endpoint_types) AS e
 WHERE %s GROUP BY 1`, "端点类型"},
	}
	for _, ft := range facets {
		where, args := catalogWhere(f, ft.excl)
		rows, err := conn.Query(ctx, fmt.Sprintf(ft.sql, where), args...)
		if err != nil {
			return page, fmt.Errorf("统计目录%s分面: %w", ft.what, err)
		}
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				rows.Close()
				return page, err
			}
			ft.into[k] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return page, err
		}
	}

	// ①bis 渠道分面。单独一条：它要连 channels 取名字，别的分面不用。
	{
		where, args := catalogWhere(f, "channel")
		rows, err := conn.Query(ctx, `
SELECT c.channel_id, ch.name, count(DISTINCT c.model_name)::int
  FROM channel_model_catalog c JOIN channels ch ON ch.id = c.channel_id
 WHERE `+where+`
 GROUP BY 1, 2`, args...)
		if err != nil {
			return page, fmt.Errorf("统计目录渠道分面: %w", err)
		}
		for rows.Next() {
			var id int64
			var name string
			var n int
			if err := rows.Scan(&id, &name, &n); err != nil {
				rows.Close()
				return page, err
			}
			key := strconv.FormatInt(id, 10)
			page.Channels[key] = n
			page.ChannelNames[key] = name
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return page, err
		}
	}

	// ①ter 不分段时的模型数。单独一条，理由见 Whole 的注释（相加会多算）。
	{
		where, args := catalogWhere(f, "unit")
		if err := conn.QueryRow(ctx, `
SELECT count(DISTINCT c.model_name)::int
  FROM channel_model_catalog c WHERE `+where, args...).Scan(&page.Whole); err != nil {
			return page, fmt.Errorf("统计目录模型总数: %w", err)
		}
	}
	where, args := catalogWhere(f, "")

	// ② 本页的模型名与三个计数。
	//
	// `count(*) OVER ()` 在分组之后、LIMIT 之前求值，所以它数的是**组数**
	// ——正是我们要的 Total。单独再发一条 count(DISTINCT) 也行，但那条要把
	// 同样的 WHERE 再写一遍，两处迟早会岔开。
	//
	// 排序按"支持的渠道数"降序：这一页要回答的是"哪些模型到处都有"。
	// 同数再按名字，保证翻页稳定 —— 只按计数排的话同计数的行在两次查询间
	// 顺序可以不同，翻页会漏行也会重复行。
	//
	// ⚠️ 计数是**在筛选之后**数的：筛了渠道以后，"渠道数"显示的是"筛出的这些
	// 渠道里有几个有它"，而不是全库有几个。这是刻意的 —— 筛了渠道还报全库的数，
	// 展开区列出的行数会对不上那个数字。
	rows, err := conn.Query(ctx, fmt.Sprintf(`
SELECT c.model_name,
       count(*)::int,
       count(*) FILTER (
         WHERE ch.catalog_sync_seq - c.last_seen_seq >= $7::bigint)::int,
       count(*) FILTER (WHERE ch.status = 'disabled')::int,
       count(*) OVER ()::int
  FROM channel_model_catalog c JOIN channels ch ON ch.id = c.channel_id
 WHERE %s
 GROUP BY c.model_name
 ORDER BY count(*) DESC, c.model_name
 LIMIT $8 OFFSET $9`, where),
		append(args, missingRounds, limit, offset)...)
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
	// **整套筛选都要在这里再加一遍**，不只是 unit：筛了渠道却不筛明细的话，
	// 展开区会列出被筛掉的那些渠道，与上面那个"渠道数"对不上；筛了供应商同理。
	// 这就是 catalogWhere 存在的理由 —— 两处条件必须逐字同源。
	rows, err = conn.Query(ctx, fmt.Sprintf(`
SELECT c.model_name, ch.id, ch.name, ch.status,
       c.input_price, c.output_price, c.billing_unit,
       c.vendor_name, c.endpoint_types,
       (ch.catalog_sync_seq - c.last_seen_seq >= $7::bigint) AS stale,
       c.last_seen_at
  FROM channel_model_catalog c JOIN channels ch ON ch.id = c.channel_id
 WHERE c.model_name = ANY($8) AND %s
 ORDER BY c.model_name, ch.name, ch.id`, where),
		append(args, missingRounds, names)...)
	if err != nil {
		return page, fmt.Errorf("列模型的渠道明细: %w", err)
	}
	// 指到本页每个 (模型, 渠道) 那一行，供 ④ 把分组挂上去。
	slot := map[string]*ModelChannel{}
	for rows.Next() {
		var name string
		var mc ModelChannel
		if err := rows.Scan(&name, &mc.ChannelID, &mc.ChannelName, &mc.ChannelStatus,
			&mc.InputPrice, &mc.OutputPrice, &mc.BillingUnit,
			&mc.VendorName, &mc.EndpointTypes,
			&mc.Stale, &mc.LastSeenAt); err != nil {
			rows.Close()
			return page, err
		}
		mc.Groups = []ModelGroup{}
		if e, ok := byName[name]; ok {
			e.Channels = append(e.Channels, mc)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	for _, e := range byName {
		for i := range e.Channels {
			slot[groupKey(e.ModelName, e.Channels[i].ChannelID)] = &e.Channels[i]
		}
	}

	// ③bis 每个渠道的额度换算基数（quota_per_unit），用来把倍率折成绝对美元价。
	//
	// 它存在 Detect 那条快照里（collector_snapshots 的 __detect__ 行），
	// 一个渠道一条。DISTINCT ON 取最近一次 —— 站点改了基数，重新 Detect
	// 之后这里就跟着变。
	//
	// 拿不到的渠道**不折算**（QuotaPerUnit 留 nil），界面只显示倍率：
	// 猜一个 500000 会在改过基数的站上给出一个看起来精确的错价，
	// 而那正是这一段要防的事。
	{
		rows, err := conn.Query(ctx, `
SELECT DISTINCT ON (channel_id) channel_id, (payload->>'quota_per_unit')::float8
  FROM collector_snapshots
 WHERE scope_type='pricing' AND scope_id='__detect__' AND payload ? 'quota_per_unit'
 ORDER BY channel_id, fetched_at DESC`)
		if err != nil {
			return page, fmt.Errorf("读渠道额度换算基数: %w", err)
		}
		qpu := map[int64]float64{}
		for rows.Next() {
			var id int64
			var v *float64
			if err := rows.Scan(&id, &v); err != nil {
				rows.Close()
				return page, err
			}
			if v != nil && *v > 0 {
				qpu[id] = *v
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return page, err
		}
		for _, e := range byName {
			for i := range e.Channels {
				if v, ok := qpu[e.Channels[i].ChannelID]; ok {
					base := v
					e.Channels[i].QuotaPerUnit = &base
				}
			}
		}
	}

	// ④ 每个 (模型, 渠道) 下**能调到这个模型**的分组及其倍率。
	//
	// 归属来自 group_models（NewAPI 由每个模型的 enable_groups 反转得来），
	// 不是"把渠道下所有分组都挂上" —— 后者会把一个调不到这个模型的分组
	// 连同它那个漂亮的 0.12 倍率一起显示出来，而那是选不到的价格。
	//
	// 按倍率升序：先看见最便宜的那个分组，这是选型时唯一想先知道的事。
	//
	// 按 Key 筛选时**只留这几把 Key 自己的分组**：那时人问的不是"这个模型最便宜
	// 能到多少"，而是"我这把 Key 调它要多少钱"。留着别的分组，界面上那句
	// "最低 · 分组 X（共 4 个分组，最高 17.5）"里的 X 会是一个他根本用不上的
	// 分组 —— 一个精确且无关的数字。
	rows, err = conn.Query(ctx, `
SELECT gm.model_name, g.channel_id, g.group_ref, g.rate_multiplier,
       g.rate_dynamic,
       (SELECT min(c2.rate_multiplier) FROM channel_groups c2
         WHERE c2.channel_id = g.channel_id
           AND c2.group_ref = ANY(g.dynamic_candidates)),
       (SELECT max(c2.rate_multiplier) FROM channel_groups c2
         WHERE c2.channel_id = g.channel_id
           AND c2.group_ref = ANY(g.dynamic_candidates))
  FROM group_models gm JOIN channel_groups g ON g.id = gm.channel_group_id
 WHERE gm.model_name = ANY($1)
   AND (cardinality($2::bigint[]) = 0
        OR g.id IN (
             SELECT COALESCE(k.channel_group_id, (
                      SELECT g2.id FROM channel_groups g2
                       WHERE g2.channel_id = ua.channel_id
                         AND g2.group_ref = ua.account_group))
               FROM upstream_keys k
               JOIN upstream_accounts ua ON ua.id = k.account_id
              WHERE k.id = ANY($2)))
 ORDER BY gm.model_name, g.channel_id, g.rate_multiplier NULLS LAST, g.group_ref`,
		names, int64Slice(f.KeyIDs))
	if err != nil {
		return page, fmt.Errorf("列模型的分组倍率: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var channelID int64
		var g ModelGroup
		if err := rows.Scan(&name, &channelID, &g.GroupRef, &g.RateMultiplier,
			&g.RateDynamic, &g.RateMin, &g.RateMax); err != nil {
			return page, err
		}
		if mc, ok := slot[groupKey(name, channelID)]; ok {
			mc.Groups = append(mc.Groups, g)
		}
	}
	return page, rows.Err()
}

// groupKey 拼 (模型名, 渠道 id) 的复合键。用 \x00 分隔：模型名里什么都可能有
// （斜杠、冒号、点），但不会有 NUL，所以拼出来的键不会与另一对撞上。
func groupKey(model string, channelID int64) string {
	return model + "\x00" + strconv.FormatInt(channelID, 10)
}
