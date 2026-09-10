<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiField from '@/components/ui/UiField.vue'
import * as adminApi from '@/api/admin'
import type { Account, ChannelGroup } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useToastStore } from '@/stores/toast'

const channels = useChannelsStore()
const toast = useToastStore()

/** 从详情跳来时预填渠道 ID，省一次手抄。 */
const accChannel = ref(channels.currentID === null ? '' : String(channels.currentID))
const accUID = ref('')
const accBalanceGroup = ref('')
const keyAccount = ref('')
const keySecret = ref('')
const keyRef = ref('')
const keyGroup = ref('')

const accounts = ref<Account[]>([])
const accountsLoaded = ref(false)
const groups = ref<ChannelGroup[]>([])
const groupsLoaded = ref(false)
const accountEditing = ref<number | null>(null)
const accountEditUID = ref('')
const accountEditGroup = ref('')
const accountDisabling = ref<number | null>(null)
const accountDisableReason = ref('')
const busy = ref(false)
let accountsRequest = 0
let groupsRequest = 0

watch(
  () => channels.currentID,
  (id) => {
    accountsRequest++
    groupsRequest++
    accChannel.value = id === null ? '' : String(id)
    accounts.value = []
    accountsLoaded.value = false
    groups.value = []
    groupsLoaded.value = false
    if (id !== null) void loadAccounts(id)
  },
)

onMounted(() => {
  if (channels.currentID !== null) void loadAccounts(channels.currentID)
})

async function loadAccounts(channelID = Number(accChannel.value)): Promise<void> {
  if (!Number.isFinite(channelID) || channelID <= 0) {
    toast.show('请填渠道 ID', 'bad')
    return
  }
  const request = ++accountsRequest
  try {
    const items = (await adminApi.listAccounts(channelID)).items
    if (channels.currentID !== null && channels.currentID !== channelID) return
    if (accountsRequest !== request) return
    accounts.value = items
    accountsLoaded.value = true
    await loadGroups(channelID)
  } catch (e) {
    toast.fail('加载账号失败', e)
  }
}

async function loadGroups(channelID: number): Promise<void> {
  const request = ++groupsRequest
  try {
    const items = (await adminApi.listGroups(channelID)).items
    if (channels.currentID !== null && channels.currentID !== channelID) return
    if (groupsRequest !== request) return
    groups.value = items
    groupsLoaded.value = true
  } catch (e) {
    toast.fail('加载分组失败', e)
  }
}

async function createAccount(): Promise<void> {
  const ch = Number(accChannel.value)
  if (!Number.isFinite(ch) || ch <= 0) {
    toast.show('请填渠道 ID', 'bad')
    return
  }
  try {
    const d = await adminApi.createAccount({
      channel_id: ch,
      external_user_id: accUID.value.trim(),
      balance_group_key: accBalanceGroup.value.trim(),
    })
    toast.show(`账号已创建：#${d.id}`, 'ok')
    // 顺手把新账号 ID 填进下一步，登记 Key 不用回头抄
    keyAccount.value = String(d.id)
    await loadAccounts(ch)
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
    const d = await adminApi.createKey({
      account_id: acc,
      secret: keySecret.value.trim(),
      external_ref: keyRef.value.trim(),
      group_ref: keyGroup.value,
    })
    toast.show(`Key 已登记：#${d.id}（明文不回显）`, 'ok')
    // 明文用完即清：留在输入框里等于把它留在了 DOM 上
    keySecret.value = ''
  } catch (e) {
    toast.fail('登记 Key 失败', e)
  }
}

function startAccountEdit(a: Account): void {
  accountDisabling.value = null
  accountEditing.value = a.id
  accountEditUID.value = a.external_user_id ?? ''
  accountEditGroup.value = a.balance_group_key ?? ''
}

function startAccountDisable(id: number): void {
  accountEditing.value = null
  accountDisabling.value = id
  accountDisableReason.value = ''
}

function cancelAccountAction(): void {
  accountEditing.value = null
  accountDisabling.value = null
}

async function saveAccount(id: number): Promise<void> {
  busy.value = true
  try {
    const ok = await adminApi.patchAccount(id, {
      external_user_id: accountEditUID.value.trim(),
      balance_group_key: accountEditGroup.value.trim(),
    })
    if (ok.updated) {
      toast.show(`账号 #${id} 已更新`, 'ok')
      cancelAccountAction()
      await loadAccounts()
    }
  } catch (e) {
    toast.fail('修改账号失败', e)
  } finally {
    busy.value = false
  }
}

async function confirmAccountDisable(id: number): Promise<void> {
  if (accountDisableReason.value.trim() === '') {
    toast.show('停用必须填原因', 'bad')
    return
  }
  busy.value = true
  try {
    await adminApi.patchAccount(id, {
      status: 'disabled',
      disabled_reason: accountDisableReason.value.trim(),
    })
    toast.show(`账号 #${id} 已停用`, 'ok')
    cancelAccountAction()
    await loadAccounts()
  } catch (e) {
    toast.fail('停用账号失败', e)
  } finally {
    busy.value = false
  }
}

async function enableAccount(id: number): Promise<void> {
  busy.value = true
  try {
    await adminApi.patchAccount(id, { status: 'active' })
    toast.show(`账号 #${id} 已启用`, 'ok')
    await loadAccounts()
  } catch (e) {
    toast.fail('启用账号失败', e)
  } finally {
    busy.value = false
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
        <UiField label="余额分组标识" for="acc-balance-group">
          <input id="acc-balance-group" v-model="accBalanceGroup" placeholder="可选" />
        </UiField>
        <div class="endcap">
          <button class="btn" id="btn-acc" @click="createAccount">创建账号</button>
          <button class="btn outline" id="btn-acc-load" @click="loadAccounts()">加载账号</button>
        </div>
      </div>
    </UiCard>

    <UiCard>
      <template #header>
        <h2 class="card-t">账号列表</h2>
        <span class="badge" id="account-count">{{ accountsLoaded ? `共 ${accounts.length} 个` : '—' }}</span>
        <div class="spacer"></div>
        <button class="btn outline sm" id="btn-account-reload" @click="loadAccounts()">刷新</button>
      </template>
      <UiEmpty v-if="!accountsLoaded">填入渠道 ID 后点「加载账号」</UiEmpty>
      <UiEmpty v-else-if="accounts.length === 0">该渠道还没有账号</UiEmpty>
      <div class="tw" v-else>
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th>上游用户 ID</th>
              <th>余额分组</th>
              <th>状态</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <template v-for="a in accounts" :key="a.id">
              <tr :data-account-row="a.id">
                <td>
                  <button class="btn ghost sm" :data-account-select="a.id" @click="keyAccount = String(a.id); void loadGroups(a.channel_id)">
                    #{{ a.id }}
                  </button>
                </td>
                <td>{{ a.external_user_id ?? '—' }}</td>
                <td class="dim">{{ a.balance_group_key ?? '—' }}</td>
                <td>
                  <span class="badge" :class="a.status === 'active' ? 'ok' : 'warn'">{{ a.status }}</span>
                  <span v-if="a.disabled_reason !== undefined" class="dim"> {{ a.disabled_reason }}</span>
                </td>
                <td class="acts">
                  <button class="btn ghost sm" :data-account-edit="a.id" @click="startAccountEdit(a)">编辑</button>
                  <button
                    v-if="a.status === 'active'"
                    class="btn ghost sm"
                    :data-account-disable="a.id"
                    @click="startAccountDisable(a.id)"
                  >停用</button>
                  <button v-else class="btn ghost sm" :data-account-enable="a.id" @click="enableAccount(a.id)">启用</button>
                </td>
              </tr>
              <tr v-if="accountEditing === a.id" class="sub-row">
                <td colspan="5">
                  <div class="row-form">
                    <UiField :label="`账号 #${a.id} 用户 ID`" :for="`account-edit-uid-${a.id}`">
                      <input :id="`account-edit-uid-${a.id}`" v-model="accountEditUID" />
                    </UiField>
                    <UiField label="余额分组标识" :for="`account-edit-group-${a.id}`">
                      <input :id="`account-edit-group-${a.id}`" v-model="accountEditGroup" />
                    </UiField>
                    <div class="endcap">
                      <button class="btn sm" :disabled="busy" :data-account-save="a.id" @click="saveAccount(a.id)">保存</button>
                      <button class="btn outline sm" @click="cancelAccountAction">取消</button>
                    </div>
                  </div>
                </td>
              </tr>
              <tr v-if="accountDisabling === a.id" class="sub-row">
                <td colspan="5">
                  <div class="row-form">
                    <UiField label="停用原因（必填）" :for="`account-disable-reason-${a.id}`">
                      <input :id="`account-disable-reason-${a.id}`" v-model="accountDisableReason" />
                    </UiField>
                    <div class="endcap">
                      <button class="btn danger sm" :disabled="busy" :data-account-disable-ok="a.id" @click="confirmAccountDisable(a.id)">确认停用</button>
                      <button class="btn outline sm" @click="cancelAccountAction">取消</button>
                    </div>
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
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
        <UiField label="所属分组" for="key-group">
          <select id="key-group" v-model="keyGroup" :disabled="!groupsLoaded">
            <option value="">不指定</option>
            <option v-for="g in groups" :key="g.id" :value="g.group_ref">
              {{ g.group_ref }}<template v-if="g.rate_multiplier !== undefined">（×{{ g.rate_multiplier }}）</template>
            </option>
          </select>
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
