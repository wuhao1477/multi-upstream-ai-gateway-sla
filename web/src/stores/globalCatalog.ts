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

/** 每页行数。与渠道目录同值：两个界面并排放着，一页多少行不该不一样。 */
export const MODEL_PAGE = 50

const DEBOUNCE_MS = 250

export const useGlobalCatalogStore = defineStore('globalCatalog', () => {
  const toast = useToastStore()

  const unit = ref('')
  const q = ref('')
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
  const filtering = computed(() => unit.value !== '' || q.value !== '')

  async function load(): Promise<void> {
    const request = { unit: unit.value, q: q.value, offset: offset.value }
    try {
      const data = await adminApi.globalCatalog({
        unit: request.unit,
        q: request.q,
        limit: MODEL_PAGE,
        offset: request.offset,
      })
      // 丢弃过期响应：三个筛选条件任一在等待期间变过，这份就不是当前视图的
      // 答案了。慢的那次回来覆盖快的那次，表现是"筛完又跳回上一批"。
      if (
        unit.value === request.unit &&
        q.value === request.q &&
        offset.value === request.offset
      ) {
        resp.value = data
        loaded.value = true
      }
    } catch (e) {
      toast.fail('加载模型目录失败', e)
    }
  }

  /** 进入分栏。重置筛选，理由同渠道目录：留着上次的分段会让人低估目录规模。 */
  async function open(): Promise<void> {
    unit.value = ''
    q.value = ''
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
    offset.value = Math.max(0, offset.value - MODEL_PAGE)
    void load()
  }

  function next(): void {
    offset.value += MODEL_PAGE
    void load()
  }

  return {
    unit,
    q,
    loaded,
    segments,
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
    prev,
    next,
  }
})
