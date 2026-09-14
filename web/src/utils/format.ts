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

/** 一个模型在某个计价口径下的输入价区间（跨渠道）。 */
export interface PriceRange {
  /** 'unknown' = billing_unit 缺失。 */
  unit: string
  min: number
  max: number
  /** 参与这段区间的渠道数（采到价格的那些）。 */
  count: number
}

/**
 * 把一个模型的逐渠道价格压成**按口径分组**的区间，给卡片用。
 *
 * ⚠️ **必须先按口径分组再取 min/max**：per_call 是每次调用的绝对美元价，
 * per_1m_token 是倍率，两者数值区间重叠（实测按次 0.004~7 vs 倍率 0.01~175）。
 * 混在一起求最小值，会让一个 $0.08/次 的模型显示成"最低 0.08"，
 * 而同一行还有倍率 0.5 的渠道 —— 读者据此选型必然选错。
 *
 * 没采到价格的渠道**不计入**（不是当 0）：0 是"免费"，缺席是"不知道"。
 * 某个口径下一个价格都没采到时，该口径不出现在返回值里。
 */
export function priceRanges(
  channels: { input_price?: number; billing_unit?: string | null }[],
): PriceRange[] {
  const by = new Map<string, PriceRange>()
  for (const c of channels) {
    if (c.input_price === undefined || c.input_price === null) continue
    const unit = c.billing_unit ?? 'unknown'
    const cur = by.get(unit)
    if (cur === undefined) {
      by.set(unit, { unit, min: c.input_price, max: c.input_price, count: 1 })
      continue
    }
    cur.min = Math.min(cur.min, c.input_price)
    cur.max = Math.max(cur.max, c.input_price)
    cur.count += 1
  }
  // 渠道数多的口径排前面：它更可能是这个模型的"常态"计价方式
  return [...by.values()].sort((a, b) => b.count - a.count)
}

/**
 * 端点类型的中文短名。
 *
 * 取值来自上游 /api/pricing 每个模型的 `supported_endpoint_types`
 * （2026-09-14 实测一个真实 NewAPI 站点的七种，括号里是实测模型数）。
 * 译名对齐上游自己的定价页，运维两边对照时不用换一套词。
 *
 * ⚠️ **不认识的值原样返回**，不要归到"其他"：站点会加新端点类型，
 * 而归进"其他"之后，界面上就再也看不出新增了什么 —— 那是静默丢信息。
 */
const ENDPOINT_LABEL: Record<string, string> = {
  openai: 'Chat', // 1364
  'openai-response': 'Response', // 8
  anthropic: 'Anthropic', // 38
  gemini: 'Gemini', // 123
  'jina-rerank': 'Rerank', // 10
  'image-generation': '图片', // 68
  'openai-video': '视频', // 17
}

export function endpointLabel(e: string): string {
  return ENDPOINT_LABEL[e] ?? e
}

/**
 * 计价类型的中文短名。
 *
 * 与 unitChip 的区别：那个说的是"这个数怎么读"（×倍率 / 每次多少钱），
 * 这个说的是"这个模型怎么计费"，是分面筛选的标签。同一份 billing_unit，
 * 两种语境两种说法 —— 混用会让分面上写着「×倍率 1188」，读起来像在筛倍率值。
 */
export function pricingTypeLabel(u: string): string {
  switch (u) {
    case 'per_call':
      return '按次计费'
    case 'per_1m_token':
    case 'per_1k_token':
    case 'per_token':
      return '按量计费'
    default:
      return '口径未知'
  }
}

/** 某个分组下这个模型的**实际**价格（已乘分组倍率）。 */
export interface EffectivePrice {
  groupRef: string
  /** 分组倍率本身。 */
  groupRate: number
  /** 模型价 × 分组倍率。 */
  input: number
  /** 输出价 × 分组倍率；模型没给输出价时为 null。 */
  output: number | null
}

/**
 * 把「模型自己的价」按分组倍率折算成**这把 Key 实际会被计的价**。
 *
 * 上游（NewAPI 系）的计费是 `模型倍率 × 分组倍率`，两个乘数都由上游给：
 * 前者在 /api/pricing 的 model_ratio / model_price，后者在顶层 group_ratio。
 * 只看前者选站，看到的数字与账单可以差一个数量级 —— 2026-09-14 实测两个真站点，
 * 分组倍率跨度分别是 0.26~3.5 与 0.12~1.5。同一个模型换个分组价格差十几倍，
 * 而目录上那个数一动不动。
 *
 * 按 input 升序：选型时先想知道的是"最便宜能到多少、要用哪个分组"。
 *
 * 两个缺席都**跳过而不是当默认值**：
 *  · 模型没采到价 → 乘出来的也是假的；
 *  · 分组没采到倍率 → 当 1 会把一个未知折扣显示成"不打折"。
 * 结果为空数组时，界面必须显示"未知"，不能显示模型自己的那个裸倍率。
 */
export function effectivePrices(c: {
  input_price?: number
  output_price?: number
  groups?: { group_ref: string; rate_multiplier?: number }[]
}): EffectivePrice[] {
  if (c.input_price === undefined || c.input_price === null) return []
  const out: EffectivePrice[] = []
  for (const g of c.groups ?? []) {
    if (g.rate_multiplier === undefined || g.rate_multiplier === null) continue
    out.push({
      groupRef: g.group_ref,
      groupRate: g.rate_multiplier,
      // 浮点相乘会留下 0.30000000000000004 这种尾巴，界面上很难看且没有意义。
      // 六位小数足够：真站点的倍率最细到 0.12，模型倍率最细到 0.000x。
      input: round6(c.input_price * g.rate_multiplier),
      output:
        c.output_price === undefined || c.output_price === null
          ? null
          : round6(c.output_price * g.rate_multiplier),
    })
  }
  return out.sort((a, b) => a.input - b.input || a.groupRef.localeCompare(b.groupRef))
}

function round6(v: number): number {
  return Math.round(v * 1e6) / 1e6
}

const FAMILY_LABEL: Record<string, string> = {
  newapi: 'NewAPI 系',
  'new-api': 'NewAPI 系',
  'one-api': 'NewAPI 系',
  sub2api: 'Sub2API 系',
  'sub2-api': 'Sub2API 系',
  unknown: '未识别',
}

const DECLARED_FAMILY_LABEL: Record<string, string> = {
  oneapi: 'OneAPI',
  newapi: 'NewAPI',
  'new-api': 'NewAPI',
  anyrouter: 'AnyRouter',
  veloera: 'Veloera',
  onehub: 'OneHub',
  donehub: 'DoneHub',
  vapi: 'VAPI',
  voapi: 'VoAPI',
  'voapi(v1/v2)': 'VoAPI（v1/v2）',
  superapi: 'SuperAPI',
  rixapi: 'RixAPI',
  neoapi: 'NeoAPI',
  wonggongyi: 'WongGongyi',
  sub2api: 'Sub2API',
  'sub2-api': 'Sub2API',
  aihubmix: 'AiHubMix',
  sharedchat: 'SharedChat',
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
  // 新枚举的原值仍保留在数据、筛选条件和 data-* 属性中；用户可见区域不直接显示英文。
  if (value === undefined || value === null || value === '') return fallback
  return labels[value] ?? fallback
}

export function familyLabel(value: string | undefined | null): string {
  return mappedLabel(value, FAMILY_LABEL, '未知站型')
}

export function declaredFamilyLabel(value: string | undefined | null): string {
  const normalized = value?.trim().toLowerCase()
  return mappedLabel(normalized, DECLARED_FAMILY_LABEL, '导出声明未识别')
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
  // 未知枚举仍保留在 data-* 属性和日志中；可见文本遵守管理界面的中文要求。
  return mappedLabel(value, KEY_STATUS_LABEL, '未知状态')
}

export function anomalyLabel(value: string | undefined | null): string {
  return mappedLabel(value, ANOMALY_LABEL, '未知异常')
}
