<script setup lang="ts">
/**
 * 登记账号 / 登记 Key 的抽屉。
 *
 * 取代原先那个独立的「账号与 Key」分栏。旧做法要求人先去渠道列表抄一个
 * 渠道 ID、再回来粘进输入框；登记 Key 还要再抄一次账号 ID。抽屉从哪一行
 * 打开就带哪一行的上下文，渠道与账号都是**下拉选择**，不再有可抄错的数字。
 *
 * 两个表单放同一个组件里，是因为它们共享同一条链路：建完账号通常紧接着
 * 就要给它登记 Key。建账号成功后直接把新账号 id 交给 Key 表单，
 * 少一次"它是几号来着"。
 */
import { computed, ref, watch } from 'vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiField from '@/components/ui/UiField.vue'
import * as adminApi from '@/api/admin'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'

/**
 * ⚠️ 属性名是 `channelId` / `accountId`，**不是 `channelID`**。
 *
 * 模板里写的是 `:channel-id="…"`，而 Vue 的 camelize 把 `channel-id` 变成
 * `channelId` —— 与 `channelID` 不相等，于是那个绑定会**静默地落进 attrs**，
 * 组件收到 undefined。表现是抽屉打开但一个字段都没预填，没有任何报错，
 * TS 也不报（模板里的 kebab 名不参与类型检查）。这里踩过一次。
 *
 * 仓库里其它地方的 `xxxID` 命名照旧 —— 那些是 TS 变量与 store 字段，
 * 不经过 kebab↔camel 这一道转换。只有**组件属性**受这条限制。
 */
const props = defineProps<{
  /** 'account' | 'key' | null（关闭）。 */
  mode: 'account' | 'key' | null
  /** 预置的渠道；0 = 让用户自己选。 */
  channelId?: number
  /** 预置的账号（仅 mode==='key'）；0 = 让用户自己选。 */
  accountId?: number
}>()
const emit = defineEmits<{ close: []; created: [] }>()

const channels = useChannelsStore()
const res = useResourcesStore()
const toast = useToastStore()

const accChannel = ref('')
const accUID = ref('')
const accWallet = ref('')
const keyChannel = ref('')
const keyAccount = ref('')
const keySecret = ref('')
const keyRef = ref('')
const keyGroup = ref('')
const busy = ref(false)

/** 打开时重置并吃掉上下文。不重置的话，上一次填了一半的明文会留在下一次。 */
watch(
  () => [props.mode, props.channelId, props.accountId],
  () => {
    const ch = props.channelId === undefined || props.channelId === 0 ? '' : String(props.channelId)
    if (props.mode === 'account') {
      accChannel.value = ch
      accUID.value = ''
      accWallet.value = ''
    } else if (props.mode === 'key') {
      const acc = props.accountId === undefined || props.accountId === 0 ? '' : String(props.accountId)
      keyAccount.value = acc
      // 由账号反推渠道：Key 挂在账号上，渠道是它的推论，不该让人再选一次
      const a = acc === '' ? undefined : res.accountByID.get(Number(acc))
      keyChannel.value = a !== undefined ? String(a.channel_id) : ch
      keySecret.value = ''
      keyRef.value = ''
      keyGroup.value = ''
    }
  },
  { immediate: true },
)

/** 选了渠道之后，账号下拉只列该渠道的账号 —— 跨渠道挂 Key 是建不出来的。 */
const keyAccountOptions = computed(() => {
  const ch = Number(keyChannel.value)
  if (!Number.isFinite(ch) || ch <= 0) return res.accounts
  return res.channelAccounts(ch)
})

const keyGroupOptions = computed(() => {
  const ch = Number(keyChannel.value)
  return Number.isFinite(ch) && ch > 0 ? res.channelGroups(ch) : []
})

/** 换渠道时清掉已选账号：留着的话会是别的渠道的账号，提交必然 400。 */
watch(keyChannel, () => {
  const acc = Number(keyAccount.value)
  if (!Number.isFinite(acc)) return
  const a = res.accountByID.get(acc)
  if (a !== undefined && String(a.channel_id) !== keyChannel.value) keyAccount.value = ''
})

const needsUID = computed(() => {
  const ch = channels.list.find((c) => c.id === Number(accChannel.value))
  if (ch === undefined) return false
  return channels.families.find((f) => f.family === ch.site_family)?.requires_external_user_id === true
})

async function createAccount(): Promise<void> {
  const ch = Number(accChannel.value)
  if (!Number.isFinite(ch) || ch <= 0) {
    toast.show('请选择渠道', 'bad')
    return
  }
  busy.value = true
  try {
    const d = await adminApi.createAccount({
      channel_id: ch,
      external_user_id: accUID.value.trim(),
      balance_group_key: accWallet.value.trim(),
    })
    toast.show(`账号已创建：#${d.id}`, 'ok')
    // 与登记 Key 同理：先关抽屉，再等重拉那一轮网络
    emit('close')
    emit('created')
    await res.reload()
  } catch (e) {
    toast.fail('创建账号失败', e)
  } finally {
    busy.value = false
  }
}

async function createKey(): Promise<void> {
  const acc = Number(keyAccount.value)
  if (!Number.isFinite(acc) || acc <= 0) {
    toast.show('请选择账号', 'bad')
    return
  }
  if (keySecret.value.trim() === '') {
    toast.show('Key 明文必填', 'bad')
    return
  }
  busy.value = true
  try {
    const d = await adminApi.createKey({
      account_id: acc,
      secret: keySecret.value.trim(),
      external_ref: keyRef.value.trim(),
      group_ref: keyGroup.value,
    })
    toast.show(`Key 已登记：#${d.id}（明文不回显）`, 'ok')
    // 明文用完即清：留在输入框里等于把它留在 DOM 上（FR-094）。
    // **先关抽屉再重拉**：reload 要等一轮网络，那段时间里明文输入框还挂在
    // DOM 上，而它已经没有任何用处了。关抽屉是 v-if，元素当场移除。
    keySecret.value = ''
    emit('close')
    emit('created')
    await res.reload()
  } catch (e) {
    toast.fail('登记 Key 失败', e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UiDrawer
    :open="mode === 'account'"
    title="登记上游账号"
    desc="账号是余额的归属方。一个渠道可以挂多个账号，一个账号可以挂多把 Key。"
    @close="emit('close')"
  >
    <UiField label="所属渠道" for="acc-channel">
      <select id="acc-channel" v-model="accChannel">
        <option value="">请选择…</option>
        <option v-for="c in channels.list" :key="c.id" :value="String(c.id)">
          {{ c.name }}（#{{ c.id }} · {{ c.site_family }}）
        </option>
      </select>
    </UiField>
    <UiField label="上游用户 ID" for="acc-uid">
      <input id="acc-uid" v-model="accUID" :placeholder="needsUID ? '本站型必填' : '可选'" />
    </UiField>
    <p class="note">
      采集请求要带「用户 ID 头」，缺了拿不到该账号的数据 ——
      <template v-if="needsUID">当前渠道的站型<b>要求</b>填它。</template>
      <template v-else>当前渠道的站型不强制要求。</template>
    </p>
    <UiField label="余额组（共享钱包标识，可选）" for="acc-balance-group">
      <input id="acc-balance-group" v-model="accWallet" placeholder="留空 = 独立钱包" />
    </UiField>
    <p class="note">
      多个账号在上游共用同一个钱包时填相同的值，汇总余额时只计一次（FR-022）。
      不确定就留空 —— 少去重会让合计虚高，而错误去重会让它虚低且无从察觉。
    </p>
    <template #footer>
      <button class="btn" id="btn-acc" :disabled="busy" @click="createAccount">创建账号</button>
      <button class="btn outline" @click="emit('close')">取消</button>
    </template>
  </UiDrawer>

  <UiDrawer
    :open="mode === 'key'"
    title="登记上游 Key"
    desc="明文只在提交时接收，之后一律只显示前缀（FR-094）。"
    @close="emit('close')"
  >
    <UiField label="所属渠道" for="key-channel">
      <select id="key-channel" v-model="keyChannel">
        <option value="">全部渠道</option>
        <option v-for="c in channels.list" :key="c.id" :value="String(c.id)">{{ c.name }}</option>
      </select>
    </UiField>
    <UiField label="所属账号" for="key-account">
      <select id="key-account" v-model="keyAccount">
        <option value="">请选择…</option>
        <option v-for="a in keyAccountOptions" :key="a.id" :value="String(a.id)">
          {{ res.accountLabel(a.id) }}
        </option>
      </select>
    </UiField>
    <p v-if="keyAccountOptions.length === 0" class="note bad">
      该渠道还没有账号。Key 必须挂在账号下，请先登记账号。
    </p>
    <UiField label="上游 Key（明文，仅此一次提交）" for="key-secret">
      <input id="key-secret" v-model="keySecret" type="password" placeholder="sk-…" autocomplete="off" />
    </UiField>
    <UiField label="上游侧 Key 标识 external_ref" for="key-ref">
      <input id="key-ref" v-model="keyRef" placeholder="用于把采到的用量写回这把 Key" />
    </UiField>
    <p class="note">
      <code>external_ref</code> 是采集匹配所需：采集只能拿到上游的 key id，拿不到明文，
      没有它用量写不回对应 Key，这把 Key 会一直显示「未采集」。
    </p>
    <UiField label="所属分组" for="key-group">
      <select id="key-group" v-model="keyGroup" :disabled="keyGroupOptions.length === 0">
        <option value="">不指定</option>
        <option v-for="g in keyGroupOptions" :key="g.id" :value="g.group_ref">
          {{ g.group_ref
          }}<template v-if="g.rate_multiplier !== undefined">（×{{ g.rate_multiplier }}）</template>
        </option>
      </select>
    </UiField>
    <p v-if="keyChannel === ''" class="note">选定渠道后才能选分组 —— 分组是渠道下的概念。</p>
    <p v-else-if="keyGroupOptions.length === 0" class="note">
      该渠道还没有采到分组。分组由采集获得（FR-123），先跑一次采集再来选，或者先留空。
    </p>
    <template #footer>
      <button class="btn" id="btn-key" :disabled="busy" @click="createKey">登记 Key</button>
      <button class="btn outline" @click="emit('close')">取消</button>
    </template>
  </UiDrawer>
</template>
