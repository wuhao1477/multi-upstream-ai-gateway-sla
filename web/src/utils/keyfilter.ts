/**
 * Key 筛选谓词。
 *
 * 抽出来是因为**同一套条件要作用在两种视图上**（平铺 / 按渠道或账号分组）。
 * 视图各自实现筛选的话，两边迟早会分叉，而用户切一下视图就发现"少了几把"
 * —— 那时候没人分得清是筛选变了还是数据变了。
 *
 * 全部在前端算。当前规模是数百把 Key，一次全量拉取（stores/resources）之后
 * 筛选是纯内存操作，比每改一个下拉就打一次后端要快。
 * ponytail: 到几千把 Key 时把这套条件搬到 `/admin/keys` 的 query 上。
 */
import type { Account, Key } from '@/api/types'
import { quotaView } from './money'

/** 有效期快到期的判定窗口。 */
const EXPIRING_DAYS = 7

/**
 * Key 表的可选列。默认不显示，由「列设置」逐项打开。
 *
 * 判据是"排查时才需要"：external_ref 只在采集写不回用量时才要看，
 * 倍率只在算成本时才要看。默认全塞进表里的结果是每一列都变得不显眼。
 */
export const KEY_OPTIONAL_COLS = [
  { key: 'ref', label: '上游标识' },
  { key: 'rate', label: '倍率' },
  { key: 'expiry', label: '有效期' },
  { key: 'created', label: '登记时间' },
] as const

export type QuotaFilter = 'all' | 'low' | 'exhausted' | 'unlimited' | 'unknown'
export type ExpiryFilter = 'all' | 'expiring' | 'expired'

export interface KeyFilter {
  /** 关键字。匹配前缀、上游标识、分组名 —— **不匹配明文**（我们本来也拿不到）。 */
  q: string
  /**
   * 渠道 ID 集合；**空数组 = 全部**。
   *
   * 原来是单个 `channelID`，用 0 表示"全部" —— 换成数组是因为选择器支持多选
   * （ScopePicker）。刻意不留"0 表示全部"那个约定：空数组自己就说明了一切，
   * 而 0 这个哨兵值得在每个读它的地方复述一遍含义。
   */
  channelIDs: number[]
  /** 账号 ID 集合；空数组 = 全部。 */
  accountIDs: number[]
  /** 状态；'' = 全部。 */
  status: string
  /** 分组名；'' = 全部。'__none__' = 未归组。 */
  groupRef: string
  quota: QuotaFilter
  expiry: ExpiryFilter
  /** 只看有上游限流登记的。 */
  hasRateLimit: boolean
}

export function emptyKeyFilter(): KeyFilter {
  return {
    q: '',
    channelIDs: [],
    accountIDs: [],
    status: '',
    groupRef: '',
    quota: 'all',
    expiry: 'all',
    hasRateLimit: false,
  }
}

export function isFilterActive(f: KeyFilter): boolean {
  return (
    f.q.trim() !== '' ||
    f.channelIDs.length > 0 ||
    f.accountIDs.length > 0 ||
    f.status !== '' ||
    f.groupRef !== '' ||
    f.quota !== 'all' ||
    f.expiry !== 'all' ||
    f.hasRateLimit
  )
}

function matchExpiry(k: Key, mode: ExpiryFilter): boolean {
  if (mode === 'all') return true
  // 没有 expired_time 就是"永不过期"（NewAPI 用 -1 表示，采集侧已归一为缺席）。
  // 它既不算"即将过期"也不算"已过期" —— 两个筛选都该把它排除掉。
  if (k.expired_time === undefined || k.expired_time === '') return false
  const at = new Date(k.expired_time).getTime()
  if (Number.isNaN(at)) return false
  const now = Date.now()
  if (mode === 'expired') return at <= now
  return at > now && at - now <= EXPIRING_DAYS * 86_400_000
}

function matchQuota(k: Key, mode: QuotaFilter): boolean {
  if (mode === 'all') return true
  const kind = quotaView(k).kind
  if (mode === 'unlimited') return kind === 'unlimited'
  if (mode === 'unknown') return kind === 'never'
  if (mode === 'exhausted') return kind === 'exhausted'
  // 'low' 是"接近耗尽"，**包含已耗尽** —— 排查"哪些 Key 快不能用了"时，
  // 已经不能用的那些当然也要看到。
  return kind === 'low' || kind === 'exhausted'
}

export function matchKey(k: Key, f: KeyFilter, accountLabel: (id: number) => string): boolean {
  if (f.channelIDs.length > 0 && !f.channelIDs.includes(k.channel_id)) return false
  if (f.accountIDs.length > 0 && !f.accountIDs.includes(k.account_id)) return false
  if (f.status !== '' && k.status !== f.status) return false
  if (f.groupRef !== '') {
    const g = k.group_ref ?? ''
    if (f.groupRef === '__none__' ? g !== '' : g !== f.groupRef) return false
  }
  if (!matchQuota(k, f.quota)) return false
  if (!matchExpiry(k, f.expiry)) return false
  if (f.hasRateLimit && k.rpm_limit === undefined && k.concurrency_limit === undefined) return false
  const q = f.q.trim().toLowerCase()
  if (q !== '') {
    const hay = [
      k.secret_prefix,
      k.external_ref ?? '',
      k.group_ref ?? '',
      accountLabel(k.account_id),
      String(k.id),
    ]
      .join(' ')
      .toLowerCase()
    if (!hay.includes(q)) return false
  }
  return true
}

export function filterKeys(
  keys: Key[],
  f: KeyFilter,
  accountLabel: (id: number) => string,
): Key[] {
  if (!isFilterActive(f)) return keys
  return keys.filter((k) => matchKey(k, f, accountLabel))
}

// ── 账号筛选（账号页用，条件比 Key 少得多）────────────────────────────────

export interface AccountFilter {
  q: string
  channelID: number
  status: string
  /** 'all' | 'known'（已采到余额）| 'unknown'（未采集）| 'risk'（状态非 normal）。 */
  balance: 'all' | 'known' | 'unknown' | 'risk'
}

export function emptyAccountFilter(): AccountFilter {
  return { q: '', channelID: 0, status: '', balance: 'all' }
}

export function isAccountFilterActive(f: AccountFilter): boolean {
  return f.q.trim() !== '' || f.channelID !== 0 || f.status !== '' || f.balance !== 'all'
}

export function filterAccounts(accounts: Account[], f: AccountFilter): Account[] {
  if (!isAccountFilterActive(f)) return accounts
  const q = f.q.trim().toLowerCase()
  return accounts.filter((a) => {
    if (f.channelID !== 0 && a.channel_id !== f.channelID) return false
    if (f.status !== '' && a.status !== f.status) return false
    if (f.balance === 'known' && a.balance_usd === undefined) return false
    if (f.balance === 'unknown' && a.balance_usd !== undefined) return false
    if (f.balance === 'risk') {
      const s = a.balance_state ?? ''
      if (s === '' || s === 'normal') return false
    }
    if (q !== '') {
      const hay = [String(a.id), a.external_user_id ?? '', a.balance_group_key ?? '']
        .join(' ')
        .toLowerCase()
      if (!hay.includes(q)) return false
    }
    return true
  })
}

/**
 * 通用分组：把已筛选的项按某个键分桶，并保留**该桶的总数**。
 *
 * 保留总数是分组视图的关键：只显示"匹配 3 条"会让人以为其它 Key 消失了。
 * 分组头写「匹配 3 / 全部 26」才能让人确信是筛选生效，不是数据丢了。
 */
export interface Bucket<T> {
  key: string
  /** 排序与跳转用的原始 id（渠道 id / 账号 id）；无归属时为 0。 */
  id: number
  label: string
  matched: T[]
  total: number
}

export function bucketize<T>(
  all: T[],
  matched: T[],
  idOf: (x: T) => number,
  labelOf: (id: number) => string,
): Bucket<T>[] {
  const totals = new Map<number, number>()
  for (const x of all) {
    const id = idOf(x)
    totals.set(id, (totals.get(id) ?? 0) + 1)
  }
  const buckets = new Map<number, Bucket<T>>()
  for (const x of matched) {
    const id = idOf(x)
    let b = buckets.get(id)
    if (b === undefined) {
      b = { key: String(id), id, label: labelOf(id), matched: [], total: totals.get(id) ?? 0 }
      buckets.set(id, b)
    }
    b.matched.push(x)
  }
  // 只返回有命中的桶（架构师建议：分组只展示包含匹配结果的渠道），
  // 按 id 升序 —— 与列表页顺序一致，来回切视图时位置不会跳。
  return [...buckets.values()].sort((x, y) => x.id - y.id)
}
