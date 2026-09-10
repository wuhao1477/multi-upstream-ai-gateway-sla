/**
 * 资金口径的**唯一**真相源。
 *
 * 界面上曾经有三个词混着用：「余额」「额度」「剩余」。它们指的其实是三件
 * 互不可加的东西 ——
 *
 *   · **账号余额**（upstream_accounts ← balance_signals）：钱包里的钱。
 *     多个账号可能共享同一个钱包（balance_group_key，FR-022），汇总要去重。
 *   · **Key 剩余配额**（upstream_keys.remain_quota_usd）：这把 Key 在上游被
 *     允许再花多少。它**不是钱**，把多把 Key 的配额相加当账号余额是错的。
 *   · **上游限流**（rpm_limit / concurrency_limit）：速率约束，与金额无关。
 *
 * 三者的关系没有蕴含：账号有余额 ≠ Key 可调用；Key 配额没用尽 ≠ 账号有钱。
 * 所以这里只提供各自的口径，**不提供任何"最终可用量"** —— P1 算不出那个
 * 数，给一个看起来精确的合成值比不给更糟。
 *
 * 一期一律美元、汇率 1:1（FR-018），故没有币种参数。
 */
import type { Account, Key } from '@/api/types'

/** 美元金额。固定两位 —— 台账要能竖着对齐着扫。 */
export function usd(v: number): string {
  return `$${v.toFixed(2)}`
}

/**
 * 小额也要看得见的美元。
 *
 * Key 配额常常是 $0.0032 这种量级（NewAPI 的 quota 归一后），两位小数会
 * 全部塌成 $0.00 —— 于是"还剩一点"和"确实是零"在界面上一模一样，
 * 而这两者对"能不能承接请求"是相反的结论（FR-025）。
 */
export function usdFine(v: number): string {
  if (v !== 0 && Math.abs(v) < 0.01) return `$${v.toFixed(4)}`
  return `$${v.toFixed(2)}`
}

// ── 账号余额 ────────────────────────────────────────────────────────────────

export type BalanceKind = 'known' | 'never' | 'abnormal'

export interface BalanceView {
  kind: BalanceKind
  /** kind==='known' 时是金额文案，否则是原因文案。 */
  text: string
  /** 状态徽标类：''|ok|warn|bad。 */
  tone: '' | 'ok' | 'warn' | 'bad'
  /** 鼠标悬停的完整解释。 */
  title: string
}

const STATE_LABEL: Record<string, string> = {
  normal: '正常',
  critical: '余额临界',
  exhausted: '余额耗尽',
  abnormal: '余额异常',
  unknown: '余额未知',
}

/**
 * 把账号的余额三元组（金额 / 状态 / 确认时刻）压成一个可直接渲染的对象。
 *
 * 判定顺序是刻意的：**先看有没有金额，再看状态**。反过来的话，
 * `balance_state='unknown'` 但确实采到过金额的账号会被显示成"未采集"，
 * 而那个金额是 FR-026 保守下限的唯一输入。
 */
export function balanceView(a: Account): BalanceView {
  if (a.balance_usd === undefined) {
    return {
      kind: 'never',
      text: '未采集',
      tone: '',
      title:
        '从未采到过该账号余额。可能是站型不提供余额接口、采集凭证缺失或从未跑过采集 —— 不是余额为 0。',
    }
  }
  const amount = usd(a.balance_usd)
  const state = a.balance_state ?? ''
  if (state !== '' && state !== 'normal') {
    return {
      kind: 'abnormal',
      text: amount,
      tone: state === 'critical' ? 'warn' : 'bad',
      title: `最近确认余额 ${amount}，当前状态：${STATE_LABEL[state] ?? state}`,
    }
  }
  return {
    kind: 'known',
    text: amount,
    tone: '',
    title: `最近一次确认的账号余额 ${amount}（来源：自动采集）`,
  }
}

/** 余额状态的中文短名，给徽标用。空串表示没有信号，不渲染徽标。 */
export function balanceStateLabel(state: string | undefined): string {
  if (state === undefined || state === '' || state === 'normal') return ''
  return STATE_LABEL[state] ?? state
}

/**
 * 按共享钱包去重后的账号余额合计（FR-022/AC-04）。
 *
 * 分组键是 balance_group_key；没有它的账号视为独立钱包，用 `acct:<id>` 兜底。
 * 同组只取**第一个采到金额的账号**——同组账号照定义看到的是同一个数，
 * 相加就会把一份钱算成 N 份。
 *
 * 返回值里 unknown 是"没采到余额的账号数"，必须与金额分开展示：
 * 把它们当 0 计入合计，会得出一个偏低且看起来精确的总额。
 */
export interface BalanceTotal {
  /** 已知余额的合计（已按钱包去重）。 */
  total: number
  /** 参与合计的钱包数。 */
  wallets: number
  /** 余额未采到的账号数。 */
  unknown: number
}

export function totalBalance(accounts: Account[]): BalanceTotal {
  const seen = new Map<string, number>()
  let unknown = 0
  for (const a of accounts) {
    if (a.balance_usd === undefined) {
      unknown += 1
      continue
    }
    const wallet =
      a.balance_group_key !== undefined && a.balance_group_key !== ''
        ? `grp:${a.balance_group_key}`
        : `acct:${a.id}`
    if (!seen.has(wallet)) seen.set(wallet, a.balance_usd)
  }
  let total = 0
  for (const v of seen.values()) total += v
  return { total, wallets: seen.size, unknown }
}

/** 该账号是否与别的账号共用同一个钱包（用于在行上打共享标记）。 */
export function sharesWallet(a: Account, all: Account[]): boolean {
  const g = a.balance_group_key
  if (g === undefined || g === '') return false
  return all.some((o) => o.id !== a.id && o.balance_group_key === g)
}

// ── Key 配额 ────────────────────────────────────────────────────────────────

export type QuotaKind = 'unlimited' | 'never' | 'exhausted' | 'low' | 'ok'

export interface QuotaView {
  kind: QuotaKind
  text: string
  tone: '' | 'ok' | 'warn' | 'bad'
  title: string
  /** 已用/总额 的比例，只有在能算出总额时才有值（用于进度条）。 */
  ratio: number | null
}

/** 剩余配额低于这个值就标黄。绝对阈值而非比例：多数 Key 采不到总额，比例算不出来。 */
export const LOW_QUOTA_USD = 1

/**
 * Key 配额的展示口径。
 *
 * ⚠️ **判定顺序不可调换**：unlimited 必须在 remain===0 之前判。
 * NewAPI 对无限额 Key 回 `remain_quota: 0`，先判 0 的话它会被显示成
 * 「额度耗尽」，而它其实是最不该被排除的那种 Key。
 *
 * 「未设独立限额」也**不等于无限可用** —— 它只是说这把 Key 自己没有约束，
 * 账号余额仍然管着它。文案里写清楚，别让人读成"随便用"。
 */
export function quotaView(k: Key): QuotaView {
  if (k.unlimited_quota === true) {
    return {
      kind: 'unlimited',
      text: '不限额度',
      tone: '',
      ratio: null,
      title:
        '上游声明该 Key 不限额度。注意：不限额度 ≠ 无限可用 —— 它仍受所属账号余额约束。',
    }
  }
  if (k.remain_quota_usd === undefined) {
    return {
      kind: 'never',
      text: '未采集',
      tone: '',
      ratio: null,
      title: '尚未采到该 Key 的剩余配额。跑一次采集，或检查该站型是否提供 Key 额度接口。',
    }
  }
  const remain = k.remain_quota_usd
  const used = k.used_quota_usd
  const limit = used === undefined ? null : remain + used
  const ratio = limit !== null && limit > 0 ? used! / limit : null
  const detail =
    used === undefined
      ? `剩余 ${usdFine(remain)}（未采到已用额度，无法给出总额）`
      : `剩余 ${usdFine(remain)} · 已用 ${usdFine(used)} · 总额 ${usdFine(limit!)}`
  if (remain <= 0) {
    return {
      kind: 'exhausted',
      text: '配额耗尽',
      tone: 'bad',
      ratio,
      title: `${detail}。配额耗尽的 Key 不能承接新请求（FR-025）。`,
    }
  }
  if (remain < LOW_QUOTA_USD) {
    return {
      kind: 'low',
      text: usdFine(remain),
      tone: 'warn',
      ratio,
      title: `${detail}。剩余低于 $${LOW_QUOTA_USD}，接近耗尽。`,
    }
  }
  return { kind: 'ok', text: usdFine(remain), tone: '', ratio, title: detail }
}

/**
 * 一组 Key 的配额概览。
 *
 * **只对"采到了剩余配额且非无限额"的 Key 求和**，其余三类各自计数、
 * 不混进金额 —— 把未采集当 0、把无限额当 0，得到的合计会低得离谱且看不出来。
 */
export interface QuotaTotal {
  /** 剩余配额合计（只含已采到且有限额的 Key）。 */
  remain: number
  /** 参与合计的 Key 数。 */
  counted: number
  /** 其中最小的剩余配额；null = 没有可计入的 Key。看风险用它，不看合计。 */
  min: number | null
  unlimited: number
  unknown: number
  exhausted: number
  low: number
}

export function totalQuota(keys: Key[]): QuotaTotal {
  const t: QuotaTotal = {
    remain: 0,
    counted: 0,
    min: null,
    unlimited: 0,
    unknown: 0,
    exhausted: 0,
    low: 0,
  }
  for (const k of keys) {
    const v = quotaView(k)
    if (v.kind === 'unlimited') {
      t.unlimited += 1
      continue
    }
    if (v.kind === 'never') {
      t.unknown += 1
      continue
    }
    const remain = k.remain_quota_usd!
    t.remain += remain
    t.counted += 1
    if (t.min === null || remain < t.min) t.min = remain
    if (v.kind === 'exhausted') t.exhausted += 1
    else if (v.kind === 'low') t.low += 1
  }
  return t
}

// ── 上游限流（FR-127：P1 只登记展示，不判闸）──────────────────────────────

/** 限流文案。两个都没有时返回 '—' —— 那是"该站型没暴露"，不是"没有限制"。 */
export function rateLimitText(k: Key): string {
  const parts: string[] = []
  if (k.rpm_limit !== undefined) parts.push(`${k.rpm_limit} rpm`)
  if (k.concurrency_limit !== undefined) parts.push(`${k.concurrency_limit} 并发`)
  return parts.length === 0 ? '—' : parts.join(' / ')
}

// ── Key 状态 ────────────────────────────────────────────────────────────────

/**
 * Key 状态的中文短名（upstream_keys.status 的 CHECK 枚举，FR-031）。
 *
 * 直接把库里的枚举名摊给运维是不行的：`revoked` 与 `expired` 的处置动作
 * 完全不同（前者要重新登记一把，后者要去上游续期），而英文枚举名读起来
 * 一样是"不能用了"。未知取值原样透出 —— 加了新状态而没加映射时，
 * 让它露出来比显示成"未知"更容易被发现。
 */
const KEY_STATUS_LABEL: Record<string, string> = {
  active: '可用',
  revoked: '已停用',
  expired: '已过期',
  insufficient_perm: '权限不足',
}

export function keyStatusLabel(s: string): string {
  return KEY_STATUS_LABEL[s] ?? s
}

export const RATE_LIMIT_HINT =
  '上游施加的 Key 级上限。P1 只登记与展示，不参与判闸（FR-127，执行属 P3 容量保留）。' +
  '显示「—」表示该站型未暴露此字段，不代表没有限制。'
