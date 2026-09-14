/**
 * 后端 /admin/* 的线上形状。逐字对着 Go 结构体的 json tag 写，不是照界面猜的。
 *
 * 两条规则贯穿全文件：
 *  - Go 的 `omitempty` + 指针 → TS 的 `?: T`（字段可能整个缺席）。
 *  - Go 的 `*T` 且语义是"上游没声明" → 允许 null，且**不许**用 `?? 默认值` 悄悄填平：
 *    价格口径 billing_unit 就是这么出过缺陷的（02 §1.3bis：nil = 未声明，按未知处理）。
 */

// ── 站型与枚举 ───────────────────────────────────────────────────────────────

/**
 * 站点家族。
 *
 * ⚠️ 真相源是后端的站型注册表（internal/collector/registry.go），不是这一行。
 * 这里只是给编辑器用的当前快照 —— 加站型时它会**落后而不报错**（值从 JSON 来，
 * TS 管不着）。所以界面的站型下拉不读这个联合类型，读 `/admin/site-families`，
 * 见 SiteFamilyInfo。
 *
 * 刻意**不写成 `| (string & {})`**（那样能兼容任何新族）：放宽之后
 * `family === '某个已删的族'` 也不再是类型错误，而删族时正需要 TS 把残留的
 * 比较全指出来。宁可加族时来改这一行 —— 那一次改是有人在场的。
 */
export type SiteFamily = 'newapi' | 'sub2api' | 'unknown'

/** 一个已注册站型（GET /admin/site-families，对着 listSiteFamilies 的 item 写）。 */
export interface SiteFamilyInfo {
  family: SiteFamily
  display_name: string
  aliases: string[]
  cred_type: string
  requires_external_user_id: boolean
}

/** 采集能力项。 */
export type Capability =
  | 'account'
  | 'keys'
  | 'groups'
  | 'pricing'
  | 'model_catalog'

/** 单项采集结果状态。partial 表示多账号部分成功；skipped 是被限流跳过。 */
export type ItemStatus = 'ok' | 'partial' | 'failed' | 'unsupported' | 'skipped'

/** 能力矩阵：能力 → 支持程度。 */
export type SupportLevel = 'supported' | 'degraded' | 'unsupported'
export type CapabilityMap = Partial<Record<Capability, SupportLevel>>

// ── 列表实体（internal/store/channels.go）─────────────────────────────────────

export interface Channel {
  id: number
  name: string
  site_family: SiteFamily
  base_url: string
  status: string
  disabled_reason?: string
  disabled_until?: string
  created_at: string
  updated_at: string
}

export interface Account {
  id: number
  channel_id: number
  external_user_id?: string
  balance_group_key?: string
  /**
   * 账号在上游的默认分组（/api/user/self 的 group）。
   * Key 自己没定分组时调用走的就是它 —— 那种 Key 不是"未归组"。
   * 缺席 = 未采到，**不可当成 "default"**。
   */
  account_group?: string
  status: string
  disabled_reason?: string
  disabled_until?: string
  created_at: string

  /**
   * 账号余额（归一美元，FR-018 一期 1:1），来自 balance_signals 最近一行。
   *
   * **缺席 = 从未采到，不是 0**（FR-020/026）。绝不许 `?? 0` —— 那会把
   * 一个查不到余额的账号渲染成"已耗尽"，运维会去充一笔不需要的钱。
   * 三种情形要分开渲染：有值 / 未采集（本字段缺席）/ 状态非 normal。
   */
  balance_usd?: number
  /** 余额五态：normal | critical | unknown | exhausted | abnormal。缺席=无余额信号。 */
  balance_state?: string
  /** 上面那个余额的确认时刻。**必须与金额同格展示** —— 三天前的余额和五分钟前的余额不是一回事。 */
  balance_confirmed_at?: string

  /** 该账号下的 Key 数。停用账号的确认框要写「将影响 N 把 Key」。 */
  keys_total: number
  keys_active: number

  /**
   * 采集凭证的**存在性与形态**，绝不含令牌内容（FR-094）。
   *
   * 库里 `collector_credentials` 有 UNIQUE(account_id)，一个账号最多一条 ——
   * 凭证是账号的属性，所以它跟着账号行走，不再有独立的凭证分栏。
   * `cred_type` 缺席 = 没登记 = **这个账号采不了**。
   */
  cred_type?: string
  cred_status?: string
  cred_expires_at?: string
}

export interface Key {
  id: number
  account_id: number
  channel_id: number
  /** 只有前缀，永远拿不到完整密钥（FR-094）。 */
  secret_prefix: string
  external_ref?: string
  channel_group_id?: number
  group_ref?: string
  rate_multiplier?: number
  /**
   * 上面那个分组是**跟账号走**的，不是这把 Key 自己定的。
   *
   * 上游 /api/token 的 group 为空串时，调用实际走账号的默认分组
   * （/api/user/self 的 group）。两者倍率同样真实，但改法不同：账号分组变了
   * 这一把跟着变，自己定的那把不变 —— 界面上要分开说，否则会改错地方。
   */
  group_inherited?: boolean
  remain_quota_usd?: number
  used_quota_usd?: number
  /**
   * 上游声明该 Key 不限额度。
   *
   * **必须先看它再看 remain_quota_usd**：NewAPI 对无限额 Key 回
   * `remain_quota: 0`，不看这一位就会把「不限额度」渲染成 `$0.0000`，
   * 与「额度耗尽」完全无法区分。
   *
   * 后端不带 omitempty（false 也会出现），所以它缺席只意味着后端版本旧。
   */
  unlimited_quota?: boolean
  rpm_limit?: number
  concurrency_limit?: number
  status: string
  expired_time?: string
  quota_synced_at?: string
  created_at: string
}

export interface ChannelGroup {
  id: number
  channel_id: number
  group_ref: string
  rate_multiplier?: number
  model_count: number
  data_source: string
  fetched_at: string
}

export interface CatalogEntry {
  model_name: string
  input_price?: number
  output_price?: number
  /**
   * 上面两个价格的口径，**必须与价格一同展示**。
   * 缺席/null = 上游未声明，按"口径未知"渲染，不要当成某个默认口径。
   */
  billing_unit?: string | null
  first_seen_at: string
  last_seen_at: string
  /** 疑似下架（last_seen_at 落后于最新一轮采集）。 */
  stale: boolean
}

// ── 响应包裹（internal/admin/upstream_api.go）─────────────────────────────────

export interface ListResp<T> {
  count: number
  items: T[]
}

export interface InventoryResp {
  channel_id: number
  name: string
  site_family: SiteFamily
  accounts: number
  keys: number
  groups: number
  catalog_models: number
  quota_total_usd: number
  last_synced_at?: string
  /** 后端总是先置空数组再 append，不会是 null。 */
  anomalies: AnomalyGroup[]
}

export interface AnomalyGroup {
  kind: string
  count: number
  hint: string
  /** 截断到前若干个（后端 cap = 20）。 */
  items?: string[]
}

/**
 * 单项采集结果。
 *
 * capability 与 elapsed_ms 标成可选是**必须的**：429/409/422 的失败响应体里
 * items 只有 `{"status":"skipped"}` 一个字段（failWith 手工拼的 map，
 * 不是 SyncItem 的序列化）。当成必填会渲染出 "undefined ms"。
 */
export interface SyncItem {
  capability?: Capability
  support?: SupportLevel
  status: ItemStatus
  elapsed_ms?: number
  rows?: number
  failed?: number
  error?: string
  note?: string
  http_status?: number
  retry_after_ms?: number
}

export interface SyncResult {
  channel_id: number
  site_family: SiteFamily
  started_at: string
  elapsed_ms: number
  items: SyncItem[]
}

export interface GroupModelsResp {
  group_id: number
  count: number
  models: string[]
}

export interface KeyImportItem {
  channel_id: number
  account_id: number
  status: string
  error?: string
  found: number
  imported: number
  skipped: number
  failed: number
  deferred: number
}

export interface KeyImportBatchResult {
  count: number
  found: number
  imported: number
  skipped: number
  failed: number
  deferred: number
  /** 因明文读取预算耗尽而尚未处理的账号数；deferred 本身只统计 Key。 */
  deferred_accounts: number
  items: KeyImportItem[]
}

export interface KeyProvisionItem {
  channel_id: number
  account_id: number
  status: string
  error?: string
  found: number
  imported: number
  matched_groups: number
  existing_groups: number
  would_create: number
  created: number
  failed: number
  deferred: number
  skipped_reason?: string
}

export interface KeyProvisionBatchResult {
  count: number
  skipped_accounts: number
  found: number
  imported: number
  matched_groups: number
  existing_groups: number
  would_create: number
  created: number
  failed: number
  deferred: number
  items: KeyProvisionItem[]
}

export interface CatalogResp {
  channel_id: number
  total: number
  limit: number
  offset: number
  /** 当前筛选的口径。'' = 不筛；'unknown' = 空值段。 */
  unit: string
  /**
   * 各口径的条数，**在分段筛选之前**统计。
   * 分段按钮要读它而不是读 items —— 否则切进某段后其余段的按钮会消失，出不去。
   */
  units: Record<string, number>
  items: CatalogEntry[]
}

/** 某个模型在某个渠道上的一行目录（全局模型目录的展开区读它）。 */
export interface ModelChannel {
  channel_id: number
  channel_name: string
  /** 渠道状态。**停用的渠道不采集也不承接请求**，所以它有这个模型 ≠ 你能用。 */
  channel_status: string
  input_price?: number
  output_price?: number
  billing_unit?: string | null
  stale: boolean
  last_seen_at: string
  /**
   * 这个渠道下**能调到这个模型**的分组及各自的分组倍率。
   *
   * ⚠️ 上面那个 `input_price` 是模型自己的倍率，而实际计费是
   * **模型倍率 × 分组倍率** —— 分组由这把 Key 所在的分组决定。实测两个真站点
   * 的分组倍率跨度是 0.12~1.5 与 0.26~3.5，十倍以上。
   *
   * 空数组 = 还没采到分组。**不可当成"倍率 1"**：那是"不知道"，不是"不打折"。
   */
  groups: ModelGroup[]
  /** 发行方（上游 vendors[].name）。缺席 = 上游未声明，不是"无供应商"。 */
  vendor_name?: string
  /** 支持的端点类型。缺席 = 未声明，不是"不支持任何端点"。 */
  endpoint_types?: string[]
  /**
   * 该站点的额度换算基数（/api/status 的 quota_per_unit）。
   * 缺席 = 没采到，**只能显示倍率、不能折算成美元**（不许拿 500000 兜底）。
   */
  quota_per_unit?: number
}

export interface ModelGroup {
  group_ref: string
  /** 缺席 = 上游没给这个分组的倍率，同样不可当 1。 */
  rate_multiplier?: number
}

/**
 * 全局模型目录的一行：一个模型 + 有它的全部渠道。
 *
 * 三个计数**互不相减**：`channel_count` 是总数，另两个是其中的子集且可以
 * 重叠（一个渠道既可能停用又可能陈旧）。界面不要自己合成"可用渠道数" ——
 * 那需要先定义"可用"，而 P1 还没有那个定义。
 */
export interface ModelEntry {
  model_name: string
  channel_count: number
  stale_count: number
  disabled_count: number
  channels: ModelChannel[]
}

export interface GlobalCatalogResp {
  /** 筛选后的**模型数**（不是目录行数）：一个模型在 30 个渠道上仍算一个。 */
  total: number
  /**
   * 不分段时的模型数（只受 q 影响）。
   *
   * ⚠️ **不要拿 units 各项相加代替它**：同一个模型可能在 A 站按倍率、在 B 站
   * 按次，于是它在两个分段里各算一次，相加会比实际多。
   */
  whole: number
  limit: number
  offset: number
  q: string
  unit: string
  channel_id: number[]
  vendor: string[]
  endpoint: string[]
  key_id: number[]
  /**
   * 四个分面，每个都**在除自己以外的全部筛选之下**统计。
   *
   * 排除自己那一维是关键：算进去的话，选中一个供应商之后其余供应商全变 0，
   * 于是换不了供应商，只能先清空再重选。
   *
   * 值都是**模型数**（不是目录行数）：一个在 30 个渠道都有的模型只算一个，
   * 与列表的总数同口径。
   */
  units: Record<string, number>
  vendors: Record<string, number>
  endpoints: Record<string, number>
  /** 键是渠道 id 的十进制串（JSON 对象的键只能是字符串）。 */
  channels: Record<string, number>
  /** 上面那些 id 对应的渠道名，省得界面为了画分面去翻渠道列表。 */
  channel_names: Record<string, string>
  items: ModelEntry[]
}

/**
 * all-api-hub 的 WebDAV 定时同步配置。
 *
 * **两个密码只回 has_*，永远拿不到内容**（FR-094 同源纪律）。所以界面上那两个
 * 输入框每次打开都是空的，而"空"的意思是"不改"，不是"清空"。
 */
export interface HubSyncConfig {
  webdav_url: string
  webdav_username: string
  has_webdav_password: boolean
  has_backup_password: boolean
  enabled: boolean
  interval_minutes: number
  /** report = 只拉取比对不落库；import = 等同「正式导入」。 */
  apply_mode: 'report' | 'import'
  /**
   * 上次同步的时刻。取自同步历史最新一行 —— 配置里不存第二份，
   * 存了迟早与历史表分叉（定时器按一个时间走、界面显示另一个）。
   * 详情与历史看 `HubSyncRun`。
   */
  last_run_at?: string
}

/**
 * 一轮同步的记录。
 *
 * **列表里的 `result` 不带 `items`**（服务端 `result - 'items'` 剥掉了）：
 * 几十行逐站明细一起回等于把上兆 JSON 塞进一个列表响应。要明细就单独取一条。
 */
export interface HubSyncRun {
  id: number
  started_at: string
  finished_at: string
  /** 这轮跑了多久。服务端算好，省得每个调用方各减一遍。 */
  elapsed_ms: number
  /** schedule = 定时器自己醒来跑的；manual = 人点的。 */
  trigger: 'schedule' | 'manual'
  /** 这轮到底落没落库。report 模式下的数字不代表建出了渠道。 */
  applied: boolean
  /** 非空 = 这轮失败，也是界面上唯一能看见"为什么一直没同步"的地方。 */
  error?: string
  result?: Omit<HubImportResult, 'items'> & { items?: HubImportItem[] }
}

/**
 * 建渠道时的探测结果。注意这是 createChannel 里**手工拼的 map**，
 * 不是 collector.DetectResult 的序列化（后者没有 json tag），字段名只有这四个。
 */
export interface DetectedInfo {
  family: SiteFamily
  version?: string
  /** 为 false 表示疑似开了 turnstile，服务端自动采集不可行（04 §6）。 */
  no_shield: boolean
  /** 额度归一的必需输入，缺了采集会失败。 */
  quota_per_unit?: number
}

/** POST /admin/channels 的 201 响应。detected 只在 auto_detect 且探测成功时出现。 */
export interface CreateChannelResp {
  id: number
  name: string
  site_family: string
  detected?: DetectedInfo
  /** 开盾提示。 */
  warning?: string
}

/** 所有 4xx/5xx 都带这个字段；同步接口的失败体另外附带 items 等字段，
 *  由 stores/channels.ts 的 itemsOf() 按形态取，不另立类型。 */
export interface ErrorResp {
  error: string
}

// ── all-api-hub 导入 ─────────────────────────────────────────────────────────

/** 一个站点的导入结果。 */
export interface HubImportItem {
  site_name: string
  site_url: string
  /** 导出侧的声明；与 detected_family 不一致时**以探测为准**。 */
  declared_family: string
  detected_family?: SiteFamily
  family_mismatch?: boolean
  channel_id?: number
  /** imported | would_import（dry_run）| skipped | failed。 */
  status: string
  reason?: string
  /** 可用但需注意：开盾站点、导出里没凭证。 */
  warning?: string
  keys_found?: number
  keys_imported?: number
  keys_skipped?: number
  keys_failed?: number
  keys_deferred?: number
}

/** 整次导入的汇总。 */
export interface HubImportResult {
  total: number
  imported: number
  skipped: number
  failed: number
  family_mismatches: number
  shielded_sites: number
  without_credential: number
  keys_found: number
  keys_imported: number
  keys_skipped: number
  keys_failed: number
  keys_deferred: number
  items: HubImportItem[]
}
