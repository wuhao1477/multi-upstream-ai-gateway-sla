<script setup lang="ts">
/**
 * Key 管理 —— 跨渠道的凭证工作台。
 *
 * 两种视图解决**两类不同的任务**，所以都要有，而且共用同一套筛选：
 *  · 平铺：跨渠道搜索与排查（"哪些 Key 快没配额了"）。默认视图 ——
 *    全局 Key 管理首先是操作型工作台。
 *  · 按渠道 / 按账号分组：理解归属与整理配置（"这个渠道下都挂了什么"）。
 *
 * 切视图**不重置筛选**。两边各自实现筛选的话，用户切一下就发现"少了几把"，
 * 而那时分不清是筛选变了还是数据变了。筛选逻辑统一在 utils/keyfilter。
 *
 * 筛选写进 URL：刷新、回退、把链接发给别人，看到的是同一批 Key。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import UiStat from '@/components/ui/UiStat.vue'
import UiSegmented from '@/components/ui/UiSegmented.vue'
import KeyTable from '@/components/res/KeyTable.vue'
import RegisterDrawers from '@/components/res/RegisterDrawers.vue'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import {
  KEY_OPTIONAL_COLS,
  bucketize,
  emptyKeyFilter,
  filterKeys,
  isFilterActive,
} from '@/utils/keyfilter'
import { totalQuota, usdFine } from '@/utils/money'

type ViewMode = 'flat' | 'by-channel' | 'by-account'

const route = useRoute()
const router = useRouter()
const channels = useChannelsStore()
const res = useResourcesStore()

const filter = ref(emptyKeyFilter())
const view = ref<ViewMode>('flat')
const cols = ref<string[]>([])
const collapsed = ref<Set<string>>(new Set())
const drawer = ref<'account' | 'key' | null>(null)

const VIEW_KEY = 'sla.keys.view'
const COLS_KEY = 'sla.keys.cols'

/** 从 URL 与 localStorage 恢复。URL 优先 —— 别人发来的链接必须能覆盖本地偏好。 */
function restore(): void {
  const q = route.query
  const s = (k: string): string => (typeof q[k] === 'string' ? (q[k] as string) : '')
  const n = (k: string): number => Number(s(k)) || 0
  filter.value = {
    q: s('q'),
    channelID: n('channel'),
    accountID: n('account'),
    status: s('status'),
    groupRef: s('group'),
    quota: (s('quota') || 'all') as typeof filter.value.quota,
    expiry: (s('expiry') || 'all') as typeof filter.value.expiry,
    hasRateLimit: s('rl') === '1',
  }
  const urlView = s('view')
  const saved = localStorage.getItem(VIEW_KEY)
  const v = urlView !== '' ? urlView : (saved ?? 'flat')
  view.value = v === 'by-channel' || v === 'by-account' ? v : 'flat'
  // 落盘一次。watch(view) 只在**变化**时写，而从 URL 恢复出的值往往与初始值
  // 相同（都是 flat）—— 不在这里写的话，别人发来的 ?view=flat 链接打开一次、
  // 刷新一下就跳回本地记着的分组视图，像是链接没生效。
  localStorage.setItem(VIEW_KEY, view.value)
  try {
    const c = JSON.parse(localStorage.getItem(COLS_KEY) ?? '[]') as unknown
    if (Array.isArray(c)) cols.value = c.filter((x): x is string => typeof x === 'string')
  } catch {
    // 存坏了就当没存过。列偏好丢了只是少两列，不值得为它报错
    cols.value = []
  }
}

/** 把当前筛选写回 URL。用 replace 而不是 push —— 每敲一个字符都进历史栈的话，返回键就废了。 */
function syncURL(): void {
  const f = filter.value
  const q: Record<string, string> = {}
  if (f.q.trim() !== '') q.q = f.q.trim()
  if (f.channelID !== 0) q.channel = String(f.channelID)
  if (f.accountID !== 0) q.account = String(f.accountID)
  if (f.status !== '') q.status = f.status
  if (f.groupRef !== '') q.group = f.groupRef
  if (f.quota !== 'all') q.quota = f.quota
  if (f.expiry !== 'all') q.expiry = f.expiry
  if (f.hasRateLimit) q.rl = '1'
  if (view.value !== 'flat') q.view = view.value
  void router.replace({ query: q })
}

onMounted(() => {
  restore()
  void res.load()
})

watch(filter, syncURL, { deep: true })
watch(view, (v) => {
  localStorage.setItem(VIEW_KEY, v)
  syncURL()
})
watch(cols, (c) => localStorage.setItem(COLS_KEY, JSON.stringify(c)), { deep: true })

const shown = computed(() => filterKeys(res.keys, filter.value, res.accountLabel))
const active = computed(() => isFilterActive(filter.value))
const quota = computed(() => totalQuota(shown.value))

/** 分组视图的桶。总数用**全量**而不是筛选后 —— 分组头要写「匹配 3 / 全部 26」。 */
const buckets = computed(() => {
  if (view.value === 'by-channel') {
    return bucketize(
      res.keys,
      shown.value,
      (k) => k.channel_id,
      (id) => channels.list.find((c) => c.id === id)?.name ?? `渠道 #${id}`,
    )
  }
  return bucketize(res.keys, shown.value, (k) => k.account_id, res.accountLabel)
})

/**
 * 搜索时自动展开命中的分组，但**不覆盖用户平时的收起偏好** ——
 * 所以记的是"被收起的"而不是"被展开的"，筛选一变就清空这份记录。
 */
watch(
  () => [filter.value.q, filter.value.status, filter.value.quota, filter.value.expiry],
  () => {
    if (active.value) collapsed.value = new Set()
  },
)

function toggleBucket(key: string): void {
  const next = new Set(collapsed.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  collapsed.value = next
}

const groupOptions = computed(() => {
  const s = new Set<string>()
  for (const k of res.keys) if (k.group_ref !== undefined && k.group_ref !== '') s.add(k.group_ref)
  return [...s].sort()
})

const accountOptions = computed(() =>
  filter.value.channelID === 0 ? res.accounts : res.channelAccounts(filter.value.channelID),
)

/** 换渠道时清掉已选账号：它多半属于别的渠道，留着会筛出零条且看不出原因。 */
watch(
  () => filter.value.channelID,
  (ch) => {
    if (ch === 0 || filter.value.accountID === 0) return
    const a = res.accountByID.get(filter.value.accountID)
    if (a !== undefined && a.channel_id !== ch) filter.value.accountID = 0
  },
)

function reset(): void {
  filter.value = emptyKeyFilter()
}

/** 快捷筛选。点第二次取消 —— chip 是开关不是按钮。 */
function chip(kind: 'low' | 'exhausted' | 'unlimited' | 'unknown'): void {
  filter.value.quota = filter.value.quota === kind ? 'all' : kind
}
</script>

<template>
  <div class="pane on" id="pane-keys">
    <UiCard>
      <template #header>
        <div>
          <h2 class="card-t">上游 Key</h2>
          <p class="card-d">
            网关拿去访问上游的凭证。明文永不回显，列表只显示前缀（FR-094）。
          </p>
        </div>
        <div class="spacer"></div>
        <button class="btn outline sm" id="btn-keys-reload" :disabled="res.loading" @click="res.reload()">
          {{ res.loading ? '刷新中…' : '刷新' }}
        </button>
        <button class="btn" id="btn-new-key" @click="drawer = 'key'">+ 登记 Key</button>
      </template>

      <div class="stats">
        <UiStat label="Key 数" :value="shown.length" />
        <UiStat label="剩余配额合计" :value="usdFine(quota.remain)" />
        <UiStat
          label="最小剩余配额"
          :value="quota.min === null ? '—' : usdFine(quota.min)"
        />
        <UiStat label="不限额度" :value="quota.unlimited" />
        <UiStat label="配额未采集" :value="quota.unknown" />
        <UiStat label="配额耗尽" :value="quota.exhausted" />
      </div>
      <p class="note">
        「Key 剩余配额」是<b>使用约束，不是钱</b>：它不能与账号余额相加，也不互相蕴含 ——
        账号有余额不代表 Key 能调用，Key 配额没用尽也不代表账号还有钱。合计只含
        {{ quota.counted }} 把「已采到且有限额」的 Key，另外 {{ quota.unlimited }} 把不限额、
        {{ quota.unknown }} 把未采集，都不计入。看规模用合计，<b>看风险用最小值</b>。
      </p>
    </UiCard>

    <UiCard>
      <template #header>
        <UiSegmented
          v-model="view"
          label="Key 视图模式"
          :options="[
            { value: 'flat', label: '平铺列表' },
            { value: 'by-channel', label: '按渠道分组' },
            { value: 'by-account', label: '按账号分组' },
          ]"
        />
        <div class="spacer"></div>
        <!-- 列设置用原生 <details>：不引 popover 依赖，Esc 与点外部关闭
             由浏览器自己管，键盘也天然可达 -->
        <details class="more cols">
          <summary class="btn outline sm" id="btn-cols">列设置</summary>
          <div class="more-menu wide">
            <label v-for="c in KEY_OPTIONAL_COLS" :key="c.key" class="chk">
              <input type="checkbox" :value="c.key" v-model="cols" />
              {{ c.label }}
            </label>
            <p class="note">默认列之外的字段。全打开会让表格挤到看不清，按需开。</p>
          </div>
        </details>
      </template>

      <div class="toolbar">
        <div class="tb-grow">
          <label class="sr" for="key-q">搜索 Key</label>
          <input id="key-q" v-model="filter.q" placeholder="搜索前缀 / 上游标识 / 分组 / 账号…" />
        </div>
        <UiField label="渠道" for="key-f-channel">
          <select id="key-f-channel" v-model.number="filter.channelID">
            <option :value="0">全部渠道</option>
            <option v-for="c in channels.list" :key="c.id" :value="c.id">{{ c.name }}</option>
          </select>
        </UiField>
        <UiField label="账号" for="key-f-account">
          <select id="key-f-account" v-model.number="filter.accountID">
            <option :value="0">全部账号</option>
            <option v-for="a in accountOptions" :key="a.id" :value="a.id">
              {{ res.accountLabel(a.id) }}
            </option>
          </select>
        </UiField>
        <UiField label="状态" for="key-f-status">
          <select id="key-f-status" v-model="filter.status">
            <option value="">全部状态</option>
            <option value="active">可用</option>
            <option value="revoked">已停用</option>
            <option value="expired">已过期</option>
            <option value="insufficient_perm">权限不足</option>
          </select>
        </UiField>
        <UiField label="分组" for="key-f-group">
          <select id="key-f-group" v-model="filter.groupRef">
            <option value="">全部分组</option>
            <option value="__none__">未归组</option>
            <option v-for="g in groupOptions" :key="g" :value="g">{{ g }}</option>
          </select>
        </UiField>
        <UiField label="有效期" for="key-f-expiry">
          <select id="key-f-expiry" v-model="filter.expiry">
            <option value="all">全部有效期</option>
            <option value="expiring">7 天内到期</option>
            <option value="expired">已过期</option>
          </select>
        </UiField>
        <button class="btn outline sm" id="btn-key-reset" :disabled="!active" @click="reset">
          重置
        </button>
      </div>

      <div class="chips">
        <button
          class="chip"
          :class="{ on: filter.quota === 'low' }"
          data-chip="low"
          @click="chip('low')"
        >
          配额将耗尽
        </button>
        <button
          class="chip"
          :class="{ on: filter.quota === 'exhausted' }"
          data-chip="exhausted"
          @click="chip('exhausted')"
        >
          配额已耗尽
        </button>
        <button
          class="chip"
          :class="{ on: filter.quota === 'unlimited' }"
          data-chip="unlimited"
          @click="chip('unlimited')"
        >
          不限额度
        </button>
        <button
          class="chip"
          :class="{ on: filter.quota === 'unknown' }"
          data-chip="unknown"
          @click="chip('unknown')"
        >
          配额未采集
        </button>
        <button
          class="chip"
          :class="{ on: filter.hasRateLimit }"
          data-chip="rl"
          @click="filter.hasRateLimit = !filter.hasRateLimit"
        >
          有上游限流
        </button>
        <span v-if="active" class="dim" id="key-filter-summary"
          >筛出 {{ shown.length }} / {{ res.keys.length }} 把</span
        >
      </div>

      <UiEmpty v-if="!res.loaded && res.error === ''">加载中…</UiEmpty>
      <UiEmpty v-else-if="res.error !== ''">加载失败：{{ res.error }}</UiEmpty>
      <UiEmpty v-else-if="res.keys.length === 0">
        还没有任何上游 Key。点右上角「登记 Key」创建第一把。
      </UiEmpty>
      <UiEmpty v-else-if="shown.length === 0">没有符合当前筛选条件的 Key</UiEmpty>

      <KeyTable
        v-else-if="view === 'flat'"
        :items="shown"
        :cols="cols"
        show-channel
        @changed="res.reload()"
      />

      <div v-else class="groups">
        <section v-for="b in buckets" :key="b.key" class="group" :data-bucket="b.id">
          <header class="group-h">
            <button
              class="twist"
              :aria-expanded="!collapsed.has(b.key)"
              :data-bucket-toggle="b.id"
              @click="toggleBucket(b.key)"
            >
              {{ collapsed.has(b.key) ? '▸' : '▾' }}
            </button>
            <b>{{ b.label }}</b>
            <!-- 「匹配 N / 全部 M」：只写匹配数的话，筛选之后看起来像 Key 丢了 -->
            <span class="badge" :data-bucket-count="b.id"
              >匹配 {{ b.matched.length }} / 全部 {{ b.total }}</span
            >
            <span class="dim">剩余配额合计 {{ usdFine(totalQuota(b.matched).remain) }}</span>
            <span
              v-if="totalQuota(b.matched).min !== null"
              class="dim"
              title="该组里最小的一把。看风险用它，不看合计"
              >最小 {{ usdFine(totalQuota(b.matched).min!) }}</span
            >
          </header>
          <KeyTable
            v-if="!collapsed.has(b.key)"
            :items="b.matched"
            :cols="cols"
            :show-channel="view === 'by-account'"
            :show-account="view === 'by-channel'"
            @changed="res.reload()"
          />
        </section>
      </div>
    </UiCard>

    <RegisterDrawers
      :mode="drawer"
      :channel-id="filter.channelID"
      :account-id="filter.accountID"
      @close="drawer = null"
    />
  </div>
</template>
