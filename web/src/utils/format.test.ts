/**
 * priceRanges：卡片上那行价格区间。
 *
 * 这一段的每种改坏法都**看起来正常**：
 *  · 不分口径直接取 min/max → 一个 $0.08/次 的模型显示成"最低 0.08"，而同一行
 *    还有倍率 0.5 的渠道。两者数值区间重叠（实测按次 0.004~7 vs 倍率 0.01~175），
 *    所以错得没有任何视觉提示，而读者据此选型必然选错。
 *  · 把没采到价格的渠道当 0 → "最低 0" = 免费，而真相是"不知道"。
 */
import {
  dynamicRateText,
  effectivePrices,
  isAutoGroup,
  priceRanges,
  ratioToUSDPer1M,
} from './format'

function assert(cond: boolean, message: string): void {
  if (!cond) throw new Error(message)
}

/**
 * 倍率 → 绝对美元价。
 *
 * 这一段存在的理由：倍率**跨站点不可比**。同一个"×1"，在 quota_per_unit=500000
 * 的站上是 $2/1M token，在 250000 的站上就是 $4 —— 而界面上两个都写着 ×1。
 * 折算是把它们放回同一把尺子上的唯一办法。
 *
 * 最容易错的三种写法，三条断言各钉一种：
 *  · 系数写死成 ×2（等于假定所有站都是 500000）
 *  · 没采到基数时兜底成 500000（给出一个看起来精确的错价）
 *  · 对按次计价也乘这个基数（上游那边按次根本不过这条公式）
 */
function testRatioToUSD(): void {
  // 实测两个真站点都是 500000 → ×2，对应 NewAPI 当基准的 $0.002/1K
  assert(ratioToUSDPer1M(1, 'per_1m_token', 500000) === 2, '倍率 1 @50 万基数应是 $2/1M')
  assert(ratioToUSDPer1M(4, 'per_1m_token', 500000) === 8, '倍率 4 @50 万基数应是 $8/1M')
  // 基数不同，同一个倍率就是另一个价 —— 这正是"倍率 ×1 实际却是官方 2 倍"的来源
  assert(
    ratioToUSDPer1M(1, 'per_1m_token', 250000) === 4,
    '基数 25 万时倍率 1 应是 $4/1M —— 系数写死成 ×2 的话这条会红',
  )
  // 没采到基数：不折算，**不兜底**
  assert(ratioToUSDPer1M(1, 'per_1m_token', undefined) === null, '没有基数时不该猜一个价出来')
  assert(ratioToUSDPer1M(1, 'per_1m_token', 0) === null, '基数为 0（没采到的哨兵）同样不折算')
  // 按次计价不过这条公式：美元价就是 model_price × group_ratio
  assert(ratioToUSDPer1M(0.08, 'per_call', 500000) === 0.08, '按次价不该再乘换算基数')
  assert(ratioToUSDPer1M(1, null, 500000) === null, '口径未知时不折算')
}

/** effectivePrices 要把折算结果一起带出来，否则上面那条只是个孤立的函数。 */
function testEffectivePricesCarryUSD(): void {
  const r = effectivePrices({
    input_price: 2.5,
    output_price: 12.5,
    billing_unit: 'per_1m_token',
    quota_per_unit: 500000,
    groups: [{ group_ref: 'daily-codex', rate_multiplier: 0.15 }],
  })
  assert(r[0]?.input === 0.375, `倍率应是 2.5×0.15=0.375，实际 ${r[0]?.input}`)
  assert(r[0]?.inputUSD === 0.75, `绝对价应是 0.375×2=$0.75/1M，实际 ${r[0]?.inputUSD}`)
  assert(r[0]?.outputUSD === 3.75, `输出绝对价应是 12.5×0.15×2=3.75，实际 ${r[0]?.outputUSD}`)
  const noBase = effectivePrices({
    input_price: 2.5,
    billing_unit: 'per_1m_token',
    groups: [{ group_ref: 'g', rate_multiplier: 1 }],
  })
  assert(noBase[0]?.input === 2.5, '没有基数时倍率照常算')
  assert(noBase[0]?.inputUSD === null, '没有基数时绝对价必须是 null，不是一个猜的数')
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
/**
 * auto 分组不参与计价。
 *
 * NewAPI 源码里 auto 是**选组模式**：实际计费用 auto_groups 里第一个有可用
 * 渠道的组的倍率，而 "auto" 自己被显式排除。但 /api/pricing 的 group_ratio
 * 照样给它一个倍率（实测某站是 1）—— 那是展示占位。照它算钱会报出一个
 * 与账单无关、却看起来完全正常的数字。
 */
/**
 * 动态倍率：判据是后端的 `rate_dynamic`，不是分组名。
 *
 * 名字判定（ref === 'auto'）只在 NewAPI 成立；`rate_dynamic` 是采集侧按各家族
 * 的规则标出来的，别的站型可能把这种模式叫别的名字。只认名字的话，换个站型
 * 就会把一个动态倍率当成固定倍率拿去算钱 —— 而算出来的数看起来完全正常。
 *
 * 另一半同样要钉：**没写分组的 Key 不等于动态倍率**。它走账号的默认分组，
 * 那通常是个有固定倍率的普通组，照常参与计价。
 */
function testDynamicRateComesFromFlagNotName(): void {
  // 名字不是 auto，但后端标了动态 → 必须排除
  const r = effectivePrices({
    input_price: 5,
    groups: [
      { group_ref: '智能调度', rate_multiplier: 1, rate_dynamic: true },
      { group_ref: 'default', rate_multiplier: 2 },
    ],
  })
  assert(r.length === 1 && r[0]?.groupRef === 'default',
    `动态组必须按 rate_dynamic 排除，实际 ${JSON.stringify(r.map((x) => x.groupRef))}`)

  // 反面：普通分组（含账号继承来的那种）照常参与计价
  const fixed = effectivePrices({
    input_price: 5,
    groups: [{ group_ref: 'default', rate_multiplier: 0.8 }],
  })
  assert(fixed.length === 1 && fixed[0]?.input === 4,
    `没写分组走的账号默认组是普通组，该照常算，实际 ${JSON.stringify(fixed)}`)

  // 文案：有范围给范围，没范围只说动态 —— 说不出好过编一个
  assert(dynamicRateText(0.26, 2.6) === '动态倍率 ×0.26~2.6', dynamicRateText(0.26, 2.6))
  assert(dynamicRateText(1, 1) === '动态倍率 ×1', dynamicRateText(1, 1))
  assert(dynamicRateText(undefined, undefined) === '动态倍率', '候选都没采到倍率时不该编一个范围')
  assert(dynamicRateText(0.5, undefined) === '动态倍率', '只有一半也算说不出范围')
}

function testAutoGroupIsNotPriceable(): void {
  assert(isAutoGroup('auto'), '字面量 auto 必须识别出来')
  assert(!isAutoGroup('auto-daily'), '只认精确的 auto —— 它是保留字不是一类命名')
  assert(!isAutoGroup(undefined), '缺席不是 auto')
  const r = effectivePrices({
    input_price: 5,
    billing_unit: 'per_1m_token',
    quota_per_unit: 500000,
    groups: [
      { group_ref: 'auto', rate_multiplier: 1 },
      { group_ref: 'default', rate_multiplier: 2 },
    ],
  })
  assert(r.length === 1, `auto 不该参与计价，实际给了 ${r.length} 条`)
  assert(r[0]?.groupRef === 'default', `留下的应是 default，实际 ${r[0]?.groupRef}`)
  // 反向哨兵：不排除 auto 的话，它的 ×1 会排在最前，界面就会说"最低 5"
  assert(r[0]?.input === 10, `应是 5×2=10，实际 ${r[0]?.input} —— 5 说明 auto 混进来了`)
  assert(
    effectivePrices({
      input_price: 5,
      groups: [{ group_ref: 'auto', rate_multiplier: 1 }],
    }).length === 0,
    '只有 auto 一个分组时应为空，界面据此说"按实际命中的分组计"',
  )
}

testRatioToUSD()
testEffectivePricesCarryUSD()
testAutoGroupIsNotPriceable()
testDynamicRateComesFromFlagNotName()
