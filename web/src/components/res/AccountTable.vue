<script setup lang="ts">
/**
 * 上游账号表格。账号页、渠道详情的账号 Tab、渠道列表二级展开共用。
 *
 * 这张表是**资金归属**的主界面：余额挂在账号上，不挂在渠道上，也不挂在
 * Key 上。多个账号可能共享同一个钱包（balance_group_key，FR-022），
 * 所以行上会打共享标记，汇总时按钱包去重 —— 否则一份钱会被算成好几份。
 *
 * 注意「上游账号」不是「平台用户」：P1 没有平台用户实体
 * （gateway_clients 已在 022 迁移里 DROP），这里管的全是**别人家站点上的
 * 账号**，用来采集与记账，不是登录本网关的人。
 */
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import UiConfirm from '@/components/ui/UiConfirm.vue'
import BalanceCell from './BalanceCell.vue'
import KeyTable from './KeyTable.vue'
import * as adminApi from '@/api/admin'
import type { Account } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'
import { sharesWallet } from '@/utils/money'
import { credentialTypeLabel, fmtTime, statusLabel } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    items: Account[]
    /** 显示「所属渠道」列。渠道内的表不需要（那一列全一样）。 */
    showChannel?: boolean
    /** 允许展开看该账号下的 Key。 */
    expandable?: boolean
    /**
     * 紧凑模式：给渠道行的二级展开区用。
     *
     * 展开区只有半屏宽，八列塞进去会横向滚动 —— 而横向滚动的表格没法竖着扫，
     * 速览的全部价值就没了。所以砍掉「余额组」（要看归属去账号页）与
     * 「+ Key」（那是账号页的动作），Key 数只留数字不留注解。
     */
    compact?: boolean
    emptyText?: string
  }>(),
  { showChannel: false, expandable: true, compact: false, emptyText: '还没有账号' },
)

const emit = defineEmits<{ addKey: [accountID: number]; editCred: [accountID: number] }>()

const router = useRouter()
const channels = useChannelsStore()
const res = useResourcesStore()
const toast = useToastStore()

const editing = ref<number | null>(null)
const editUID = ref('')
const editGroup = ref('')
const busy = ref(false)
const expanded = ref<Set<number>>(new Set())
/** 待确认的停用。启用不需要确认 —— 它只会放宽，不会让谁突然不可用。 */
const pending = ref<Account | null>(null)
const disableReason = ref('')

function channelName(id: number): string {
  return channels.list.find((c) => c.id === id)?.name ?? `#${id}`
}

async function openChannel(id: number): Promise<void> {
  await router.push({ name: 'channel-detail', params: { id: String(id) } })
}

/**
 * 展开行的 colspan。按"哪些列真的渲染了"逐项加，不要写成一个基数加减 ——
 * 加一列时那个基数是最容易漏改的东西，而漏改的表现是展开区错位一格。
 */
const colCount = computed(
  () =>
    6 + // 账号 / 上游用户 ID / 账号余额 / Key 数 / 状态 / 动作
    (props.compact ? 0 : 2) + // 余额组 + 采集凭证，紧凑模式都砍掉
    (props.showChannel ? 1 : 0) +
    (props.expandable ? 1 : 0),
)

function toggle(id: number): void {
  const next = new Set(expanded.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  expanded.value = next
}

function startEdit(a: Account): void {
  editing.value = a.id
  editUID.value = a.external_user_id ?? ''
  editGroup.value = a.balance_group_key ?? ''
}

async function saveEdit(id: number): Promise<void> {
  busy.value = true
  try {
    const ok = await adminApi.patchAccount(id, {
      external_user_id: editUID.value.trim(),
      balance_group_key: editGroup.value.trim(),
    })
    if (ok.updated) {
      toast.show(`账号 #${id} 已更新`, 'ok')
      editing.value = null
      await res.reload()
    }
  } catch (e) {
    toast.fail('修改账号失败', e)
  } finally {
    busy.value = false
  }
}

/**
 * 停用的影响面。用的是 keys_active 而不是 keys_total ——
 * 已经 revoked 的 Key 再被账号停用一次，对运维没有增量信息。
 */
const pendingImpact = computed(() => {
  const a = pending.value
  if (a === null) return ''
  const n = a.keys_active
  if (n === 0) return '该账号下没有可用 Key，停用不影响任何凭证。'
  return `将影响该账号下 ${n} 把可用 Key —— 它们所属的账号被停用后不再参与采集与记账。`
})

async function confirmDisable(): Promise<void> {
  const a = pending.value
  if (a === null) return
  // 原因必填（FR-095）。服务端也会拦，这里先拦是为了少一次往返。
  if (disableReason.value.trim() === '') {
    toast.show('停用必须填原因（FR-095）', 'bad')
    return
  }
  busy.value = true
  try {
    await adminApi.patchAccount(a.id, {
      status: 'disabled',
      disabled_reason: disableReason.value.trim(),
    })
    toast.show(`账号 #${a.id} 已停用`, 'ok')
    pending.value = null
    await res.reload()
  } catch (e) {
    toast.fail('停用账号失败', e)
  } finally {
    busy.value = false
  }
}

async function enable(id: number): Promise<void> {
  busy.value = true
  try {
    await adminApi.patchAccount(id, { status: 'active' })
    toast.show(`账号 #${id} 已启用`, 'ok')
    await res.reload()
  } catch (e) {
    toast.fail('启用账号失败', e)
  } finally {
    busy.value = false
  }
}

function openDisable(a: Account): void {
  pending.value = a
  disableReason.value = ''
}
</script>

<template>
  <UiEmpty v-if="items.length === 0">{{ emptyText }}</UiEmpty>
  <div v-else class="tw">
    <table class="acc-table">
      <thead>
        <!-- 表头带 data-col 且与 <td> 一一对应：窄屏成对隐藏列，
             只给 td 加会让整行右移一格（见 KeyTable 同处注释）。 -->
        <tr>
          <th v-if="expandable" class="x"></th>
          <th data-col="account">账号</th>
          <th v-if="showChannel" data-col="channel">所属渠道</th>
          <th data-col="uid">上游用户 ID</th>
          <th v-if="!compact" data-col="wallet">余额组</th>
          <!-- 「账号余额」：这是钱。与 Key 的「剩余配额」是两个口径，
               二者不可相加也不互相蕴含 -->
          <th data-col="balance" class="n">账号余额</th>
          <th data-col="keys" class="n">Key 数</th>
          <!-- 采集凭证跟着账号行走（UNIQUE(account_id)，023 迁移）。
               它回答的是"这个账号采不采得了" —— 原先要去另一个分栏反查
               "谁不在列表里"才知道。 -->
          <th v-if="!compact" data-col="cred">采集凭证</th>
          <th data-col="status">状态</th>
          <th data-col="acts"></th>
        </tr>
      </thead>
      <tbody>
        <template v-for="a in items" :key="a.id">
          <tr :data-account-row="a.id" :class="{ open: expanded.has(a.id) }">
            <td v-if="expandable" class="x">
              <button
                class="twist"
                :data-account-toggle="a.id"
                :aria-expanded="expanded.has(a.id)"
                :aria-label="`展开账号 ${a.id} 的 Key`"
                @click="toggle(a.id)"
              >
                {{ expanded.has(a.id) ? '▾' : '▸' }}
              </button>
            </td>
            <td data-col="account">
              <code>#{{ a.id }}</code>
            </td>
            <td v-if="showChannel" data-col="channel">
              <button class="linkish" @click="openChannel(a.channel_id)">
                {{ channelName(a.channel_id) }}
              </button>
            </td>
            <td data-col="uid">
              {{ a.external_user_id ?? '—' }}
              <!-- 紧凑模式砍掉了余额组那一列，但共享标记必须留下 ——
                   它是"合计为什么比逐行相加小"的唯一解释 -->
              <span
                v-if="compact && sharesWallet(a, res.accounts)"
                class="badge"
                title="与其它账号共用同一个上游钱包，汇总时只计一次（FR-022）"
                >共享</span
              >
            </td>
            <td v-if="!compact" data-col="wallet" class="dim">
              {{ a.balance_group_key ?? '—' }}
              <!-- 共享钱包必须看得见：不然渠道页那个"账号余额合计"为什么
                   比几行相加要小，没人解释得了（FR-022 去重的可见形态） -->
              <span
                v-if="sharesWallet(a, res.accounts)"
                class="badge"
                title="与其它账号共用同一个上游钱包，汇总时只计一次（FR-022）"
                >共享</span
              >
            </td>
            <td data-col="balance" class="n"><BalanceCell :account="a" /></td>
            <td data-col="keys" class="n">
              <span :data-account-keys="a.id" :title="`${a.keys_active} 把可用 / 共 ${a.keys_total} 把`"
                >{{ a.keys_active }} / {{ a.keys_total }}</span
              >
              <span v-if="!compact" class="dim cell-sub">可用 / 总数</span>
            </td>
            <td v-if="!compact" data-col="cred">
              <!-- 缺凭证用红而不是灰：它不是"这一格没数据"，是**这个账号采不了**，
                   和渠道总览里 credential_missing 被划进阻断性异常是同一条理由。 -->
              <span
                v-if="a.cred_type === undefined"
                class="badge bad"
                :data-account-cred="a.id"
                data-cred="missing"
                >未登记</span
              >
              <template v-else>
                <span
                  class="badge"
                  :class="a.cred_status === 'valid' ? 'ok' : 'warn'"
                  :data-account-cred="a.id"
                  data-cred="has"
                  >{{ statusLabel(a.cred_status) }}</span
                >
                <span class="dim cell-sub">{{ credentialTypeLabel(a.cred_type) }}</span>
                <span class="dim cell-sub">{{
                  a.cred_expires_at === undefined ? '长期' : `到期 ${fmtTime(a.cred_expires_at)}`
                }}</span>
              </template>
            </td>
            <td data-col="status">
              <span class="badge" :class="a.status === 'active' ? 'ok' : 'bad'">{{
                statusLabel(a.status)
              }}</span>
              <span v-if="a.disabled_reason !== undefined" class="dim cell-sub">{{
                a.disabled_reason
              }}</span>
            </td>
            <td data-col="acts" class="acts">
              <button class="btn ghost sm" :data-account-edit="a.id" @click="startEdit(a)">
                编辑
              </button>
              <button
                v-if="!compact"
                class="btn ghost sm"
                :data-account-cred-edit="a.id"
                @click="emit('editCred', a.id)"
              >
                {{ a.cred_type === undefined ? '登记凭证' : '换凭证' }}
              </button>
              <button
                v-if="!compact"
                class="btn ghost sm"
                :data-account-add-key="a.id"
                @click="emit('addKey', a.id)"
              >
                + Key
              </button>
              <button
                v-if="a.status === 'active'"
                class="btn ghost sm danger-t"
                :data-account-disable="a.id"
                @click="openDisable(a)"
              >
                停用
              </button>
              <button
                v-else
                class="btn ghost sm"
                :data-account-enable="a.id"
                :disabled="busy"
                @click="enable(a.id)"
              >
                启用
              </button>
            </td>
          </tr>

          <tr v-if="editing === a.id" class="sub-row">
            <td :colspan="colCount">
              <div class="row-form">
                <UiField :label="`账号 #${a.id} 上游用户 ID`" :for="`account-edit-uid-${a.id}`">
                  <input :id="`account-edit-uid-${a.id}`" v-model="editUID" />
                </UiField>
                <UiField label="余额组（共享钱包标识）" :for="`account-edit-group-${a.id}`">
                  <input
                    :id="`account-edit-group-${a.id}`"
                    v-model="editGroup"
                    placeholder="留空 = 独立钱包"
                  />
                </UiField>
                <div class="endcap">
                  <button class="btn sm" :disabled="busy" :data-account-save="a.id" @click="saveEdit(a.id)">
                    保存
                  </button>
                  <button class="btn outline sm" @click="editing = null">取消</button>
                </div>
              </div>
              <p class="note">
                余额组相同的账号在上游共用同一个钱包，汇总余额时只计一次（FR-022）。
                填错会让合计虚高或虚低，但不影响单个账号的余额显示。
                该账号登记于 {{ fmtTime(a.created_at) }}。
              </p>
            </td>
          </tr>

          <!-- 二级：该账号下的 Key。按需渲染，不展开就不进 DOM -->
          <tr v-if="expandable && expanded.has(a.id)" class="sub-row">
            <td :colspan="colCount">
              <div class="sec-t">
                该账号下的 Key
                <span class="badge">{{ res.accountKeys(a.id).length }} 把</span>
              </div>
              <KeyTable
                :items="res.accountKeys(a.id)"
                :show-account="false"
                compact
                empty-text="该账号还没有登记 Key"
              />
            </td>
          </tr>
        </template>
      </tbody>
    </table>
  </div>

  <UiConfirm
    :open="pending !== null"
    title="停用这个上游账号？"
    :impact="pendingImpact"
    confirm-text="确认停用"
    :busy="busy"
    @cancel="pending = null"
    @confirm="confirmDisable"
  >
    <UiField
      v-if="pending !== null"
      label="停用原因（必填，FR-095）"
      :for="`account-disable-reason-${pending.id}`"
    >
      <input
        :id="`account-disable-reason-${pending.id}`"
        v-model="disableReason"
        placeholder="例：站点跑路 / 余额耗尽待充值"
      />
    </UiField>
  </UiConfirm>
</template>
