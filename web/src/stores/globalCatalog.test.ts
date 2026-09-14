import { createPinia, setActivePinia } from 'pinia'
import type { GlobalCatalogResp } from '@/api/types'
import { useGlobalCatalogStore } from './globalCatalog'

interface Deferred<T> {
  promise: Promise<T>
  resolve(value: T): void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((r) => {
    resolve = r
  })
  return { promise, resolve }
}

function resp(over: Partial<GlobalCatalogResp>): GlobalCatalogResp {
  return {
    total: 1,
    whole: 1,
    limit: 50,
    offset: 0,
    q: '',
    unit: '',
    channel_id: [],
    vendor: [],
    endpoint: [],
    units: { per_1m_token: 1 },
    vendors: {},
    endpoints: {},
    channels: {},
    channel_names: {},
    items: [
      {
        model_name: 'm',
        channel_count: 1,
        stale_count: 0,
        disabled_count: 0,
        channels: [],
      },
    ],
    ...over,
  }
}

function assertEqual<T>(actual: T, expected: T, message: string): void {
  if (actual !== expected) throw new Error(`${message}: expected ${expected}, got ${actual}`)
}

/**
 * 慢的那次响应回来时不许覆盖快的那次。
 *
 * 与 catalog.test.ts 同型，理由也相同：两个请求在途时，先发的那个可能后到，
 * 而它带回的是**上一个筛选**的结果。没有这道门的话，症状是"筛完过一秒又跳回
 * 上一批"——看起来像筛选没生效，而实际上生效了又被盖掉。
 */
async function testStaleResponseCannotReplaceCurrentFilter(): Promise<void> {
  setActivePinia(createPinia())
  const all = deferred<GlobalCatalogResp>()
  const filtered = deferred<GlobalCatalogResp>()

  globalThis.slaAdminMock = {
    globalCatalog: (q: { unit?: string }) =>
      q.unit === 'per_call' ? filtered.promise : all.promise,
  } as unknown as typeof globalThis.slaAdminMock // 本用例只会用到 globalCatalog

  const cat = useGlobalCatalogStore()
  const open = cat.open()
  cat.setUnit('per_call')

  filtered.resolve(
    resp({ unit: 'per_call', total: 1, whole: 9, items: [
      { model_name: 'new-seg-model', channel_count: 2, stale_count: 0, disabled_count: 0, channels: [] },
    ] }),
  )
  await filtered.promise
  assertEqual(cat.unit, 'per_call', '分段状态应立即变更')
  assertEqual(cat.items[0]?.model_name, 'new-seg-model', '新分段的响应应渲染')

  all.resolve(resp({ items: [
    { model_name: 'old-all-model', channel_count: 1, stale_count: 0, disabled_count: 0, channels: [] },
  ] }))
  await open
  assertEqual(cat.unit, 'per_call', '分段状态不该被过期响应改回去')
  assertEqual(cat.items[0]?.model_name, 'new-seg-model', '过期响应应被丢弃')
}

/**
 * 「全部」那一段的数读 whole，**不是 units 求和**。
 *
 * 一个模型在 A 站按倍率、在 B 站按次时，它在两个分段里各算一次 ——
 * 实测两个真站点 1397 个模型而分段相加是 1398。求和得到的数比逐段翻完能看到的
 * 多，多出来的那几个怎么翻都找不到，像是列表漏了行。
 */
async function testWholeIsNotSumOfSegments(): Promise<void> {
  setActivePinia(createPinia())
  globalThis.slaAdminMock = {
    globalCatalog: () =>
      Promise.resolve(
        resp({ whole: 2, total: 2, units: { per_1m_token: 2, per_call: 1 } }),
      ),
  } as unknown as typeof globalThis.slaAdminMock // 本用例只会用到 globalCatalog

  const cat = useGlobalCatalogStore()
  await cat.open()
  assertEqual(cat.whole, 2, 'whole 应取后端给的值')
  assertEqual(
    cat.segments.reduce((n, [, c]) => n + c, 0),
    3,
    '夹具本身要满足"分段相加 > whole"，否则这条断言验不到东西',
  )
}

await testStaleResponseCannotReplaceCurrentFilter()
await testWholeIsNotSumOfSegments()
