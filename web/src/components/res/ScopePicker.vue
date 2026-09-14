<script setup lang="ts">
/**
 * 渠道 / 账号选择器：一个触发按钮 + 一个列表弹窗。
 *
 * 取代 Key 相关表单里的原生 `<select>`。换掉的理由不是好看：65 个渠道里有
 * 成批名字高度相似的站点（同一批镜像站只差一两个字），而原生下拉每行只有
 * `name` —— 选错了没有任何提示，选错的后果是把「同步 / 补齐 Key」打到别人家
 * 站点上。所以这里每一行都带**域名**，它才是渠道真正的身份标识；账号行带
 * `#id` 与上游用户 ID（P1 的账号没有名字列，见 stores/resources.accountLabel）。
 *
 * 值统一是 `number[]`，单选时长度 ≤ 1。不做 `number | number[]` 两套类型：
 * 调用方就得在 0 / undefined / [] 之间来回转换，而"0 表示全部"这个约定
 * 在 keyfilter 里已经踩过一次（换渠道时残留的账号 id 会筛出零条且看不出原因）。
 *
 * 勾选是**弹窗内的草稿**，点「确定」才写回 v-model。边勾边生效的话，多选
 * 渠道时每勾一个都会触发一次 URL 重写与整表重算，而人还没选完。
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import type { Channel } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { dynamicRateText, familyLabel, statusLabel } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    /** 触发按钮的 id。与其它表单控件同理，id 是验收契约，由调用方写。 */
    id: string
    kind: 'channel' | 'account' | 'key'
    modelValue: number[]
    multi?: boolean
    /** 账号选择器的渠道范围；空数组 = 不限。渠道选择器忽略它。 */
    scope?: number[]
    /** 未选中任何项时按钮上的文案。 */
    placeholder?: string
    disabled?: boolean
  }>(),
  { multi: false, scope: () => [], placeholder: '请选择…', disabled: false },
)
const emit = defineEmits<{ 'update:modelValue': [number[]] }>()

const channels = useChannelsStore()
const res = useResourcesStore()

const open = ref(false)
const q = ref('')
/** 渠道选择器 = 站型；账号选择器 = 余额状态。'' = 不筛。 */
const facet = ref('')
/** 两种选择器都有的「启用 / 停用」筛选。'' = 不筛。 */
const state = ref('')
/** 弹窗内的草稿选择。 */
const draft = ref<Set<number>>(new Set())

const NOUN: Record<'channel' | 'account' | 'key', string> = {
  channel: '渠道',
  account: '账号',
  key: 'Key',
}
const noun = computed(() => NOUN[props.kind])

/** 域名才是渠道的身份。协议与端口留在 title 里，列表只显示主机名。 */
function host(url: string): string {
  try {
    return new URL(url).host
  } catch {
    // 地址存坏了也要显示出来 —— 显示成空白会让人以为是渠道没配地址
    return url
  }
}

const channelByID = computed(() => {
  const m = new Map<number, Channel>()
  for (const c of channels.list) m.set(c.id, c)
  return m
})

interface Row {
  id: number
  /** 主标题：渠道名 / 账号标签。 */
  title: string
  /** 主标题下那行小字。 */
  sub: string
  /** 第二列：域名（渠道）/ 所属渠道（账号）。 */
  origin: string
  originSub: string
  /** 第三列：账号数 / Key 数 这类计数。 */
  counts: string
  status: string
  statusOK: boolean
  /** 搜索命中用的干草堆，全部小写。 */
  hay: string
  /** facet 筛选的取值（站型 / 余额状态）。 */
  facetValue: string
}

const channelRows = computed<Row[]>(() =>
  channels.list.map((c) => ({
    id: c.id,
    title: c.name,
    sub: `#${c.id} · ${familyLabel(c.site_family)}`,
    origin: host(c.base_url),
    originSub: c.base_url,
    counts: res.loaded
      ? `账号 ${res.channelAccounts(c.id).length} · Key ${res.channelKeys(c.id).length}`
      : '—',
    status: statusLabel(c.status),
    statusOK: c.status === 'enabled',
    hay: `${c.name} ${c.base_url} ${c.site_family} #${c.id} ${c.id}`.toLowerCase(),
    facetValue: c.site_family,
  })),
)

const accountRows = computed<Row[]>(() => {
  const scoped =
    props.scope.length === 0
      ? res.accounts
      : res.accounts.filter((a) => props.scope.includes(a.channel_id))
  return scoped.map((a) => {
    const c = channelByID.value.get(a.channel_id)
    const wallet = a.balance_group_key ?? ''
    return {
      id: a.id,
      title: res.accountLabel(a.id),
      // 上游用户 ID 缺席是真实情况（P1 不强制），写明白比留空好
      sub:
        a.external_user_id === undefined || a.external_user_id === ''
          ? '无上游用户 ID'
          : `上游用户 ID ${a.external_user_id}`,
      origin: c === undefined ? `渠道 #${a.channel_id}` : c.name,
      originSub: c === undefined ? '' : host(c.base_url),
      counts:
        `Key ${a.keys_active}/${a.keys_total}` + (wallet === '' ? '' : ` · 余额组 ${wallet}`),
      status: a.status === 'active' ? '可用' : statusLabel(a.status),
      statusOK: a.status === 'active',
      hay: `${res.accountLabel(a.id)} ${a.external_user_id ?? ''} ${wallet} ${
        c?.name ?? ''
      } ${c?.base_url ?? ''} #${a.id} ${a.id}`.toLowerCase(),
      facetValue: a.balance_usd === undefined ? 'unknown' : 'known',
    }
  })
})

/**
 * Key 行。第二列给**所属渠道**，第三列给分组与倍率 —— 那是选 Key 时真正要
 * 看的两件事（同一渠道下不同分组能调的模型与计费倍率都不一样）。
 *
 * 解析不出分组的 Key 标成 `statusOK=false` 并写明原因：它按定义筛不出任何
 * 模型，让人选中只会得到一个无法解释的空列表（见 04 §3.1bis）。
 */
const keyRows = computed<Row[]>(() => {
  const scoped =
    props.scope.length === 0
      ? res.keys
      : res.keys.filter((k) => props.scope.includes(k.channel_id))
  return scoped.map((k) => {
    const c = channelByID.value.get(k.channel_id)
    const grouped = k.group_ref !== undefined && k.group_ref !== ''
    const rate = k.rate_dynamic === true
      ? dynamicRateText(k.rate_min, k.rate_max)
      : k.rate_multiplier === undefined
        ? '倍率未采到'
        : `×${k.rate_multiplier}`
    return {
      id: k.id,
      title: k.secret_prefix,
      sub: grouped
        ? `分组 ${k.group_ref}${k.group_inherited === true ? '（跟账号）' : ''}`
        : '未归组 —— 筛不出模型',
      origin: c === undefined ? `渠道 #${k.channel_id}` : c.name,
      originSub: c === undefined ? '' : host(c.base_url),
      counts: grouped ? rate : '—',
      status: grouped ? '可筛' : '筛不了',
      statusOK: grouped,
      hay: `${k.secret_prefix} ${k.group_ref ?? ''} ${c?.name ?? ''} ${
        c?.base_url ?? ''
      } #${k.id} ${k.id}`.toLowerCase(),
      facetValue: grouped ? 'grouped' : 'ungrouped',
    }
  })
})

const rows = computed(() => {
  if (props.kind === 'channel') return channelRows.value
  if (props.kind === 'key') return keyRows.value
  return accountRows.value
})

/** facet 下拉的选项，从当前数据现算 —— 写死的那份在加站型时不会报错，只是选不到。 */
const facetOptions = computed(() => {
  if (props.kind === 'key') {
    return [
      { value: 'grouped', label: '能解析出分组' },
      { value: 'ungrouped', label: '未归组（筛不了）' },
    ]
  }
  if (props.kind === 'account') {
    return [
      { value: 'known', label: '已采到余额' },
      { value: 'unknown', label: '余额未采集' },
    ]
  }
  const seen = new Set(rows.value.map((r) => r.facetValue))
  return [...seen].sort().map((f) => ({ value: f, label: familyLabel(f) }))
})

const shown = computed(() => {
  const needle = q.value.trim().toLowerCase()
  return rows.value.filter((r) => {
    if (facet.value !== '' && r.facetValue !== facet.value) return false
    if (state.value === 'ok' && !r.statusOK) return false
    if (state.value === 'off' && r.statusOK) return false
    return needle === '' || r.hay.includes(needle)
  })
})

/** 触发按钮上的文案。多选到 2 个以上就只报个数 —— 拼全名会把按钮撑出工具条。 */
const summary = computed(() => {
  const picked = props.modelValue
  if (picked.length === 0) return props.placeholder
  if (picked.length === 1) {
    const r = rows.value.find((x) => x.id === picked[0])
    return r === undefined ? `#${picked[0]}（已不存在）` : `${r.title}（${r.origin}）`
  }
  return `已选 ${picked.length} 个${noun.value}`
})

function show(): void {
  if (props.disabled) return
  draft.value = new Set(props.modelValue)
  q.value = ''
  facet.value = ''
  state.value = ''
  open.value = true
}

function toggle(id: number): void {
  if (!props.multi) {
    emit('update:modelValue', [id])
    open.value = false
    return
  }
  const next = new Set(draft.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  draft.value = next
}

/** 全选 / 反选 / 清空都只作用在**当前筛出的行**上 —— 筛完再全选是这个组合的用法。 */
function selectAll(): void {
  const next = new Set(draft.value)
  for (const r of shown.value) next.add(r.id)
  draft.value = next
}

function invert(): void {
  const next = new Set(draft.value)
  for (const r of shown.value) {
    if (next.has(r.id)) next.delete(r.id)
    else next.add(r.id)
  }
  draft.value = next
}

function clear(): void {
  draft.value = new Set()
}

function confirm(): void {
  emit('update:modelValue', [...draft.value].sort((a, b) => a - b))
  open.value = false
}

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') open.value = false
}

// 与 UiDrawer 同样的处理：开着的时候才挂监听，组件卸载时一定摘掉，
// 否则留下来的监听会吃掉后续所有 Esc。
watch(open, (isOpen) => {
  if (isOpen) window.addEventListener('keydown', onKey)
  else window.removeEventListener('keydown', onKey)
})
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <button
    :id="id"
    type="button"
    class="picker-t"
    :class="{ empty: modelValue.length === 0 }"
    :disabled="disabled"
    :data-picker="kind"
    :data-picked="modelValue.join(',')"
    @click="show"
  >
    <span class="picker-sum">{{ summary }}</span>
    <span class="picker-caret" aria-hidden="true">▾</span>
  </button>

  <!-- v-if 而不是 v-show：关掉就该从 DOM 上消失，和 UiDrawer 同一条理由 -->
  <div v-if="open" class="drawer-scrim center" @click.self="open = false">
    <section class="picker" role="dialog" aria-modal="true" :aria-label="`选择${noun}`">
      <header class="picker-h">
        <div>
          <h2 class="drawer-t">选择{{ noun }}</h2>
          <p class="drawer-d">
            <template v-if="kind === 'channel'"
              >渠道名可能高度相似，<b>按域名核对</b>再选。</template
            >
            <template v-else>账号在 P1 没有名字，靠 <b>#id 与上游用户 ID</b> 辨认。</template>
            {{ multi ? '可多选。' : '单选，点一行即选定。' }}
          </p>
        </div>
        <button class="btn ghost sm drawer-x" aria-label="关闭" @click="open = false">✕</button>
      </header>

      <div class="picker-tb">
        <div class="tb-grow">
          <label class="sr" :for="`${id}-q`">搜索{{ noun }}</label>
          <input
            :id="`${id}-q`"
            v-model="q"
            :placeholder="kind === 'channel' ? '搜索名称 / 域名 / 站型 / #id…' : '搜索 #id / 上游用户 ID / 余额组 / 渠道…'"
          />
        </div>
        <select :id="`${id}-facet`" v-model="facet" :aria-label="kind === 'channel' ? '站型' : '余额'">
          <option value="">{{ kind === 'channel' ? '全部站型' : '全部余额状态' }}</option>
          <option v-for="f in facetOptions" :key="f.value" :value="f.value">{{ f.label }}</option>
        </select>
        <select :id="`${id}-state`" v-model="state" aria-label="状态">
          <option value="">全部状态</option>
          <option value="ok">{{ kind === 'channel' ? '已启用' : '可用' }}</option>
          <option value="off">已停用</option>
        </select>
      </div>

      <div v-if="multi" class="picker-bulk">
        <button class="btn outline sm" data-pick-all @click="selectAll">全选当前筛选</button>
        <button class="btn outline sm" data-pick-invert @click="invert">反选</button>
        <button class="btn outline sm" data-pick-clear @click="clear">清空</button>
        <span class="dim" data-pick-count>
          已选 {{ draft.size }} · 筛出 {{ shown.length }} / 全部 {{ rows.length }}
        </span>
      </div>
      <p v-else class="picker-bulk dim" data-pick-count>
        筛出 {{ shown.length }} / 全部 {{ rows.length }}
      </p>

      <div class="picker-b">
        <p v-if="rows.length === 0" class="note">
          还没有可选的{{ noun }}。<template v-if="kind === 'channel'"
            >先在渠道管理里新建渠道。</template
          ><template v-else>先登记账号，或换一个渠道范围。</template>
        </p>
        <p v-else-if="shown.length === 0" class="note">没有符合当前搜索与筛选的{{ noun }}</p>
        <table v-else class="picker-table">
          <thead>
            <tr>
              <th class="x"></th>
              <th>{{ kind === 'channel' ? '渠道' : '账号' }}</th>
              <th>{{ kind === 'channel' ? '域名' : '所属渠道' }}</th>
              <th>{{ kind === 'channel' ? '账号 / Key' : 'Key / 余额组' }}</th>
              <th>状态</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="r in shown"
              :key="r.id"
              :data-pick-row="r.id"
              :class="{ picked: multi ? draft.has(r.id) : modelValue.includes(r.id) }"
              @click="toggle(r.id)"
            >
              <td class="x">
                <input
                  :type="multi ? 'checkbox' : 'radio'"
                  :checked="multi ? draft.has(r.id) : modelValue.includes(r.id)"
                  :data-pick-box="r.id"
                  :aria-label="r.title"
                  @click.stop="toggle(r.id)"
                />
              </td>
              <td>
                <b>{{ r.title }}</b>
                <span class="cell-sub dim">{{ r.sub }}</span>
              </td>
              <td :title="r.originSub">
                {{ r.origin }}
                <span v-if="r.originSub !== '' && r.originSub !== r.origin" class="cell-sub dim">{{
                  r.originSub
                }}</span>
              </td>
              <td class="dim">{{ r.counts }}</td>
              <td>
                <span class="badge" :class="r.statusOK ? 'ok' : 'bad'">{{ r.status }}</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <footer class="drawer-f">
        <button v-if="multi" class="btn" data-pick-ok @click="confirm">
          确定（{{ draft.size }}）
        </button>
        <button
          v-if="!multi && modelValue.length > 0"
          class="btn outline"
          data-pick-clear
          @click="emit('update:modelValue', []); open = false"
        >
          清除选择
        </button>
        <button class="btn outline" data-pick-cancel @click="open = false">取消</button>
      </footer>
    </section>
  </div>
</template>
