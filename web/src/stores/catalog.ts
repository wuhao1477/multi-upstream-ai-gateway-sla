import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import * as adminApi from '@/api/admin'
import type { CatalogResp } from '@/api/types'
import { useToastStore } from './toast'

/** 每页行数。真库单渠道 1369 行，50 行一页在 1280 宽下正好不用滚两屏。 */
export const CAT_PAGE = 50

/** 名称筛选防抖：真库单渠道 1369 行，每个按键都打一次后端没必要。 */
const DEBOUNCE_MS = 250

export const useCatalogStore = defineStore('catalog', () => {
  const toast = useToastStore()

  /** 分段与页码是**视图状态而非请求参数默认值**：切分段要回到第一页，
   *  否则从"倍率第 4 页"切到只有 208 行的按次段会落到空页上。 */
  const unit = ref('')
  const q = ref('')
  const offset = ref(0)
  const resp = ref<CatalogResp | null>(null)

  let channelID: number | null = null
  let timer: ReturnType<typeof setTimeout> | undefined

  /** 分段按钮读 units（后端在筛选**之前**算的），不读 items ——
   *  否则切进某段后其余段的按钮会自己消失，出不去。 */
  const segments = computed<Array<[string, number]>>(() => {
    const u = resp.value?.units
    if (u === undefined) return []
    return Object.entries(u).sort((a, b) => b[1] - a[1])
  })

  /** 全部段的总行数。 */
  const whole = computed(() => segments.value.reduce((n, [, c]) => n + c, 0))

  const items = computed(() => resp.value?.items ?? [])
  const total = computed(() => resp.value?.total ?? 0)
  const from = computed(() => (total.value === 0 ? 0 : offset.value + 1))
  const to = computed(() => offset.value + items.value.length)
  const hasPrev = computed(() => offset.value > 0)
  const hasNext = computed(() => to.value < total.value)
  /** 当前是否处于筛选态（用于页脚标注"（当前筛选）"）。 */
  const filtering = computed(() => unit.value !== '' || q.value !== '')

  async function load(): Promise<void> {
    const id = channelID
    if (id === null) return
    const request = {
      unit: unit.value,
      q: q.value,
      offset: offset.value,
    }
    try {
      const data = await adminApi.channelCatalog(id, {
        unit: request.unit,
        q: request.q,
        limit: CAT_PAGE,
        offset: request.offset,
      })
      if (
        channelID === id &&
        unit.value === request.unit &&
        q.value === request.q &&
        offset.value === request.offset
      ) {
        resp.value = data
      }
    } catch (e) {
      toast.fail('加载目录失败', e)
    }
  }

  /** 进入目录视图。每次都重置筛选：留着上次的分段会让人以为目录只有那么多行。 */
  async function open(id: number): Promise<void> {
    channelID = id
    unit.value = ''
    q.value = ''
    offset.value = 0
    resp.value = null
    await load()
  }

  function setUnit(u: string): void {
    unit.value = u
    offset.value = 0
    void load()
  }

  /** 输入框改动。防抖 + 与当前值相同则不重复请求。 */
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
    offset.value = Math.max(0, offset.value - CAT_PAGE)
    void load()
  }

  function next(): void {
    offset.value += CAT_PAGE
    void load()
  }

  return {
    unit,
    q,
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
