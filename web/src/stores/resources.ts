/**
 * 全量账号与 Key 的共享缓存。
 *
 * 四个地方要看同一批数据：渠道列表的二级展开、账号管理页、Key 管理页、
 * 渠道详情的两个 Tab。各自去拉的话，65 个渠道的展开就是 65×2 次往返，
 * 而且四处的"刚改完还没刷新"状态会互相打架。
 *
 * 所以一次拉全量（`/admin/accounts` 与 `/admin/keys` 不带 channel_id 即返回
 * 全部，见 store/channels.go 的 `$1 <= 0 OR channel_id = $1`），在前端按
 * channel_id / account_id 分组。当前规模是 65 渠道 / 数百把 Key，两个请求。
 *
 * ponytail: 全量拉取 + 前端筛选。到几千把 Key 时改服务端分页与筛选
 * （`/admin/keys` 加 q/status/limit/offset），届时本 store 的对外形状不变。
 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import * as adminApi from '@/api/admin'
import type { Account, ChannelGroup, Key } from '@/api/types'
import { useToastStore } from './toast'

export const useResourcesStore = defineStore('resources', () => {
  const toast = useToastStore()

  const accounts = ref<Account[]>([])
  const keys = ref<Key[]>([])
  const groups = ref<ChannelGroup[]>([])
  /** 已经成功拉过至少一轮。区分"空库"与"还没拉" —— 空态文案完全不同。 */
  const loaded = ref(false)
  const loading = ref(false)
  /** 上一轮拉取失败的原因；非空时界面渲染失败态而不是空态。 */
  const error = ref('')

  /**
   * 请求序号。四个页面都可能触发 reload，慢的那一次回来时不能覆盖快的那一次
   * —— 否则删完一把 Key 后列表会闪回删除前的样子。
   */
  let request = 0

  async function load(force = false): Promise<void> {
    if (loaded.value && !force) return
    const seq = ++request
    loading.value = true
    try {
      // 三个并发：它们互不依赖，串行只是把等待时间加起来。
      const [a, k, g] = await Promise.all([
        adminApi.listAccounts(),
        adminApi.listKeys(),
        adminApi.listGroups(),
      ])
      if (seq !== request) return
      accounts.value = a.items
      keys.value = k.items
      groups.value = g.items
      loaded.value = true
      error.value = ''
    } catch (e) {
      if (seq !== request) return
      error.value = e instanceof Error ? e.message : String(e)
      toast.fail('加载账号与 Key 失败', e)
    } finally {
      if (seq === request) loading.value = false
    }
  }

  /** 写操作之后调它。强制重拉而不是本地改数组：界面要显示库里真有的东西。 */
  function reload(): Promise<void> {
    return load(true)
  }

  const accountByID = computed(() => {
    const m = new Map<number, Account>()
    for (const a of accounts.value) m.set(a.id, a)
    return m
  })

  const groupByID = computed(() => {
    const m = new Map<number, ChannelGroup>()
    for (const g of groups.value) m.set(g.id, g)
    return m
  })

  const accountsByChannel = computed(() => {
    const m = new Map<number, Account[]>()
    for (const a of accounts.value) {
      const arr = m.get(a.channel_id)
      if (arr === undefined) m.set(a.channel_id, [a])
      else arr.push(a)
    }
    return m
  })

  const keysByChannel = computed(() => {
    const m = new Map<number, Key[]>()
    for (const k of keys.value) {
      const arr = m.get(k.channel_id)
      if (arr === undefined) m.set(k.channel_id, [k])
      else arr.push(k)
    }
    return m
  })

  const keysByAccount = computed(() => {
    const m = new Map<number, Key[]>()
    for (const k of keys.value) {
      const arr = m.get(k.account_id)
      if (arr === undefined) m.set(k.account_id, [k])
      else arr.push(k)
    }
    return m
  })

  /**
   * 账号的展示名。
   *
   * 上游账号在 P1 **没有名字列** —— 只有 id 和 external_user_id（NewAPI 的
   * 用户 ID 头的值）。所以这里给的是"#12 · user-prod"这种可辨认串，
   * 而不是编一个名字。没有 external_user_id 时就只有 #12，那是真实情况。
   */
  function accountLabel(id: number): string {
    const a = accountByID.value.get(id)
    if (a === undefined) return `#${id}`
    const uid = a.external_user_id
    return uid === undefined || uid === '' ? `#${a.id}` : `#${a.id} · ${uid}`
  }

  function channelAccounts(channelID: number): Account[] {
    return accountsByChannel.value.get(channelID) ?? []
  }

  function channelKeys(channelID: number): Key[] {
    return keysByChannel.value.get(channelID) ?? []
  }

  function accountKeys(accountID: number): Key[] {
    return keysByAccount.value.get(accountID) ?? []
  }

  /** 某渠道的分组（Key 的分组下拉用它）。 */
  function channelGroups(channelID: number): ChannelGroup[] {
    return groups.value.filter((g) => g.channel_id === channelID)
  }

  return {
    accounts,
    keys,
    groups,
    loaded,
    loading,
    error,
    load,
    reload,
    accountByID,
    groupByID,
    accountsByChannel,
    keysByChannel,
    keysByAccount,
    accountLabel,
    channelAccounts,
    channelKeys,
    accountKeys,
    channelGroups,
  }
})
