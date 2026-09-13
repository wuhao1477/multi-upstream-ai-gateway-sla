/**
 * /admin/* 各端点的类型化入口。
 *
 * 组件与 store 只调这里的函数，不直接拼 URL —— 查询参数名（channel_id / unit / q…）
 * 集中在一处，改后端契约时只有这个文件会红。
 */
import { api } from './client'
import type {
  Account,
  CatalogResp,
  Channel,
  ChannelGroup,
  CreateChannelResp,
  GroupModelsResp,
  HubImportResult,
  HubSyncConfig,
  InventoryResp,
  Key,
  KeyImportBatchResult,
  KeyProvisionBatchResult,
  ListResp,
  SiteFamilyInfo,
  SyncResult,
} from './types'

// ── 站型注册表 ───────────────────────────────────────────────────────────────

/**
 * 已注册的站型。界面的站型下拉读它而不是写死四个 option ——
 * 写死的那份在加站型时不会报任何错，新站型只是**在界面上不存在**。
 */
export function listSiteFamilies(): Promise<ListResp<SiteFamilyInfo>> {
  return api<ListResp<SiteFamilyInfo>>('/admin/site-families')
}

// ── 渠道 ─────────────────────────────────────────────────────────────────────

export function listChannels(): Promise<ListResp<Channel>> {
  return api<ListResp<Channel>>('/admin/channels')
}

export interface CreateChannelInput {
  name: string
  base_url: string
  site_family?: string
  /** 为真时先探测站型；探测失败不阻止建渠道（站型可以后补）。 */
  auto_detect?: boolean
}

export function createChannel(input: CreateChannelInput): Promise<CreateChannelResp> {
  return api<CreateChannelResp>('/admin/channels', { method: 'POST', body: input })
}

/**
 * 改渠道。省略的字段一律不动（服务端 `COALESCE(NULLIF(...))`）。
 *
 * **`site_family` 故意不在这里**：它由 Detect 判定，手改会让整套字段映射错位
 * （站型决定用哪个适配器、带哪个用户 ID 头、怎么解析分页信封）。要改站型
 * 就重新探测。
 *
 * `status='disabled'` 时 `disabled_reason` 必填（FR-095），服务端会 400。
 */
export interface PatchChannelInput {
  name?: string
  base_url?: string
  status?: 'enabled' | 'disabled'
  disabled_reason?: string
  disabled_until?: string | null
}

export function patchChannel(id: number, input: PatchChannelInput): Promise<{ updated: boolean }> {
  return api<{ updated: boolean }>(`/admin/channels/${id}`, { method: 'PATCH', body: input })
}

export function channelInventory(id: number): Promise<InventoryResp> {
  return api<InventoryResp>(`/admin/channels/${id}/inventory`)
}

/** 触发一次采集。服务端整体给 5 分钟，所以这里不设超时。 */
export function syncChannel(id: number): Promise<SyncResult> {
  return api<SyncResult>(`/admin/channels/${id}/sync`, { method: 'POST' })
}

// ── 模型目录 ─────────────────────────────────────────────────────────────────

export interface CatalogQuery {
  /** 只看疑似下架。 */
  stale?: boolean
  /** 模型名关键字。**必须走服务端**：分段后目标行常在第一页之外。 */
  q?: string
  /** 计价口径。''=不筛；'unknown'=空值段（billing_unit IS NULL）。 */
  unit?: string
  limit?: number
  offset?: number
}

export function channelCatalog(id: number, q: CatalogQuery = {}): Promise<CatalogResp> {
  const sp = new URLSearchParams()
  if (q.stale === true) sp.set('stale', 'true')
  if (q.q !== undefined && q.q !== '') sp.set('q', q.q)
  // unit 允许为 'unknown'，但空串要省掉：空串在 query 里与"未筛选"无从区分
  if (q.unit !== undefined && q.unit !== '') sp.set('unit', q.unit)
  if (q.limit !== undefined) sp.set('limit', String(q.limit))
  if (q.offset !== undefined) sp.set('offset', String(q.offset))
  const qs = sp.toString()
  return api<CatalogResp>(`/admin/channels/${id}/catalog${qs === '' ? '' : `?${qs}`}`)
}

// ── 账号 / Key ───────────────────────────────────────────────────────────────

export function listAccounts(channelID?: number): Promise<ListResp<Account>> {
  const qs = channelID === undefined ? '' : `?channel_id=${channelID}`
  return api<ListResp<Account>>(`/admin/accounts${qs}`)
}

export interface CreateAccountInput {
  channel_id: number
  external_user_id?: string
  balance_group_key?: string
}

export function createAccount(input: CreateAccountInput): Promise<{ id: number }> {
  return api<{ id: number }>('/admin/accounts', { method: 'POST', body: input })
}

export interface PatchAccountInput {
  external_user_id?: string
  balance_group_key?: string
  status?: 'active' | 'disabled'
  disabled_reason?: string
  disabled_until?: string | null
}

export function patchAccount(id: number, input: PatchAccountInput): Promise<{ updated: boolean }> {
  return api<{ updated: boolean }>(`/admin/accounts/${id}`, { method: 'PATCH', body: input })
}

export function listKeys(channelID?: number): Promise<ListResp<Key>> {
  const qs = channelID === undefined ? '' : `?channel_id=${channelID}`
  return api<ListResp<Key>>(`/admin/keys${qs}`)
}

export interface CreateKeyInput {
  account_id: number
  secret: string
  external_ref?: string
  group_ref?: string
}

export function createKey(input: CreateKeyInput): Promise<{ id: number }> {
  return api<{ id: number }>('/admin/keys', { method: 'POST', body: input })
}

export interface PatchKeyInput {
  secret?: string
  external_ref?: string
  channel_group_id?: number | null
  group_ref?: string
  status?: 'active' | 'revoked' | 'expired' | 'insufficient_perm'
}

export function patchKey(id: number, input: PatchKeyInput): Promise<{ updated: boolean }> {
  return api<{ updated: boolean }>(`/admin/keys/${id}`, { method: 'PATCH', body: input })
}

export function deleteKey(id: number): Promise<{ deleted: boolean }> {
  return api<{ deleted: boolean }>(`/admin/keys/${id}`, { method: 'DELETE', body: {} })
}

export function disableKey(id: number): Promise<{ status: string }> {
  return api<{ status: string }>(`/admin/keys/${id}/disable`, { method: 'POST' })
}

/**
 * Key 自动化的范围。
 *
 * 用数组而不是单个 id：界面上渠道与账号都是多选（ScopePicker）。后端仍收
 * `channel_id` / `account_id` 两个单数字段（旧契约，见 docs/dev/09 §5bis），
 * 但界面一律只发复数版 —— 两套都发的话，"到底哪个说了算"迟早会分叉。
 */
export interface KeyAutomationInput {
  channel_ids: number[]
  account_ids: number[]
  all: boolean
  model: string
  only_without_keys: boolean
}

export function importKeys(input: KeyAutomationInput): Promise<KeyImportBatchResult> {
  return api<KeyImportBatchResult>('/admin/keys/import', { method: 'POST', body: input })
}

export function provisionKeys(
  input: KeyAutomationInput,
  dryRun: boolean,
): Promise<KeyProvisionBatchResult> {
  return api<KeyProvisionBatchResult>(`/admin/keys/provision?dry_run=${dryRun}`, {
    method: 'POST',
    body: input,
  })
}

// ── 分组 ─────────────────────────────────────────────────────────────────────

export function listGroups(channelID?: number): Promise<ListResp<ChannelGroup>> {
  const qs = channelID === undefined ? '' : `?channel_id=${channelID}`
  return api<ListResp<ChannelGroup>>(`/admin/channel-groups${qs}`)
}

export function groupModels(groupID: number): Promise<GroupModelsResp> {
  return api<GroupModelsResp>(`/admin/channel-groups/${groupID}/models`)
}

// ── 采集凭证（04 §5，一期明文 FR-113）────────────────────────────────────────

export interface SaveCredentialInput {
  account_id: number
  access_token?: string
  refresh_token?: string
  external_user_id?: string
  user_id_header_name?: string
}

export function saveCredential(input: SaveCredentialInput): Promise<unknown> {
  return api<unknown>('/admin/collector/credentials', { method: 'POST', body: input })
}

// 这里原来还有一个 listCredentials()：凭证并进账号页后没有调用方了 ——
// 凭证跟着 listAccounts 的行回来（UNIQUE(account_id)）。
// 服务端 GET /admin/collector/credentials 仍在（09 §，供脚本用），只是界面不走它。

// ── all-api-hub 导入 ─────────────────────────────────────────────────────────

/**
 * 导入 all-api-hub 的导出文件。
 *
 * `raw` 是文件原文，按原样发送（后端直接从 body 解 JSON）。
 * dryRun 为真时只报探测结果不落库 —— 上百个站点的导入是不可逆操作，
 * 先看一眼再决定。服务端探测阶段给 8 分钟，这里同样不设超时。
 */
export function importHub(raw: string, dryRun: boolean): Promise<HubImportResult> {
  return api<HubImportResult>(`/admin/import/all-api-hub?dry_run=${dryRun ? 'true' : 'false'}`, {
    method: 'POST',
    body: raw,
  })
}

/** 读 all-api-hub 定时同步配置。两个密码只回 has_*，永远拿不到内容。 */
export function getHubSync(): Promise<HubSyncConfig> {
  return api<HubSyncConfig>('/admin/hub-sync')
}

/**
 * 写配置。两个密码字段**留空 = 保持原值**（服务端 COALESCE(NULLIF(…))）——
 * 界面上它们每次打开都是空的，若把空串当清空，任何一次只改间隔的保存都会顺手
 * 把密码抹掉，而症状要等下一次定时同步 401 才出现。
 */
export function saveHubSync(input: SaveHubSyncInput): Promise<HubSyncConfig> {
  return api<HubSyncConfig>('/admin/hub-sync', { method: 'PUT', body: input })
}

/** 立即跑一轮。apply 为真则强制落库，否则按配置里的 apply_mode 走。 */
export function runHubSync(apply: boolean): Promise<HubImportResult> {
  return api<HubImportResult>(`/admin/hub-sync/run?apply=${apply ? 'true' : 'false'}`, {
    method: 'POST',
  })
}

export interface SaveHubSyncInput {
  webdav_url: string
  webdav_username: string
  /** 留空 = 不改。 */
  webdav_password: string
  /** 留空 = 不改。 */
  backup_password: string
  enabled: boolean
  interval_minutes: number
  apply_mode: 'report' | 'import'
}
