import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import * as adminApi from '@/api/admin'
import { ApiError } from '@/api/client'
import type { Channel, InventoryResp, SiteFamilyInfo, SyncItem, SyncResult } from '@/api/types'
import { useToastStore } from './toast'

/**
 * 从失败响应体里取 items。按形态取而不认类型，因为四种失败体带的字段各不相同：
 *  - 429/409 限流或已在跑：带 items（单项 status='skipped'）
 *  - 422 前置条件不满足（配置问题，故意不报 502）：带 site_family + items
 *  - 502 认证/连接失败：**不带** items —— 所以返回 undefined 的分支不是兜底，
 *    它就是 502 的正常路径，调用方靠它决定退回纯文本报错。
 */
function itemsOf(body: unknown): SyncItem[] | undefined {
  if (typeof body === 'object' && body !== null && 'items' in body) {
    const it = (body as { items?: unknown }).items
    if (Array.isArray(it)) return it as SyncItem[]
  }
  return undefined
}

export const useChannelsStore = defineStore('channels', () => {
  const toast = useToastStore()

  const list = ref<Channel[]>([])
  const loaded = ref(false)
  /** 筛选词。65 个站点靠肉眼找太慢，按名称/地址/站型过滤。 */
  const filter = ref('')

  /** 当前选中渠道。 */
  const currentID = ref<number | null>(null)
  const currentName = ref('')

  const inventory = ref<InventoryResp | null>(null)
  const syncResult = ref<SyncResult | null>(null)
  /** 同步失败且响应体里没有 items 时的兜底文案。 */
  const syncError = ref('')
  const syncing = ref(false)

  const count = computed(() => list.value.length)

  /** 过滤是**重渲染**而不是隐藏行：验收脚本直接数 tbody tr。 */
  const filtered = computed(() => {
    const q = filter.value.trim().toLowerCase()
    if (q === '') return list.value
    return list.value.filter((c) =>
      `${c.name} ${c.base_url} ${c.site_family}`.toLowerCase().includes(q),
    )
  })

  /**
   * 后端注册了哪些站型（新增表单的下拉用它）。
   *
   * 与 list 一起加载而不是各自触发：两者都要 token，而 token 是进入界面后
   * 才有的。加载失败时留空数组 —— 下拉只剩"自动探测"，而自动探测本就是
   * 推荐路径，建渠道不会因此做不了。
   */
  const families = ref<SiteFamilyInfo[]>([])

  async function load(): Promise<void> {
    try {
      const d = await adminApi.listChannels()
      list.value = d.items
      loaded.value = true
    } catch (e) {
      toast.fail('加载渠道失败', e)
      // 渠道都拉不到（最常见是没填令牌）时不再打第二个请求：
      // 它必然同样失败，而后一条 toast 会**盖掉**前一条 —— 运维看到的是
      // "站型注册表失败"，而真正该看的是"请先填入 ADMIN_TOKEN"。
      // 验收里那条"无令牌时拒绝拉取数据"就是这么被换掉文案的。
      return
    }
    try {
      families.value = (await adminApi.listSiteFamilies()).items
    } catch (e) {
      // 单独 catch：渠道拉到了而站型表没拿到（如后端版本旧），
      // 那是真该单独报出来的一种失败，不该让渠道列表也显示成加载失败。
      toast.fail('加载站型注册表失败（站型下拉只剩自动探测）', e)
    }
  }

  async function create(input: adminApi.CreateChannelInput): Promise<boolean> {
    try {
      const d = await adminApi.createChannel(input)
      let msg = `渠道已创建：#${d.id}`
      if (d.detected) {
        msg += `\n探测到站型 ${d.detected.family}`
        if (d.detected.version !== undefined && d.detected.version !== '') {
          msg += ` ${d.detected.version}`
        }
        if (d.detected.quota_per_unit !== undefined) {
          msg += `\nquota_per_unit=${d.detected.quota_per_unit}`
        }
      }
      if (d.warning !== undefined && d.warning !== '') msg += `\n⚠️ ${d.warning}`
      if (d.warning_persist !== undefined && d.warning_persist !== '') {
        msg += `\n⚠️ ${d.warning_persist}`
      }
      toast.show(msg, 'ok')
      await load()
      return true
    } catch (e) {
      toast.fail('创建失败', e)
      return false
    }
  }

  /**
   * 改渠道（改名/换地址/停用/启用）。成功后重拉列表 —— 不本地改 list.value，
   * 那样界面显示的是我们以为写进去的东西，而不是库里真有的东西。
   */
  async function patch(id: number, input: adminApi.PatchChannelInput): Promise<boolean> {
    try {
      await adminApi.patchChannel(id, input)
      await load()
      return true
    } catch (e) {
      toast.fail('修改渠道失败', e)
      return false
    }
  }

  /** 选中一个渠道：清掉上一个渠道的采集结果，再拉总览。 */
  async function select(id: number, name: string): Promise<void> {
    currentID.value = id
    currentName.value = name
    inventory.value = null
    syncResult.value = null
    syncError.value = ''
    await loadInventory()
  }

  async function loadInventory(): Promise<void> {
    const id = currentID.value
    if (id === null) return
    try {
      inventory.value = await adminApi.channelInventory(id)
    } catch (e) {
      toast.fail('加载总览失败', e)
    }
  }

  async function sync(): Promise<void> {
    const id = currentID.value
    if (id === null) return
    syncing.value = true
    syncResult.value = null
    syncError.value = ''
    try {
      syncResult.value = await adminApi.syncChannel(id)
      await loadInventory()
    } catch (e) {
      // 429/409/422 都带结构化响应，一并展示 —— 运维要看到"哪一项被跳过了"，
      // 而不是只看到一句"采集失败"。
      const items = e instanceof ApiError ? itemsOf(e.body) : undefined
      if (items !== undefined) {
        syncResult.value = {
          channel_id: id,
          site_family: 'unknown',
          started_at: '',
          elapsed_ms: 0,
          items,
        }
      } else {
        syncError.value = e instanceof Error ? e.message : String(e)
      }
      toast.fail('采集未成功', e)
    } finally {
      syncing.value = false
    }
  }

  return {
    list,
    loaded,
    families,
    filter,
    filtered,
    count,
    currentID,
    currentName,
    inventory,
    syncResult,
    syncError,
    syncing,
    load,
    create,
    patch,
    select,
    sync,
  }
})
