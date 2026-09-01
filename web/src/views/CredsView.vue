<script setup lang="ts">
import { ref, watch } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import { useChannelsStore } from '@/stores/channels'
import { useCredentialsStore } from '@/stores/credentials'
import { useToastStore } from '@/stores/toast'
import { fmtTime } from '@/utils/format'

const channels = useChannelsStore()
const creds = useCredentialsStore()
const toast = useToastStore()

const channel = ref(channels.currentID === null ? '' : String(channels.currentID))
const token = ref('')
const uid = ref('')
const refresh = ref('')

watch(
  () => channels.currentID,
  (id) => {
    channel.value = id === null ? '' : String(id)
  },
)

async function save(): Promise<void> {
  const ch = Number(channel.value)
  if (!Number.isFinite(ch) || ch <= 0) {
    toast.show('请填渠道 ID', 'bad')
    return
  }
  const ok = await creds.save({
    channel_id: ch,
    access_token: token.value.trim(),
    refresh_token: refresh.value.trim(),
    external_user_id: uid.value.trim(),
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
        <UiField label="渠道 ID" for="cr-channel">
          <input id="cr-channel" v-model="channel" placeholder="从渠道列表取" />
        </UiField>
        <UiField label="访问令牌 / JWT" for="cr-token">
          <input
            id="cr-token"
            v-model="token"
            type="password"
            placeholder="NewAPI 系统令牌 或 JWT"
          />
        </UiField>
        <UiField label="上游用户 ID（NewAPI 必填）" for="cr-uid">
          <input id="cr-uid" v-model="uid" placeholder="用户 ID 头的值" />
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
                <th>站型</th>
                <th>类型</th>
                <th>状态</th>
                <th>已存</th>
                <th>令牌到期</th>
              </tr>
            </thead>
            <tbody>
              <!-- 一个渠道可能有多条凭证，key 用 channel_id + cred_type -->
              <tr v-for="c in creds.list" :key="`${c.channel_id}-${c.cred_type}`">
                <td>
                  <code>{{ c.channel_id }}</code>
                </td>
                <td>
                  <span class="badge">{{ c.site_family }}</span>
                </td>
                <td class="dim">{{ c.cred_type }}</td>
                <td>
                  <span class="badge" :class="c.status === 'valid' ? 'ok' : 'warn'">{{
                    c.status
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
