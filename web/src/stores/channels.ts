import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import * as adminApi from '@/api/admin'
import { ApiError } from '@/api/client'
import type { Channel, InventoryResp, SiteFamilyInfo, SyncItem, SyncResult } from '@/api/types'
import { collectBlockerLabel, collectModeLabel, familyLabel } from '@/utils/format'
import { useResourcesStore } from './resources'
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
  const res = useResourcesStore()

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
  let syncRequest = 0

  const count = computed(() => list.value.length)

  /**
   * 渠道 id → 搜索用的干草堆（名称 + 地址 + 站型原值与展示名 + #id）。
   *
   * **账号页与 Key 页也读它**：那两页原先只搜自己那点字段（账号 id / 上游用户 id
   * / Key 前缀 / 分组），于是"钱多多那个站的 Key 在哪"根本搜不出来 —— 而人记得住
   * 的恰恰是站名和域名，记不住 Key 前缀。三处共用同一个串，才不会出现
   * "渠道页搜得到、Key 页搜不到"这种各说各话。
   *
   * 站型两种写法都收：库里存的是 `newapi`，界面上显示的是「NewAPI 系」，
   * 照着屏幕上的字搜是更自然的动作。
   */
  const hays = computed(() => {
    const m = new Map<number, string>()
    for (const c of list.value) {
      m.set(
        c.id,
        (
          `${c.name} ${c.base_url} ${c.site_family} ${familyLabel(c.site_family)} #${c.id} ` +
          // 采集状态也收进来：「需人工」一搜就能把所有要人动手的站捞出来。
          // 原值与展示名都收（同站型那两种写法），照着屏幕上的字搜是更自然的动作。
          `${c.collect.mode} ${collectModeLabel(c.collect.mode)} ` +
          `${c.collect.blocker ?? ''} ${collectBlockerLabel(c.collect.blocker)}`
        ).toLowerCase(),
      )
    }
    return m
  })

  /** 取某个渠道的干草堆。渠道还没拉回来时返回空串 —— 匹配不上，不是匹配全部。 */
  function hay(id: number): string {
    return hays.value.get(id) ?? ''
  }

  /** 过滤是**重渲染**而不是隐藏行：验收脚本直接数 tbody tr。 */
  const filtered = computed(() => {
    const q = filter.value.trim().toLowerCase()
    if (q === '') return list.value
    return list.value.filter((c) => hay(c.id).includes(q))
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
        msg += `\n探测到站型 ${familyLabel(d.detected.family)}`
        if (d.detected.version !== undefined && d.detected.version !== '') {
          msg += ` ${d.detected.version}`
        }
        if (d.detected.quota_per_unit !== undefined) {
          msg += `\n额度换算基数=${d.detected.quota_per_unit}`
        }
      }
      if (d.warning !== undefined && d.warning !== '') msg += `\n⚠️ ${d.warning}`
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
    syncRequest++
    currentID.value = id
    currentName.value = name
    inventory.value = null
    syncResult.value = null
    syncError.value = ''
    syncing.value = false
    await loadInventory()
  }

  function clearSelection(): void {
    syncRequest++
    currentID.value = null
    currentName.value = ''
    inventory.value = null
    syncResult.value = null
    syncError.value = ''
    syncing.value = false
  }

  async function loadInventory(): Promise<void> {
    const id = currentID.value
    if (id === null) return
    try {
      const next = await adminApi.channelInventory(id)
      if (currentID.value === id) inventory.value = next
    } catch (e) {
      toast.fail('加载总览失败', e)
    }
  }

  async function sync(): Promise<void> {
    const id = currentID.value
    if (id === null) return
    const request = ++syncRequest
    syncing.value = true
    syncResult.value = null
    syncError.value = ''
    try {
      const next = await adminApi.syncChannel(id)
      if (currentID.value !== id || syncRequest !== request) return
      syncResult.value = next
      // 采集**改变了库里的账号/Key/分组**，共享缓存必须跟着重拉。
      //
      // 漏了这一步的症状很隐蔽：界面看着采成功了（逐项结果表全绿、总览数字
      // 也变了，因为 inventory 是另拉的），但 Key 编辑里的「所属分组」下拉
      // 是空的 —— 分组是这轮采集才建的，而 res.groups 还停在进页面时那一份。
      // 旧版每个子视图自己 onMounted 拉分组，所以从来碰不到；改成共享缓存
      // 之后就必须在这里补。本地验收正是在这条上超时的。
      await Promise.all([loadInventory(), res.reload()])
    } catch (e) {
      if (currentID.value !== id || syncRequest !== request) return
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
      if (currentID.value === id && syncRequest === request) syncing.value = false
    }
  }

  return {
    list,
    loaded,
    families,
    filter,
    filtered,
    hay,
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
    clearSelection,
    sync,
  }
})
