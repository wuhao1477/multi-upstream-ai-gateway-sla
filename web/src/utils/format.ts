/** 时间戳格式化。缺值显式写"从未"，不留空格 —— 空白单元格分不清"没有"和"没渲染"。 */
export function fmtTime(t: string | undefined | null): string {
  if (t === undefined || t === null || t === '') return '从未'
  return new Date(t).toLocaleString('zh-CN')
}

/**
 * 相对时刻（"2 小时前"）。表格里比绝对时间好扫 —— 运维要判断的是"这个数
 * 有多旧"，而不是"它是几号采的"。绝对时间放 title，鼠标一停就能看到。
 *
 * 与 fmtTime 一样，缺值写"从未"而不是空白。
 */
export function fmtAgo(t: string | undefined | null): string {
  if (t === undefined || t === null || t === '') return '从未'
  const ms = Date.now() - new Date(t).getTime()
  if (Number.isNaN(ms)) return '从未'
  // 未来时刻（时钟偏移）不写成"-3 分钟前"，直接说刚刚
  if (ms < 60_000) return '刚刚'
  const min = Math.floor(ms / 60_000)
  if (min < 60) return `${min} 分钟前`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} 小时前`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d} 天前`
  return fmtTime(t)
}

/**
 * 计价口径的展示文案。
 *
 * 口径必须与价格同格显示：per_call 是每次调用的绝对美元价，
 * per_1m_token 是倍率，两者数值区间重叠，光看数字无法分辨。
 * 未声明时**不假定默认值** —— 返回 null，由组件渲染成带 title 的告警文案。
 */
export function unitLabel(u: string | undefined | null): string | null {
  switch (u) {
    case 'per_call':
      return '/次'
    case 'per_1m_token':
      return '×倍率'
    case 'per_1k_token':
      return '×倍率（千令牌）'
    case 'per_token':
      return '×倍率（每令牌）'
    default:
      return null
  }
}

/** 分段筛选按钮的文案。unknown 是 billing_unit IS NULL 的存量行。 */
export function unitChip(u: string): string {
  if (u === 'unknown') return '口径未知'
  return unitLabel(u) ?? '口径未知'
}

const FAMILY_LABEL: Record<string, string> = {
  newapi: 'NewAPI 系',
  'new-api': 'NewAPI 系',
  'one-api': 'NewAPI 系',
  veloera: 'NewAPI 系',
  sub2api: 'Sub2API 系',
  'sub2-api': 'Sub2API 系',
  unknown: '未识别',
}

const STATUS_LABEL: Record<string, string> = {
  enabled: '启用',
  disabled: '停用',
  active: '启用',
  revoked: '已停用',
  expired: '已过期',
  insufficient_perm: '权限不足',
  valid: '有效',
  invalid: '无效',
  ok: '成功',
  partial: '部分成功',
  failed: '失败',
  unsupported: '不支持',
  skipped: '跳过',
  imported: '已导入',
  would_import: '可导入',
  detected: '已探测',
}

const CAPABILITY_LABEL: Record<string, string> = {
  account: '账号',
  keys: '密钥',
  groups: '分组',
  pricing: '价格',
  model_catalog: '模型目录',
}

const SUPPORT_LABEL: Record<string, string> = {
  supported: '支持',
  degraded: '部分支持',
  unsupported: '不支持',
}

const CREDENTIAL_TYPE_LABEL: Record<string, string> = {
  newapi_access_token: 'NewAPI 系访问令牌',
  sub2api_jwt: 'Sub2API 系 JWT',
}

const KEY_STATUS_LABEL: Record<string, string> = {
  active: '可用',
  revoked: '已停用',
  expired: '已过期',
  insufficient_perm: '权限不足',
}

const ANOMALY_LABEL: Record<string, string> = {
  delisted_model: '疑似下架模型',
  stale_data: '数据陈旧',
  degraded_missing_fields: '价格待人工录入',
  unregistered_key: '上游有、库里没登记的密钥',
  credential_invalid: '采集凭证失效',
  credential_missing: '未登记采集凭证',
  site_family_unknown: '站型未识别',
  never_collected: '从未采集成功',
  key_unusable: '密钥已停用或过期',
}

function mappedLabel(value: string | undefined | null, labels: Record<string, string>, fallback: string): string {
  if (value === undefined || value === null || value === '') return fallback
  return labels[value] ?? fallback
}

export function familyLabel(value: string | undefined | null): string {
  return mappedLabel(value, FAMILY_LABEL, '未知站型')
}

export function statusLabel(value: string | undefined | null): string {
  return mappedLabel(value, STATUS_LABEL, '未知状态')
}

export function importStatusLabel(value: string | undefined | null): string {
  return mappedLabel(value, STATUS_LABEL, '未知结果')
}

export function capabilityLabel(value: string | undefined | null): string {
  return mappedLabel(value, CAPABILITY_LABEL, '未知能力')
}

export function supportLevelLabel(value: string | undefined | null): string {
  return mappedLabel(value, SUPPORT_LABEL, '未知支持级别')
}

export function credentialTypeLabel(value: string | undefined | null): string {
  return mappedLabel(value, CREDENTIAL_TYPE_LABEL, '未知凭证类型')
}

export function keyStatusLabel(value: string | undefined | null): string {
  return mappedLabel(value, KEY_STATUS_LABEL, '未知状态')
}

export function anomalyLabel(value: string | undefined | null): string {
  return mappedLabel(value, ANOMALY_LABEL, '未知异常')
}
