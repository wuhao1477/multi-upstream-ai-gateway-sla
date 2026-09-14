/**
 * priceRanges：卡片上那行价格区间。
 *
 * 这一段的每种改坏法都**看起来正常**：
 *  · 不分口径直接取 min/max → 一个 $0.08/次 的模型显示成"最低 0.08"，而同一行
 *    还有倍率 0.5 的渠道。两者数值区间重叠（实测按次 0.004~7 vs 倍率 0.01~175），
 *    所以错得没有任何视觉提示，而读者据此选型必然选错。
 *  · 把没采到价格的渠道当 0 → "最低 0" = 免费，而真相是"不知道"。
 */
import { priceRanges } from './format'

function assert(cond: boolean, message: string): void {
  if (!cond) throw new Error(message)
}

function testGroupsByUnitNeverAcrossIt(): void {
  const r = priceRanges([
    { input_price: 0.5, billing_unit: 'per_1m_token' },
    { input_price: 5, billing_unit: 'per_1m_token' },
    { input_price: 0.08, billing_unit: 'per_call' },
  ])
  assert(r.length === 2, `两种口径应给两段区间，实际 ${r.length}`)
  const ratio = r.find((x) => x.unit === 'per_1m_token')
  const call = r.find((x) => x.unit === 'per_call')
  assert(ratio?.min === 0.5 && ratio?.max === 5, `倍率段应是 0.5–5，实际 ${JSON.stringify(ratio)}`)
  assert(call?.min === 0.08 && call?.max === 0.08, `按次段应是 0.08，实际 ${JSON.stringify(call)}`)
  // 反向哨兵：不分口径取全局 min 会得到 0.08，上面两条照样能被写成"通过"
  assert(
    ratio?.min !== 0.08,
    '倍率段的最小值不该是那个按次价格 —— 说明求 min 时跨了口径',
  )
}

function testUncollectedPriceIsNotZero(): void {
  const r = priceRanges([
    { billing_unit: 'per_1m_token' },
    { input_price: 3, billing_unit: 'per_1m_token' },
  ])
  assert(r.length === 1 && r[0]?.min === 3, `没采到价格的渠道不该当 0，实际 ${JSON.stringify(r)}`)
  assert(r[0]?.count === 1, `count 只数采到价格的渠道，实际 ${r[0]?.count}`)
  assert(priceRanges([{ billing_unit: 'per_call' }]).length === 0, '一个价格都没采到时不该给区间')
}

function testMissingUnitIsItsOwnGroup(): void {
  const r = priceRanges([
    { input_price: 1, billing_unit: null },
    { input_price: 2, billing_unit: 'per_1m_token' },
  ])
  assert(r.length === 2, '口径缺失自成一段，不能并进任何已知口径')
  assert(r.some((x) => x.unit === 'unknown'), `缺失口径应归入 unknown，实际 ${JSON.stringify(r)}`)
}

testGroupsByUnitNeverAcrossIt()
testUncollectedPriceIsNotZero()
testMissingUnitIsItsOwnGroup()
