<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiField from '@/components/ui/UiField.vue'
import UiStat from '@/components/ui/UiStat.vue'
import * as adminApi from '@/api/admin'
import type { KeyProvisionBatchResult } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'

const props = defineProps<{
  mode: 'import' | 'provision' | null
  channelId?: number
  accountId?: number
}>()
const emit = defineEmits<{ close: []; changed: [] }>()

const channels = useChannelsStore()
const resources = useResourcesStore()
const toast = useToastStore()
const channelID = ref(0)
const accountID = ref(0)
const groupMode = ref<'all' | 'model'>('all')
const model = ref('')
const onlyWithoutKeys = ref(true)
const busy = ref(false)
const preview = ref<KeyProvisionBatchResult | null>(null)

watch(
  () => [props.mode, props.channelId, props.accountId],
  () => {
    channelID.value = props.channelId ?? 0
    accountID.value = props.accountId ?? 0
    groupMode.value = 'all'
    model.value = ''
    onlyWithoutKeys.value = true
    preview.value = null
  },
  { immediate: true },
)

const accounts = computed(() =>
  channelID.value === 0 ? resources.accounts : resources.channelAccounts(channelID.value),
)

watch(channelID, () => {
  const account = resources.accountByID.get(accountID.value)
  if (account !== undefined && channelID.value !== 0 && account.channel_id !== channelID.value) {
    accountID.value = 0
  }
})

watch([channelID, accountID, groupMode, model, onlyWithoutKeys], () => {
  preview.value = null
})

function request(): adminApi.KeyAutomationInput {
  return {
    channel_id: channelID.value,
    account_id: accountID.value,
    all: props.mode === 'provision' && channelID.value === 0 && accountID.value === 0,
    model: groupMode.value === 'model' ? model.value.trim() : '',
    only_without_keys: onlyWithoutKeys.value,
  }
}

async function syncExisting(): Promise<void> {
  if (channelID.value === 0) {
    toast.show('请选择渠道', 'bad')
    return
  }
  busy.value = true
  try {
    const result = await adminApi.importKeys(request())
    toast.show(
      `同步完成：发现 ${result.found}，新增 ${result.imported}，已有 ${result.skipped}，失败 ${result.failed + result.deferred}`,
      result.failed + result.deferred > 0 ? 'bad' : 'ok',
    )
    emit('changed')
    emit('close')
  } catch (error) {
    toast.fail('同步已有 Key 失败', error)
  } finally {
    busy.value = false
  }
}

async function loadPreview(): Promise<void> {
  if (groupMode.value === 'model' && model.value.trim() === '') {
    toast.show('请输入模型名称', 'bad')
    return
  }
  busy.value = true
  try {
    preview.value = await adminApi.provisionKeys(request(), true)
  } catch (error) {
    toast.fail('预览批量补齐失败', error)
  } finally {
    busy.value = false
  }
}

async function applyProvision(): Promise<void> {
  if (preview.value === null) return
  busy.value = true
  try {
    const result = await adminApi.provisionKeys(request(), false)
    preview.value = result
    toast.show(
      `补齐完成：远端创建 ${result.created}，本地登记 ${result.imported}，失败 ${result.failed}`,
      result.failed > 0 ? 'bad' : 'ok',
    )
    emit('changed')
  } catch (error) {
    toast.fail('批量补齐 Key 失败', error)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UiDrawer
    :open="mode !== null"
    :title="mode === 'import' ? '同步已有 Key' : '批量补齐 Key'"
    desc="使用已登记的账号采集凭证访问上游；Key 明文不会回显，单次最多处理 20 个账号。"
    @close="emit('close')"
  >
    <UiField label="渠道范围" for="key-auto-channel">
      <select id="key-auto-channel" v-model.number="channelID">
        <option :value="0">{{ mode === 'import' ? '请选择渠道' : '全部渠道' }}</option>
        <option v-for="channel in channels.list" :key="channel.id" :value="channel.id">
          {{ channel.name }}
        </option>
      </select>
    </UiField>
    <UiField label="账号范围" for="key-auto-account">
      <select id="key-auto-account" v-model.number="accountID">
        <option :value="0">全部账号</option>
        <option v-for="account in accounts" :key="account.id" :value="account.id">
          {{ resources.accountLabel(account.id) }}
        </option>
      </select>
    </UiField>

    <template v-if="mode === 'provision'">
      <UiField label="目标分组" for="key-auto-group-mode">
        <select id="key-auto-group-mode" v-model="groupMode">
          <option value="all">账号全部可用分组</option>
          <option value="model">拥有指定模型的分组</option>
        </select>
      </UiField>
      <UiField v-if="groupMode === 'model'" label="模型名称" for="key-auto-model">
        <input id="key-auto-model" v-model="model" placeholder="例如 gpt-5.5" />
      </UiField>
      <label class="chk">
        <input v-model="onlyWithoutKeys" type="checkbox" />
        仅处理远端没有任何 Key 的账号
      </label>
      <p class="note">未勾选时也只补缺少对应分组 Key 的账号，不会重复创建同组 Key。</p>

      <div v-if="preview !== null" class="stats">
        <UiStat label="账号" :value="preview.count" />
        <UiStat label="跳过账号" :value="preview.skipped_accounts" />
        <UiStat label="匹配分组" :value="preview.matched_groups" />
        <UiStat label="已有分组 Key" :value="preview.existing_groups" />
        <UiStat label="计划创建" :value="preview.would_create" />
        <UiStat v-if="preview.created > 0" label="已创建" :value="preview.created" />
        <UiStat v-if="preview.deferred > 0" label="待后续处理" :value="preview.deferred" />
        <UiStat v-if="preview.failed > 0" label="失败" :value="preview.failed" />
      </div>
    </template>

    <template #footer>
      <button v-if="mode === 'import'" class="btn" :disabled="busy" @click="syncExisting">
        {{ busy ? '同步中…' : '开始同步' }}
      </button>
      <template v-else>
        <button class="btn outline" :disabled="busy" @click="loadPreview">
          {{ busy ? '检查中…' : '预览' }}
        </button>
        <button class="btn" :disabled="busy || preview === null" @click="applyProvision">
          执行补齐
        </button>
      </template>
      <button class="btn outline" :disabled="busy" @click="emit('close')">取消</button>
    </template>
  </UiDrawer>
</template>
