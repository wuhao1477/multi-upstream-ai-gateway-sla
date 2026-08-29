/**
 * 后端 /admin/* 的线上形状。逐字对着 Go 结构体的 json tag 写，不是照界面猜的。
 *
 * 两条规则贯穿全文件：
 *  - Go 的 `omitempty` + 指针 → TS 的 `?: T`（字段可能整个缺席）。
 *  - Go 的 `*T` 且语义是"上游没声明" → 允许 null，且**不许**用 `?? 默认值` 悄悄填平：
 *    价格口径 billing_unit 就是这么出过缺陷的（02 §1.3bis：nil = 未声明，按未知处理）。
 */

// ── 站型与枚举 ───────────────────────────────────────────────────────────────

/** 站点家族。ASXS 一期不纳管，但后端仍可能返回，所以类型里保留。 */
export type SiteFamily = 'newapi' | 'sub2api' | 'asxs' | 'unknown'

/** 采集能力项。 */
export type Capability =
  | 'account'
  | 'keys'
  | 'groups'
  | 'subscription_quotas'
  | 'pricing'
  | 'model_catalog'

/** 单项采集结果状态。partial 只可能出现在 keys；skipped 是被限流跳过。 */
export type ItemStatus = 'ok' | 'partial' | 'failed' | 'unsupported' | 'skipped'

/** 能力矩阵：能力 → 支持程度。 */
export type CapabilityMap = Partial<Record<Capability, string>>

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
  status: string
  created_at: string
}

export interface Key {
  id: number
  account_id: number
  channel_id: number
  /** 只有前缀，永远拿不到完整密钥（FR-094）。 */
  secret_prefix: string
  external_ref?: string
  channel_group_id?: number
  remain_quota_usd?: number
  used_quota_usd?: number
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
  status: ItemStatus
  elapsed_ms?: number
  rows?: number
  failed?: number
  error?: string
  note?: string
}

export interface SyncResult {
  channel_id: number
  site_family: SiteFamily
  started_at: string
  elapsed_ms: number
  items: SyncItem[]
}

/**
 * 同步的失败响应体。三种都可能只带一部分字段：
 *  - 429/409 限流或已在跑：带 items（单项 status='skipped'）
 *  - 422 前置条件不满足（配置问题，故意不报 502）：带 site_family + items
 *  - 502 认证/连接失败：**不带** items
 */
export interface SyncErrorResp {
  channel_id?: number
  site_family?: SiteFamily
  items?: SyncItem[]
  error?: string
}

export interface GroupModelsResp {
  group_id: number
  count: number
  models: string[]
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

export interface CredentialItem {
  channel_id: number
  site_family: SiteFamily
  cred_type: string
  status: string
  token_expires_at?: string
  updated_at: string
  /** 只报"有没有"，不报内容。 */
  has_token: boolean
  has_password: boolean
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
  /** 探测结果落库失败 —— 渠道建成了但采集会缺 quota_per_unit。 */
  warning_persist?: string
}

/** 所有 4xx/5xx 都带这个字段；同步接口另外附带 SyncErrorResp 的字段。 */
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
  items: HubImportItem[]
}
