import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import * as adminApi from '@/api/admin'
import type { GlobalCatalogResp } from '@/api/types'
import { useToastStore } from './toast'

/**
 * 跨渠道模型目录的分栏状态。
 *
 * 与 `stores/catalog.ts` 形状相近（分段 / 防抖 / 翻页 / 丢弃过期响应），
 * **刻意没有合并成一个**：两者的资源不同（一个渠道的模型 vs 一个模型的渠道），
 * 响应体与 API 入参都不一样，合并的结果是 load() 里一个贯穿始终的分支，
 * 而那条分支要走的恰恰是 catalog.test.ts 钉住的那段竞态代码 —— 为省几十行
 * 去重写一段已经被验收覆盖的逻辑，不划算。真要抽，等出现第三个。
 */

/** 每页行数的默认值与可选档位。 */
export const MODEL_PAGE = 50
export const PAGE_SIZES = [20, 50, 100] as const

const DEBOUNCE_MS = 250

/** 分面排序：数量降序。先看见的应该是"大头"，而不是名字排在前面的那个。 */
function sortedFacet(m: Record<string, number> | undefined): Array<[string, number]> {
  if (m === undefined) return []
  return Object.entries(m).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
}

/** 两个数组逐项相等。给"过期响应"那道门比对多选筛选用。 */
function same(a: readonly unknown[], b: readonly unknown[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

export const useGlobalCatalogStore = defineStore('globalCatalog', () => {
  const toast = useToastStore()

  const unit = ref('')
  const q = ref('')
  /** 三个多选筛选。空数组 = 不筛（不是"一个都不要"）。 */
  const channelIDs = ref<number[]>([])
  const vendors = ref<string[]>([])
  const endpoints = ref<string[]>([])
  /**
   * 「这几把 Key 调得到」。与上面三维不同：它筛的不是目录行的某个字段，
   * 而是 Key 所在分组的可用模型清单（group_models）。未归组的 Key 什么都
   * 匹配不到 —— 界面必须不让它被选中，否则得到的是一个无法解释的空列表。
   */
  const keyIDs = ref<number[]>([])
  /** 排序与每页行数。两者都进请求 —— 排序必须在服务端做，客户端只排当前页
   *  的话，翻到第二页看到的是"另一段里各自排好的"，整体并不有序。 */
  const sort = ref('')
  const pageSize = ref<number>(MODEL_PAGE)
  const offset = ref(0)
  const resp = ref<GlobalCatalogResp | null>(null)
  const loaded = ref(false)

  let timer: ReturnType<typeof setTimeout> | undefined

  /** 分段按钮读 units（后端在分段筛选**之前**算的），不读 items。 */
  const segments = computed<Array<[string, number]>>(() => {
    const u = resp.value?.units
    if (u === undefined) return []
    return Object.entries(u).sort((a, b) => b[1] - a[1])
  })

  /**
   * 「全部」那一段的模型数，读后端的 whole。
   *
   * ⚠️ 不要写成 segments 求和：一个模型在 A 站按倍率、在 B 站按次时，它在两个
   * 分段里各算一次（实测两个真站点 1397 个模型、分段相加 1398）。相加得到的
   * 数比逐段翻完能看到的多，而多出来的那几个找不到，像是列表漏了行。
   */
  const whole = computed(() => resp.value?.whole ?? 0)

  const items = computed(() => resp.value?.items ?? [])
  const total = computed(() => resp.value?.total ?? 0)
  const from = computed(() => (total.value === 0 ? 0 : offset.value + 1))
  const to = computed(() => offset.value + items.value.length)
  const hasPrev = computed(() => offset.value > 0)
  const hasNext = computed(() => to.value < total.value)
  const filtering = computed(
    () =>
      unit.value !== '' ||
      q.value !== '' ||
      channelIDs.value.length > 0 ||
      vendors.value.length > 0 ||
      endpoints.value.length > 0 ||
      keyIDs.value.length > 0,
  )

  /** 分面：id/名字 → 模型数。渠道那个额外带名字，见响应体注释。 */
  const vendorFacet = computed(() => sortedFacet(resp.value?.vendors))
  /** 发行方 → 图标名。分面 chip 与列表都读它，见响应体注释（不从 items 里凑）。 */
  const vendorIcons = computed<Record<string, string>>(() => resp.value?.vendor_icons ?? {})
  const endpointFacet = computed(() => sortedFacet(resp.value?.endpoints))
  const channelFacet = computed(() =>
    sortedFacet(resp.value?.channels).map(
      ([id, n]) => [Number(id), resp.value?.channel_names[id] ?? `#${id}`, n] as const,
    ),
  )

  async function load(): Promise<void> {
    // 快照**全部**筛选条件，不只是三个：下面那道"过期响应"的门要拿它逐项比对，
    // 漏一维就等于那一维不设防 —— 而它的症状（筛完过一秒跳回上一批）看起来
    // 像是筛选没生效，不像是竞态。
    const request = {
      unit: unit.value,
      q: q.value,
      offset: offset.value,
      channelIDs: [...channelIDs.value],
      vendors: [...vendors.value],
      endpoints: [...endpoints.value],
      keyIDs: [...keyIDs.value],
      sort: sort.value,
      pageSize: pageSize.value,
    }
    try {
      const data = await adminApi.globalCatalog({
        unit: request.unit,
        q: request.q,
        channelIDs: request.channelIDs,
        vendors: request.vendors,
        endpoints: request.endpoints,
        keyIDs: request.keyIDs,
        sort: request.sort,
        limit: request.pageSize,
        offset: request.offset,
      })
      if (
        unit.value === request.unit &&
        q.value === request.q &&
        offset.value === request.offset &&
        same(channelIDs.value, request.channelIDs) &&
        same(vendors.value, request.vendors) &&
        same(endpoints.value, request.endpoints) &&
        same(keyIDs.value, request.keyIDs) &&
        sort.value === request.sort &&
        pageSize.value === request.pageSize
      ) {
        resp.value = data
        loaded.value = true
      }
    } catch (e) {
      toast.fail('加载模型目录失败', e)
    }
  }

  /** 切某个多选项。改完回第一页 —— 筛完还停在第 7 页多半是空页。 */
  function toggleIn<T>(list: { value: T[] }, v: T): void {
    const i = list.value.indexOf(v)
    list.value = i < 0 ? [...list.value, v] : list.value.filter((x) => x !== v)
    offset.value = 0
    void load()
  }

  function toggleChannel(id: number): void {
    toggleIn(channelIDs, id)
  }
  function toggleVendor(v: string): void {
    toggleIn(vendors, v)
  }
  function toggleEndpoint(e: string): void {
    toggleIn(endpoints, e)
  }
  function toggleKey(id: number): void {
    toggleIn(keyIDs, id)
  }

  /** 清掉全部筛选（含筛选框）。分面上的「全部」按钮与「重置」都走它。 */
  function clearFilters(): void {
    unit.value = ''
    q.value = ''
    channelIDs.value = []
    vendors.value = []
    endpoints.value = []
    keyIDs.value = []
    offset.value = 0
    void load()
  }

  /** 从 URL 恢复。一次性设好再拉，避免每维各打一次请求。 */
  function setFilters(f: {
    q?: string
    unit?: string
    channelIDs?: number[]
    vendors?: string[]
    endpoints?: string[]
    keyIDs?: number[]
  }): void {
    if (f.q !== undefined) q.value = f.q
    if (f.unit !== undefined) unit.value = f.unit
    if (f.channelIDs !== undefined) channelIDs.value = f.channelIDs
    if (f.vendors !== undefined) vendors.value = f.vendors
    if (f.endpoints !== undefined) endpoints.value = f.endpoints
    if (f.keyIDs !== undefined) keyIDs.value = f.keyIDs
    offset.value = 0
    void load()
  }

  /** 进入分栏。重置筛选，理由同渠道目录：留着上次的分段会让人低估目录规模。 */
  async function open(): Promise<void> {
    unit.value = ''
    q.value = ''
    channelIDs.value = []
    vendors.value = []
    endpoints.value = []
    keyIDs.value = []
    sort.value = ''
    offset.value = 0
    resp.value = null
    loaded.value = false
    await load()
  }

  function setUnit(u: string): void {
    unit.value = u
    offset.value = 0
    void load()
  }

  function setQuery(v: string): void {
    clearTimeout(timer)
    timer = setTimeout(() => {
      const t = v.trim()
      if (t === q.value) return
      q.value = t
      offset.value = 0
      void load()
    }, DEBOUNCE_MS)
  }

  function prev(): void {
    offset.value = Math.max(0, offset.value - pageSize.value)
    void load()
  }

  function next(): void {
    offset.value += pageSize.value
    void load()
  }

  /** 改排序/每页行数都回第一页 —— 不回的话多半落到空页上。 */
  function setSort(v: string): void {
    sort.value = v
    offset.value = 0
    void load()
  }

  function setPageSize(n: number): void {
    pageSize.value = n
    offset.value = 0
    void load()
  }

  return {
    unit,
    q,
    channelIDs,
    vendors,
    endpoints,
    keyIDs,
    sort,
    pageSize,
    loaded,
    segments,
    vendorFacet,
    vendorIcons,
    endpointFacet,
    channelFacet,
    whole,
    items,
    total,
    from,
    to,
    hasPrev,
    hasNext,
    filtering,
    open,
    setUnit,
    setQuery,
    setFilters,
    toggleChannel,
    toggleVendor,
    toggleEndpoint,
    toggleKey,
    setSort,
    setPageSize,
    clearFilters,
    prev,
    next,
  }
})
