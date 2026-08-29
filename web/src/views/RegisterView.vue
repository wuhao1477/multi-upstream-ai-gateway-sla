<script setup lang="ts">
import { ref, watch } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiField from '@/components/ui/UiField.vue'
import { api } from '@/api/client'
import { useChannelsStore } from '@/stores/channels'
import { useToastStore } from '@/stores/toast'

const channels = useChannelsStore()
const toast = useToastStore()

/** 从详情跳来时预填渠道 ID，省一次手抄。 */
const accChannel = ref(channels.currentID === null ? '' : String(channels.currentID))
const accUID = ref('')
const keyAccount = ref('')
const keySecret = ref('')
const keyRef = ref('')

watch(
  () => channels.currentID,
  (id) => {
    accChannel.value = id === null ? '' : String(id)
  },
)

async function createAccount(): Promise<void> {
  const ch = Number(accChannel.value)
  if (!Number.isFinite(ch) || ch <= 0) {
    toast.show('请填渠道 ID', 'bad')
    return
  }
  try {
    const d = await api<{ id: number }>('/admin/accounts', {
      method: 'POST',
      body: { channel_id: ch, external_user_id: accUID.value.trim() },
    })
    toast.show(`账号已创建：#${d.id}`, 'ok')
    // 顺手把新账号 ID 填进下一步，登记 Key 不用回头抄
    keyAccount.value = String(d.id)
  } catch (e) {
    toast.fail('创建账号失败', e)
  }
}

async function createKey(): Promise<void> {
  const acc = Number(keyAccount.value)
  if (!Number.isFinite(acc) || acc <= 0 || keySecret.value.trim() === '') {
    toast.show('账号 ID 与 Key 必填', 'bad')
    return
  }
  try {
    const d = await api<{ id: number }>('/admin/keys', {
      method: 'POST',
      body: {
        account_id: acc,
        secret: keySecret.value.trim(),
        external_ref: keyRef.value.trim(),
      },
    })
    toast.show(`Key 已登记：#${d.id}（明文不回显）`, 'ok')
    // 明文用完即清：留在输入框里等于把它留在了 DOM 上
    keySecret.value = ''
  } catch (e) {
    toast.fail('登记 Key 失败', e)
  }
}
</script>

<template>
  <div class="pane on" id="pane-register">
    <UiCard
      title="登记账号"
      desc="NewAPI 系必须填上游用户 ID：采集请求要带「用户 ID 头」，缺了拿不到该账号的数据"
    >
      <div class="grid">
        <UiField label="渠道 ID" for="acc-channel">
          <input id="acc-channel" v-model="accChannel" placeholder="从渠道列表取" />
        </UiField>
        <UiField label="上游用户 ID" for="acc-uid">
          <input id="acc-uid" v-model="accUID" placeholder="NewAPI 必填（用户 ID 头的值）" />
        </UiField>
        <div class="endcap">
          <button class="btn" id="btn-acc" @click="createAccount">创建账号</button>
        </div>
      </div>
    </UiCard>

    <UiCard title="登记 Key" desc="明文只在提交时接收，列表一律只显示前缀（FR-094）">
      <div class="grid">
        <UiField label="账号 ID" for="key-account">
          <input id="key-account" v-model="keyAccount" placeholder="上一步返回的 id" />
        </UiField>
        <UiField label="上游 Key（明文，仅此一次提交）" for="key-secret">
          <input id="key-secret" v-model="keySecret" type="password" placeholder="sk-..." />
        </UiField>
        <UiField label="上游侧 Key 标识 external_ref" for="key-ref">
          <input id="key-ref" v-model="keyRef" placeholder="用于把采到的用量写回这把 Key" />
        </UiField>
        <div class="endcap">
          <button class="btn" id="btn-key" @click="createKey">登记 Key</button>
        </div>
      </div>
      <p class="note">
        <code>external_ref</code> 是采集匹配所需：采集只能拿到上游的 key id，拿不到明文，
        没有它用量写不回对应 Key。
      </p>
    </UiCard>
  </div>
</template>
