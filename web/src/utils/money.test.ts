/**
 * 不限额 Key 借显账号余额的判定。
 *
 * 这一段值得钉住，是因为它的每种改坏法都**看起来正常**：
 *  · 把 `unlimited` 判定挪到 `remain<=0` 后面 → 不限额 Key 显示成「配额耗尽」
 *    （上游对它回的就是 remain_quota=0），而有限额的 Key 一切如常。
 *  · 借显时顺手把 kind 改成 'ok' → 筛选「不限额」筛不到、概览里的不限额计数归零，
 *    但表格看起来完全正确。
 *  · 忘了给 sub 赋值 → 配额列里出现一个没有注记的金额，被读成 Key 配额，
 *    而它是账号余额（两者不可加、不可比，见 money.ts 开头）。
 *  · 账号没采到余额时硬凑一个 $0 → 把"不知道"显示成"没钱了"。
 */
import type { Account, Key } from '@/api/types'
import { quotaView } from './money'

function assert(cond: boolean, message: string): void {
  if (!cond) throw new Error(message)
}

function key(over: Partial<Key>): Key {
  return { id: 1, account_id: 7, channel_id: 3, secret_prefix: 'sk-a', status: 'active', created_at: '', ...over }
}

function account(over: Partial<Account>): Account {
  return {
    id: 7,
    channel_id: 3,
    status: 'active',
    created_at: '',
    keys_total: 1,
    keys_active: 1,
    ...over,
  }
}

// 上游对无限额 Key 回 remain_quota=0，所以夹具里把 0 一起给上：
// 判定顺序写反时这条才会红。
const unlimited = key({ unlimited_quota: true, remain_quota_usd: 0 })

function testBorrowsAccountBalance(): void {
  const v = quotaView(unlimited, account({ balance_usd: 12.5, balance_state: 'normal' }))
  assert(v.text === '$12.50', `应显示账号余额 $12.50，实际 ${v.text}`)
  assert(v.sub === '账号余额', `必须带口径注记「账号余额」，实际 ${JSON.stringify(v.sub)}`)
  assert(v.kind === 'unlimited', `借显不该改 kind（筛选与合计按它归类），实际 ${v.kind}`)
  assert(v.title.includes('不是 Key 自己的配额'), '悬停解释里必须写明这不是 Key 配额')
}

// 账号余额临界/耗尽会把它名下所有不限额 Key 一起拖下水，色调得跟着账号走。
function testInheritsAccountTone(): void {
  const v = quotaView(unlimited, account({ balance_usd: 0.3, balance_state: 'critical' }))
  assert(v.tone === 'warn', `临界账号的不限额 Key 应标黄，实际 ${JSON.stringify(v.tone)}`)
}

function testUnknownBalanceStaysUnlimitedText(): void {
  for (const [label, a] of [
    ['账号余额从未采到', account({})],
    ['没传账号', undefined],
  ] as [string, Account | undefined][]) {
    const v = quotaView(unlimited, a)
    assert(v.text === '不限额度', `${label}时应退回「不限额度」而不是编一个金额，实际 ${v.text}`)
    assert(v.sub === '', `${label}时不该有口径注记，实际 ${JSON.stringify(v.sub)}`)
    assert(v.kind === 'unlimited', `${label}时 kind 仍应是 unlimited，实际 ${v.kind}`)
  }
}

// 反向哨兵：把借显写成"对所有 Key 都显示账号余额"，上面三条照样全绿。
function testLimitedKeyIgnoresAccountBalance(): void {
  const a = account({ balance_usd: 12.5, balance_state: 'normal' })
  const v = quotaView(key({ remain_quota_usd: 4, used_quota_usd: 1 }), a)
  assert(v.text === '$4.00', `有限额的 Key 必须显示自己的剩余配额，实际 ${v.text}`)
  assert(v.sub === '', `有限额的 Key 不该有口径注记，实际 ${JSON.stringify(v.sub)}`)
  const gone = quotaView(key({ remain_quota_usd: 0, used_quota_usd: 5 }), a)
  assert(gone.kind === 'exhausted', `配额耗尽不该被账号余额盖过去，实际 ${gone.kind}`)
}

testBorrowsAccountBalance()
testInheritsAccountTone()
testUnknownBalanceStaysUnlimitedText()
testLimitedKeyIgnoresAccountBalance()
