<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import * as adminApi from '@/api/admin'
import type { Account } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useCredentialsStore } from '@/stores/credentials'
import { useToastStore } from '@/stores/toast'
import { credentialTypeLabel, familyLabel, fmtTime, statusLabel } from '@/utils/format'

const channels = useChannelsStore()
const creds = useCredentialsStore()
const toast = useToastStore()

const accountID = ref('')
const token = ref('')
const refresh = ref('')
const accounts = ref<Account[]>([])
const accountsLoaded = ref(false)
let accountsRequest = 0

watch(
  () => channels.currentID,
  (id) => {
    accountsRequest++
    accountID.value = ''
    accounts.value = []
    accountsLoaded.value = false
    if (id !== null) void loadAccounts(id)
  },
)

onMounted(() => {
  if (channels.currentID !== null) void loadAccounts(channels.currentID)
})

async function loadAccounts(channelID = channels.currentID): Promise<void> {
  if (channelID === null) return
  const request = ++accountsRequest
  try {
    const items = (await adminApi.listAccounts(channelID)).items
    if (channels.currentID !== channelID || accountsRequest !== request) return
    accounts.value = items
    accountsLoaded.value = true
    const [only] = accounts.value
    if (only !== undefined && accounts.value.length === 1) accountID.value = String(only.id)
  } catch (e) {
    toast.fail('加载账号失败', e)
  }
}

async function save(): Promise<void> {
  const acc = Number(accountID.value)
  if (!Number.isFinite(acc) || acc <= 0) {
    toast.show('请选择账号', 'bad')
    return
  }
  const ok = await creds.save({
    account_id: acc,
    access_token: token.value.trim(),
    refresh_token: refresh.value.trim(),
  })
  if (ok) {
    // 秘密字段用完即清。
    token.value = ''
    refresh.value = ''
  }
}
</script>

<template>
  <div class="pane on" id="pane-creds">
    <UiCard desc="按站型填不同字段（04 §5）；凭证内容一律不回显">
      <template #header>
        <h2 class="card-t">登记采集凭证</h2>
        <span class="badge warn">采集必须先有它</span>
      </template>
      <div class="grid">
        <UiField label="账号" for="cr-account">
          <select id="cr-account" v-model="accountID" :disabled="!accountsLoaded">
            <option value="">请选择账号</option>
            <option v-for="a in accounts" :key="a.id" :value="String(a.id)">
              #{{ a.id }}（渠道 #{{ a.channel_id }}{{ a.external_user_id ? `，${a.external_user_id}` : '' }}）
            </option>
          </select>
        </UiField>
        <UiField label="访问令牌 / JWT" for="cr-token">
          <input
            id="cr-token"
            v-model="token"
            type="password"
            placeholder="NewAPI 系统令牌 或 JWT"
          />
        </UiField>
        <UiField label="refresh_token（Sub2API 可选）" for="cr-refresh">
          <input
            id="cr-refresh"
            v-model="refresh"
            type="password"
            placeholder="用于 24h JWT 自动续期"
          />
        </UiField>
        <div class="endcap">
          <button class="btn" id="btn-cred" @click="save">登记凭证</button>
          <button class="btn outline" id="btn-cred-accounts" @click="loadAccounts()">刷新账号</button>
        </div>
      </div>
      <p class="note">
        各家族的续期方式不同（04 §5）：NewAPI 长期令牌<b>不可运行时重新生成</b>
        （会作废正在用的那个）；Sub2API 24h JWT 用 refresh 续期、同账号串行。
      </p>
    </UiCard>

    <UiCard>
      <template #header>
        <h2 class="card-t">已登记凭证</h2>
        <span class="badge" id="cred-count">{{ creds.loaded ? `共 ${creds.count} 条` : '—' }}</span>
        <div class="spacer"></div>
        <button class="btn outline sm" id="btn-cred-reload" @click="creds.load()">刷新</button>
      </template>

      <div id="cred-list">
        <UiEmpty v-if="!creds.loaded">填入令牌后点「刷新」</UiEmpty>
        <UiEmpty v-else-if="creds.count === 0">还没有凭证，用上方表单登记</UiEmpty>
        <div class="tw" v-else>
          <table>
            <thead>
              <tr>
                <th>渠道</th>
                <th>账号</th>
                <th>站型</th>
                <th>类型</th>
                <th>状态</th>
                <th>已存</th>
                <th>令牌到期</th>
              </tr>
            </thead>
            <tbody>
              <!-- 凭证按账号归属，同一渠道可有多个相同类型的账号凭证。 -->
              <tr v-for="c in creds.list" :key="`${c.account_id}-${c.cred_type}`">
                <td>
                  <code>{{ c.channel_id }}</code>
                </td>
                <td>
                  <code>{{ c.account_id }}</code>
                </td>
                <td>
                  <span class="badge">{{ familyLabel(c.site_family) }}</span>
                </td>
                <td class="dim">{{ credentialTypeLabel(c.cred_type) }}</td>
                <td>
                  <span class="badge" :class="c.status === 'valid' ? 'ok' : 'warn'">{{
                    statusLabel(c.status)
                  }}</span>
                </td>
                <!-- 只报"有没有"，不报内容 -->
                <td class="dim">
                  {{ c.has_token ? '有令牌' : '' }}
                </td>
                <td class="dim">
                  {{ c.token_expires_at === undefined ? '长期' : fmtTime(c.token_expires_at) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </UiCard>
  </div>
</template>
