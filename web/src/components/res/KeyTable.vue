<script setup lang="ts">
/**
 * Key 表格。**四个地方共用同一份**：Key 管理页（平铺与分组）、渠道详情的
 * Key Tab、渠道列表的二级展开、账号行的二级展开。
 *
 * 为什么共用而不是各写一遍：这张表上挂着编辑/停用/删除三个写操作和一整套
 * 口径判定（不限额度 vs 配额耗尽 vs 未采集）。抄四份的话，
 * 「不限额度排在 0 之前」这条判定顺序迟早会有一份写反，而那一份看起来
 * 完全正常 —— 它只在无限额 Key 上出错。
 *
 * 列的可见性由 `cols` 控制，**单元格一律带 data-col**：验收脚本按语义名
 * 取单元格，而不是按下标 slice(4,6)。下标式断言在改一次列顺序后仍然会绿，
 * 只是从此验的是别的列。
 */
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import UiConfirm from '@/components/ui/UiConfirm.vue'
import QuotaCell from './QuotaCell.vue'
import * as adminApi from '@/api/admin'
import { api } from '@/api/client'
import type { Key } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'
import { RATE_LIMIT_HINT, rateLimitText } from '@/utils/money'
import { KEY_OPTIONAL_COLS } from '@/utils/keyfilter'
import { fmtAgo, fmtTime, keyStatusLabel } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    items: Key[]
    /** 打开的可选列。 */
    cols?: string[]
    /** 显示「所属渠道」列。全局 Key 页要，渠道内的表不要（那一列全一样）。 */
    showChannel?: boolean
    /** 显示「所属账号」列。 */
    showAccount?: boolean
    /** 紧凑模式：只留识别与风险两类信息，给二级展开区用。 */
    compact?: boolean
    emptyText?: string
  }>(),
  { cols: () => [], showChannel: false, showAccount: true, compact: false, emptyText: '没有 Key' },
)

const emit = defineEmits<{ changed: [] }>()

const router = useRouter()
const channels = useChannelsStore()
const res = useResourcesStore()
const toast = useToastStore()

const editing = ref<number | null>(null)
const editSecret = ref('')
const editRef = ref('')
const editRefOriginal = ref('')
const editGroupID = ref('')
const busy = ref(false)
const usage = ref('')
/** 待确认的危险操作。同时只会有一个。 */
const pending = ref<{ key: Key; action: 'disable' | 'delete' } | null>(null)

const has = (c: string): boolean => !props.compact && props.cols.includes(c)
const showChannelCol = computed(() => props.showChannel && !props.compact)
const showAccountCol = computed(() => props.showAccount)

function channelName(id: number): string {
  return channels.list.find((c) => c.id === id)?.name ?? `#${id}`
}

/** 跳到该渠道的详情。先切路由再 select —— 反过来会让详情页短暂显示上一个渠道。 */
async function openChannel(id: number): Promise<void> {
  await router.push({ name: 'channel-detail', params: { id: String(id) } })
}

/** 编辑行的 colspan。列数算错只是子行宽度不对，但会露出一格空白，很显眼。 */
const colCount = computed(() => {
  let n = 4 // 前缀 / 分组 / 配额 / 操作
  if (showChannelCol.value) n += 1
  if (showAccountCol.value) n += 1
  if (!props.compact) n += 2 // 上游限流 / 状态
  n += 1 // 最近同步
  for (const c of KEY_OPTIONAL_COLS) if (has(c.key)) n += 1
  return n
})

/** 收起点中的那个「更多」菜单。原生 <details> 选完不会自己关。 */
function closeMenu(e: MouseEvent): void {
  ;(e.target as HTMLElement | null)?.closest('details')?.removeAttribute('open')
}

function startEdit(k: Key): void {
  editing.value = k.id
  editSecret.value = ''
  editRef.value = k.external_ref ?? ''
  editRefOriginal.value = editRef.value
  editGroupID.value = k.channel_group_id === undefined ? '' : String(k.channel_group_id)
}

function cancelEdit(): void {
  editing.value = null
  // 明文用完即清：留在 v-model 里等于留在内存与 DOM 上（FR-094）
  editSecret.value = ''
}

async function saveEdit(k: Key): Promise<void> {
  const input: adminApi.PatchKeyInput = {}
  if (editSecret.value.trim() !== '') input.secret = editSecret.value.trim()
  if (editRef.value.trim() !== editRefOriginal.value.trim()) {
    input.external_ref = editRef.value.trim()
  }
  if (editGroupID.value === 'none') input.channel_group_id = null
  else if (editGroupID.value !== '') input.channel_group_id = Number(editGroupID.value)
  if (Object.keys(input).length === 0) {
    toast.show('至少修改一项', 'bad')
    return
  }
  busy.value = true
  try {
    await adminApi.patchKey(k.id, input)
    toast.show(`Key #${k.id} 已更新`, 'ok')
    cancelEdit()
    await res.reload()
    emit('changed')
  } catch (e) {
    toast.fail('修改 Key 失败', e)
  } finally {
    busy.value = false
  }
}

interface UsageResp {
  key_id: number
  count: number
  points?: unknown[]
}

async function showUsage(k: Key): Promise<void> {
  try {
    const u = await api<UsageResp>(`/admin/keys/${k.id}/usage`)
    if (u.count > 0 && u.points !== undefined && u.points.length > 0) {
      const last = u.points[u.points.length - 1]
      usage.value = `Key ${u.key_id} 用量点位 ${u.count} 个，最近：${JSON.stringify(last)}`
    } else {
      usage.value = `Key ${u.key_id} 暂无用量历史（采集后才有）`
    }
  } catch (e) {
    toast.fail('加载用量失败', e)
  }
}

/** 确认框的影响面文案。**只写算得出来的**，算不出就不写。 */
const pendingImpact = computed(() => {
  const p = pending.value
  if (p === null) return ''
  const where = `${channelName(p.key.channel_id)} · 账号 ${res.accountLabel(p.key.account_id)}`
  if (p.action === 'delete') {
    return `将从 ${where} 永久删除这把 Key。删除不可恢复；它的用量历史快照会保留，但不再关联到任何 Key。`
  }
  return `将把 ${where} 下的这把 Key 置为 revoked，它不再能承接请求（FR-031）。可以再登记一把新的，但停用本身不可撤销。`
})

async function runPending(): Promise<void> {
  const p = pending.value
  if (p === null) return
  busy.value = true
  try {
    if (p.action === 'delete') {
      await adminApi.deleteKey(p.key.id)
      toast.show(`Key #${p.key.id} 已删除`, 'ok')
    } else {
      await adminApi.disableKey(p.key.id)
      toast.show(`Key #${p.key.id} 已停用`, 'ok')
    }
    pending.value = null
    await res.reload()
    emit('changed')
  } catch (e) {
    toast.fail(p.action === 'delete' ? '删除 Key 失败' : '停用 Key 失败', e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UiEmpty v-if="items.length === 0">{{ emptyText }}</UiEmpty>
  <template v-else>
    <div class="tw">
      <table class="key-table">
        <!-- ⚠️ 表头也必须带 data-col，且与下面的 <td> 一一对应。
             窄屏靠 `th[data-col=x], td[data-col=x] { display:none }` 成对隐藏列 ——
             只给 td 加的话，表头留在原地而单元格少一个，**整行右移一格**，
             于是"状态"下面显示的是同步时刻。这种错位不会报任何错。 -->
        <thead>
          <tr>
            <th data-col="prefix">Key 前缀</th>
            <th v-if="showChannelCol" data-col="channel">所属渠道</th>
            <th v-if="showAccountCol" data-col="account">所属账号</th>
            <th v-if="has('ref')" data-col="ref">上游标识</th>
            <th data-col="group">分组</th>
            <th v-if="has('rate')" data-col="rate" class="n">倍率</th>
            <!-- 「Key 剩余配额」而不是「剩余额度」：它是使用约束不是资金。
                 多把 Key 的配额相加**不等于**账号余额（FR-022） -->
            <th
              data-col="quota"
              class="n"
              title="这把 Key 还被允许花多少。不限额的 Key 自己没有这个数，改显所属账号余额并在下方标注「账号余额」——两者不可混算。"
            >
              Key 剩余配额
            </th>
            <th v-if="!compact" data-col="rl" :title="RATE_LIMIT_HINT">上游限流</th>
            <th v-if="has('expiry')" data-col="expiry">有效期</th>
            <th v-if="!compact" data-col="status">状态</th>
            <th data-col="synced">最近同步</th>
            <th v-if="has('created')" data-col="created">登记时间</th>
            <th data-col="acts"></th>
          </tr>
        </thead>
        <tbody>
          <template v-for="k in items" :key="k.id">
            <tr :data-key-row="k.id">
              <td data-col="prefix">
                <code>{{ k.secret_prefix }}</code>
                <span class="dim cell-sub">#{{ k.id }}</span>
              </td>
              <td v-if="showChannelCol" data-col="channel">
                <button
                  class="linkish"
                  :data-key-channel="k.channel_id"
                  @click="openChannel(k.channel_id)"
                >
                  {{ channelName(k.channel_id) }}
                </button>
              </td>
              <td v-if="showAccountCol" data-col="account" class="dim">
                {{ res.accountLabel(k.account_id) }}
              </td>
              <td v-if="has('ref')" data-col="ref" class="dim">{{ k.external_ref ?? '—' }}</td>
              <td data-col="group">{{ k.group_ref ?? '—' }}</td>
              <td v-if="has('rate')" data-col="rate" class="n">
                <template v-if="k.rate_multiplier !== undefined">×{{ k.rate_multiplier }}</template>
                <span v-else class="dim">未知</span>
              </td>
              <!-- 传账号：不限额 Key 的配额格改显该账号余额（带口径注记）。
                   账号没拉到时传 undefined，QuotaCell 退回「不限额度」 -->
              <td data-col="quota" class="n">
                <QuotaCell
                  :item="k"
                  :account="res.accountByID.get(k.account_id)"
                  :bar="!compact"
                />
              </td>
              <!-- data-rl 保留：它是既有验收契约。data-col 是新增的语义名 -->
              <td
                v-if="!compact"
                data-col="rl"
                :data-rl="k.id"
                class="dim"
                :title="RATE_LIMIT_HINT"
              >
                {{ rateLimitText(k) }}
              </td>
              <td v-if="has('expiry')" data-col="expiry" class="dim">
                {{ k.expired_time === undefined ? '永不过期' : fmtTime(k.expired_time) }}
              </td>
              <td v-if="!compact" data-col="status">
                <span class="badge" :class="k.status === 'active' ? 'ok' : 'bad'" :data-key-status="k.status" :title="keyStatusLabel(k.status)">{{
                  keyStatusLabel(k.status)
                }}</span>
              </td>
              <td data-col="synced" class="dim" :title="fmtTime(k.quota_synced_at)">
                {{ fmtAgo(k.quota_synced_at) }}
              </td>
              <td v-if="has('created')" data-col="created" class="dim">
                {{ fmtTime(k.created_at) }}
              </td>
              <td data-col="acts" class="acts">
                <button class="btn ghost sm" :data-key-edit="k.id" @click="startEdit(k)">
                  编辑
                </button>
                <!-- 低频操作收进 details：一行四个按钮时，"删除"挨着"编辑"，
                     而它们的后果差着一个数量级。native <details> 不引依赖 -->
                <details class="more">
                  <summary class="btn ghost sm" :data-key-more="k.id">更多</summary>
                  <!-- 选完就收起：原生 details 不会自己关，留着一个悬在表格上
                       盖住下一行的菜单，下一次点哪一行都要先躲开它 -->
                  <div class="more-menu" @click="closeMenu">
                    <button class="btn ghost sm" :data-usage="k.id" @click="showUsage(k)">
                      用量历史
                    </button>
                    <button
                      class="btn ghost sm"
                      :data-dis="k.id"
                      @click="pending = { key: k, action: 'disable' }"
                    >
                      停用
                    </button>
                    <button
                      class="btn ghost sm danger-t"
                      :data-key-delete="k.id"
                      @click="pending = { key: k, action: 'delete' }"
                    >
                      删除
                    </button>
                  </div>
                </details>
              </td>
            </tr>
            <tr v-if="editing === k.id" class="sub-row">
              <td :colspan="colCount">
                <div class="row-form">
                  <UiField :label="`Key #${k.id} 新明文（可选，留空则不改）`" :for="`key-edit-secret-${k.id}`">
                    <input :id="`key-edit-secret-${k.id}`" v-model="editSecret" type="password" autocomplete="off" />
                  </UiField>
                  <UiField label="上游标识 external_ref" :for="`key-edit-ref-${k.id}`">
                    <input :id="`key-edit-ref-${k.id}`" v-model="editRef" />
                  </UiField>
                  <UiField label="所属分组" :for="`key-edit-group-${k.id}`">
                    <select :id="`key-edit-group-${k.id}`" v-model="editGroupID">
                      <option value="">不修改</option>
                      <option value="none">无分组</option>
                      <option
                        v-for="g in res.channelGroups(k.channel_id)"
                        :key="g.id"
                        :value="String(g.id)"
                      >
                        {{ g.group_ref
                        }}<template v-if="g.rate_multiplier !== undefined"
                          >（×{{ g.rate_multiplier }}）</template
                        >
                      </option>
                    </select>
                  </UiField>
                  <div class="endcap">
                    <button
                      class="btn sm"
                      :disabled="busy"
                      :data-key-save="k.id"
                      @click="saveEdit(k)"
                    >
                      保存
                    </button>
                    <button class="btn outline sm" @click="cancelEdit">取消</button>
                  </div>
                </div>
                <p class="note">
                  明文只在提交时接收，保存后一律只显示前缀（FR-094）。改明文即完成一次轮换，
                  没有独立的 rotate 入口（FR-122）。
                </p>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <div id="ku">
      <p class="note" v-if="usage !== ''">{{ usage }}</p>
    </div>

    <UiConfirm
      :open="pending !== null"
      :title="pending?.action === 'delete' ? '删除这把 Key？' : '停用这把 Key？'"
      :impact="pendingImpact"
      :confirm-text="pending?.action === 'delete' ? '确认删除' : '确认停用'"
      :busy="busy"
      @cancel="pending = null"
      @confirm="runPending"
    >
      <p v-if="pending !== null" class="dim">
        <code>{{ pending.key.secret_prefix }}</code>
        <template v-if="pending.key.group_ref !== undefined"> · 分组 {{ pending.key.group_ref }}</template>
      </p>
    </UiConfirm>
  </template>
</template>
