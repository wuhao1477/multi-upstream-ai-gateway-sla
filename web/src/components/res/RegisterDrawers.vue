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
import ScopePicker from '@/components/res/ScopePicker.vue'
import * as adminApi from '@/api/admin'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'

/**
 * ⚠️ 属性名是 `channelIds` / `accountIds`，**不是 `channelIDs`**。
 *
 * 模板里写的是 `:channel-ids="…"`，而 Vue 的 camelize 把 `channel-ids` 变成
 * `channelIds` —— 与 `channelIDs` 不相等，于是那个绑定会**静默地落进 attrs**，
 * 组件收到 undefined。表现是抽屉打开但一个字段都没预填，没有任何报错，
 * TS 也不报（模板里的 kebab 名不参与类型检查）。这里踩过一次。
 *
 * 仓库里其它地方的 `xxxIDs` 命名照旧 —— 那些是 TS 变量与 store 字段，
 * 不经过 kebab↔camel 这一道转换。只有**组件属性**受这条限制。
 */
const props = defineProps<{
  /** 'account' | 'key' | null（关闭）。 */
  mode: 'account' | 'key' | null
  /** Key 管理页当前的渠道筛选；取第一个作为预置值，空 = 让用户自己选。 */
  channelIds?: number[]
  /** 同上，账号（仅 mode==='key'）。 */
  accountIds?: number[]
}>()
const emit = defineEmits<{ close: []; created: [] }>()

const channels = useChannelsStore()
const res = useResourcesStore()
const toast = useToastStore()

/**
 * 三个选择器都收 `number[]`（ScopePicker 的统一形状），但这两张表单
 * **本质是单选**：账号只能挂在一个渠道下，Key 只能挂在一个账号下。
 * 所以选择器都以 `multi=false` 渲染，数组长度不会超过 1。
 */
const accChannelIDs = ref<number[]>([])
const accUID = ref('')
const accWallet = ref('')
const keyChannelIDs = ref<number[]>([])
const keyAccountIDs = ref<number[]>([])
const keySecret = ref('')
const keyRef = ref('')
const keyGroup = ref('')
const busy = ref(false)

const accChannelID = computed(() => accChannelIDs.value[0] ?? 0)
const keyChannelID = computed(() => keyChannelIDs.value[0] ?? 0)
const keyAccountID = computed(() => keyAccountIDs.value[0] ?? 0)

/** 打开时重置并吃掉上下文。不重置的话，上一次填了一半的明文会留在下一次。 */
watch(
  () => [props.mode, props.channelIds, props.accountIds],
  () => {
    const ch = props.channelIds?.[0] ?? 0
    if (props.mode === 'account') {
      accChannelIDs.value = ch === 0 ? [] : [ch]
      accUID.value = ''
      accWallet.value = ''
    } else if (props.mode === 'key') {
      const acc = props.accountIds?.[0] ?? 0
      keyAccountIDs.value = acc === 0 ? [] : [acc]
      // 由账号反推渠道：Key 挂在账号上，渠道是它的推论，不该让人再选一次
      const a = acc === 0 ? undefined : res.accountByID.get(acc)
      const inferred = a?.channel_id ?? ch
      keyChannelIDs.value = inferred === 0 ? [] : [inferred]
      keySecret.value = ''
      keyRef.value = ''
      keyGroup.value = ''
    }
  },
  { immediate: true, deep: true },
)

const keyGroupOptions = computed(() =>
  keyChannelID.value > 0 ? res.channelGroups(keyChannelID.value) : [],
)

/** 换渠道时清掉已选账号：留着的话会是别的渠道的账号，提交必然 400。 */
watch(keyChannelID, (ch) => {
  const a = res.accountByID.get(keyAccountID.value)
  if (a !== undefined && a.channel_id !== ch) keyAccountIDs.value = []
})

/** 反过来：先选账号时把渠道补上 —— 分组下拉是按渠道列的，缺了它就永远是空。 */
watch(keyAccountID, (id) => {
  const a = res.accountByID.get(id)
  if (a !== undefined && a.channel_id !== keyChannelID.value) keyChannelIDs.value = [a.channel_id]
})

const needsUID = computed(() => {
  const ch = channels.list.find((c) => c.id === accChannelID.value)
  if (ch === undefined) return false
  return channels.families.find((f) => f.family === ch.site_family)?.requires_external_user_id === true
})

async function createAccount(): Promise<void> {
  const ch = accChannelID.value
  if (ch <= 0) {
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
  const acc = keyAccountID.value
  if (acc <= 0) {
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
      <ScopePicker id="acc-channel" kind="channel" v-model="accChannelIDs" />
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
      <ScopePicker
        id="key-channel"
        kind="channel"
        v-model="keyChannelIDs"
        placeholder="全部渠道"
      />
    </UiField>
    <UiField label="所属账号" for="key-account">
      <ScopePicker
        id="key-account"
        kind="account"
        v-model="keyAccountIDs"
        :scope="keyChannelIDs"
      />
    </UiField>
    <p v-if="keyChannelID > 0 && res.channelAccounts(keyChannelID).length === 0" class="note bad">
      该渠道还没有账号。Key 必须挂在账号下，请先登记账号。
    </p>
    <UiField label="上游 Key（明文，仅此一次提交）" for="key-secret">
      <input id="key-secret" v-model="keySecret" type="password" placeholder="sk-…" autocomplete="off" />
    </UiField>
    <UiField label="上游侧 Key 标识 external_ref" for="key-ref">
      <input id="key-ref" v-model="keyRef" placeholder="用于把采到的用量写回这把 Key" />
    </UiField>
    <p class="note">
      手工登记时建议填写 <code>external_ref</code>，采集会用它匹配上游 Key；
      「同步已有 Key」会自动读取并登记该标识。
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
    <p v-if="keyChannelID === 0" class="note">选定渠道后才能选分组 —— 分组是渠道下的概念。</p>
    <p v-else-if="keyGroupOptions.length === 0" class="note">
      该渠道还没有采到分组。分组由采集获得（FR-123），先跑一次采集再来选，或者先留空。
    </p>
    <template #footer>
      <button class="btn" id="btn-key" :disabled="busy" @click="createKey">登记 Key</button>
      <button class="btn outline" @click="emit('close')">取消</button>
    </template>
  </UiDrawer>
</template>
