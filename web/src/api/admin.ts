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
  CredentialItem,
  GroupModelsResp,
  HubImportResult,
  InventoryResp,
  Key,
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

/** 列凭证。只回"有没有"，不回内容。 */
export function listCredentials(): Promise<ListResp<CredentialItem>> {
  return api<ListResp<CredentialItem>>('/admin/collector/credentials')
}

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
