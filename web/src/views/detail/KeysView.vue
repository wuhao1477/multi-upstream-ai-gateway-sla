<script setup lang="ts">
import { onMounted, ref } from 'vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import * as adminApi from '@/api/admin'
import { api } from '@/api/client'
import type { Key } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useToastStore } from '@/stores/toast'

const channels = useChannelsStore()
const toast = useToastStore()

const keys = ref<Key[]>([])
const loaded = ref(false)
const usage = ref('')

async function load(): Promise<void> {
  const id = channels.currentID
  if (id === null) return
  try {
    const d = await adminApi.listKeys(id)
    keys.value = d.items
  } catch (e) {
    toast.fail('加载 Key 失败', e)
  } finally {
    loaded.value = true
  }
}

onMounted(load)

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

async function disable(k: Key): Promise<void> {
  if (!confirm('停用后该 Key 不再可用，确定？')) return
  try {
    await api<unknown>(`/admin/keys/${k.id}/disable`, { method: 'POST' })
    toast.show('已停用', 'ok')
    await load()
  } catch (e) {
    toast.fail('停用失败', e)
  }
}
</script>

<template>
  <UiEmpty v-if="loaded && keys.length === 0">还没有 Key，去「账号与 Key」登记</UiEmpty>
  <template v-else-if="keys.length > 0">
    <div class="sec-t">Key <span class="badge">{{ keys.length }} 把</span></div>
    <div class="tw">
      <table>
        <thead>
          <tr>
            <th>ID</th>
            <th>前缀</th>
            <th>上游标识</th>
            <th>剩余额度</th>
            <!-- FR-127：P1 只登记与展示，判闸属 P3（FR-028/029/032）。
                 采到才有值 —— 实测 newapi 的 /api/token 不给 Key 级 RPM，
                 sub2api 给并发（sub2api.go:183）。所以"—"是真实信息：
                 该站型没暴露，不是我们没采。 -->
            <th>上游限流</th>
            <th>状态</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="k in keys" :key="k.id">
            <td>
              <code>{{ k.id }}</code>
            </td>
            <td>
              <code>{{ k.secret_prefix }}</code>
            </td>
            <td class="dim">{{ k.external_ref ?? '—' }}</td>
            <td>
              <template v-if="k.remain_quota_usd !== undefined"
                >${{ k.remain_quota_usd.toFixed(4) }}</template
              >
              <span v-else class="dim">未采集</span>
            </td>
            <td :data-rl="k.id">
              <template v-if="k.rpm_limit !== undefined">{{ k.rpm_limit }} rpm</template>
              <template v-if="k.rpm_limit !== undefined && k.concurrency_limit !== undefined">
                /
              </template>
              <template v-if="k.concurrency_limit !== undefined"
                >{{ k.concurrency_limit }} 并发</template
              >
              <span
                v-if="k.rpm_limit === undefined && k.concurrency_limit === undefined"
                class="dim"
                >—</span
              >
            </td>
            <td>
              <span class="badge" :class="k.status === 'active' ? 'ok' : 'warn'">{{
                k.status
              }}</span>
            </td>
            <td style="white-space: nowrap">
              <button class="btn ghost sm" :data-usage="k.id" @click="showUsage(k)">用量</button>
              <button class="btn ghost sm" :data-dis="k.id" @click="disable(k)">停用</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div id="ku">
      <p class="note" v-if="usage !== ''">{{ usage }}</p>
    </div>
    <p class="note">
      只显示前缀，完整凭证永不回显（FR-094）。「上游限流」是上游施加的 Key 级上限，P1
      只登记展示、不判闸
    </p>
  </template>
</template>
