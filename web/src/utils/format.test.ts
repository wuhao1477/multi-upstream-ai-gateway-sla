/**
 * priceRanges：卡片上那行价格区间。
 *
 * 这一段的每种改坏法都**看起来正常**：
 *  · 不分口径直接取 min/max → 一个 $0.08/次 的模型显示成"最低 0.08"，而同一行
 *    还有倍率 0.5 的渠道。两者数值区间重叠（实测按次 0.004~7 vs 倍率 0.01~175），
 *    所以错得没有任何视觉提示，而读者据此选型必然选错。
 *  · 把没采到价格的渠道当 0 → "最低 0" = 免费，而真相是"不知道"。
 */
import { effectivePrices, priceRanges } from './format'

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

/**
 * effectivePrices：模型倍率 × 分组倍率。
 *
 * 实测 2026-09-14 两个真站点的分组倍率跨度：0.26~3.5 与 0.12~1.5。
 * 同一个模型换个分组差十几倍，而目录上那个裸倍率一动不动 —— 这就是这段存在
 * 的理由，也是它写错时最贵的一种错：数字看起来完全正常，只是与账单无关。
 */
function testMultipliesByGroupRate(): void {
  const r = effectivePrices({
    input_price: 2.5,
    output_price: 12.5,
    groups: [
      { group_ref: 'default', rate_multiplier: 1 },
      { group_ref: '通用大模型', rate_multiplier: 0.26 },
      { group_ref: '测试', rate_multiplier: 3.5 },
    ],
  })
  assert(r.length === 3, `三个分组应给三条，实际 ${r.length}`)
  // 升序：最便宜的在最前，选型时先看见它
  assert(r[0]?.groupRef === '通用大模型', `应按实际价升序，首条实际是 ${r[0]?.groupRef}`)
  assert(r[0]?.input === 0.65, `2.5 × 0.26 = 0.65，实际 ${r[0]?.input}`)
  assert(r[0]?.output === 3.25, `12.5 × 0.26 = 3.25，实际 ${r[0]?.output}`)
  assert(r[2]?.input === 8.75, `2.5 × 3.5 = 8.75，实际 ${r[2]?.input}`)
  // 反向哨兵：漏乘分组倍率的话，每一条都会等于模型自己那个 2.5
  assert(!r.every((x) => x.input === 2.5), '每条都等于模型裸倍率 —— 分组倍率没乘上去')
}

function testMissingRateIsSkippedNotTreatedAsOne(): void {
  const r = effectivePrices({
    input_price: 2,
    groups: [{ group_ref: 'a' }, { group_ref: 'b', rate_multiplier: 0.5 }],
  })
  assert(r.length === 1 && r[0]?.groupRef === 'b', `没采到倍率的分组要跳过，实际 ${JSON.stringify(r)}`)
  assert(
    !r.some((x) => x.input === 2),
    '没采到倍率的分组被当成了 1 —— 那是把"不知道"显示成"不打折"',
  )
  assert(effectivePrices({ groups: [{ group_ref: 'a', rate_multiplier: 1 }] }).length === 0,
    '模型没采到价时不该乘出一个价来')
  assert(effectivePrices({ input_price: 1 }).length === 0, '没有分组时应为空，界面据此显示未知')
}

function testNoFloatTail(): void {
  const r = effectivePrices({ input_price: 0.3, groups: [{ group_ref: 'g', rate_multiplier: 1.1 }] })
  assert(r[0]?.input === 0.33, `0.3 × 1.1 应显示成 0.33 而不是浮点尾巴，实际 ${r[0]?.input}`)
}

testGroupsByUnitNeverAcrossIt()
testUncollectedPriceIsNotZero()
testMissingUnitIsItsOwnGroup()
testMultipliesByGroupRate()
testMissingRateIsSkippedNotTreatedAsOne()
testNoFloatTail()
