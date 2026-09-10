<script setup lang="ts">
import { onMounted, ref } from 'vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import * as adminApi from '@/api/admin'
import { api } from '@/api/client'
import type { ChannelGroup, Key } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useToastStore } from '@/stores/toast'

const channels = useChannelsStore()
const toast = useToastStore()

const keys = ref<Key[]>([])
const groups = ref<ChannelGroup[]>([])
const loaded = ref(false)
const usage = ref('')
const editing = ref<number | null>(null)
const editSecret = ref('')
const editRef = ref('')
const editRefOriginal = ref('')
const editGroupID = ref('')
const busy = ref(false)
let loadRequest = 0

async function load(): Promise<void> {
  const id = channels.currentID
  if (id === null) return
  const request = ++loadRequest
  await Promise.all([loadKeys(id), loadGroups(id)])
  if (channels.currentID !== id || loadRequest !== request) return
  loaded.value = true
}

async function loadKeys(channelID: number): Promise<void> {
  try {
    const items = (await adminApi.listKeys(channelID)).items
    if (channels.currentID !== channelID) return
    keys.value = items
  } catch (e) {
    toast.fail('加载 Key 失败', e)
  }
}

async function loadGroups(channelID: number): Promise<void> {
  try {
    const items = (await adminApi.listGroups(channelID)).items
    if (channels.currentID !== channelID) return
    groups.value = items
  } catch (e) {
    toast.fail('加载分组失败', e)
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
    await adminApi.disableKey(k.id)
    toast.show('已停用', 'ok')
    await load()
  } catch (e) {
    toast.fail('停用失败', e)
  }
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
    await load()
  } catch (e) {
    toast.fail('修改 Key 失败', e)
  } finally {
    busy.value = false
  }
}

async function remove(k: Key): Promise<void> {
  if (!confirm('删除后无法恢复，确定删除这把 Key？')) return
  try {
    await adminApi.deleteKey(k.id)
    toast.show('Key 已删除', 'ok')
    await load()
  } catch (e) {
    toast.fail('删除 Key 失败', e)
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
            <th>账号</th>
            <th>前缀</th>
            <th>上游标识</th>
            <th>分组</th>
            <th>倍率</th>
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
          <template v-for="k in keys" :key="k.id">
          <tr :data-key-row="k.id">
            <td>
              <code>{{ k.id }}</code>
            </td>
            <td><code>{{ k.account_id }}</code></td>
            <td>
              <code>{{ k.secret_prefix }}</code>
            </td>
            <td class="dim">{{ k.external_ref ?? '—' }}</td>
            <td>{{ k.group_ref ?? '—' }}</td>
            <td>
              <template v-if="k.rate_multiplier !== undefined">×{{ k.rate_multiplier }}</template>
              <span v-else class="dim">未知</span>
            </td>
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
              <button class="btn ghost sm" :data-key-edit="k.id" @click="startEdit(k)">编辑</button>
              <button class="btn ghost sm" :data-key-delete="k.id" @click="remove(k)">删除</button>
              <button class="btn ghost sm" :data-dis="k.id" @click="disable(k)">停用</button>
            </td>
          </tr>
          <tr v-if="editing === k.id" class="sub-row">
            <td colspan="10">
              <div class="row-form">
                <UiField :label="`Key #${k.id} 新明文（可选）`" :for="`key-edit-secret-${k.id}`">
                  <input :id="`key-edit-secret-${k.id}`" v-model="editSecret" type="password" />
                </UiField>
                <UiField label="上游标识" :for="`key-edit-ref-${k.id}`">
                  <input :id="`key-edit-ref-${k.id}`" v-model="editRef" />
                </UiField>
                <UiField label="所属分组" :for="`key-edit-group-${k.id}`">
                  <select :id="`key-edit-group-${k.id}`" v-model="editGroupID">
                    <option value="">不修改</option>
                    <option value="none">无分组</option>
                    <option v-for="g in groups" :key="g.id" :value="String(g.id)">
                      {{ g.group_ref }}<template v-if="g.rate_multiplier !== undefined">（×{{ g.rate_multiplier }}）</template>
                    </option>
                  </select>
                </UiField>
                <div class="endcap">
                  <button class="btn sm" :disabled="busy" :data-key-save="k.id" @click="saveEdit(k)">保存</button>
                  <button class="btn outline sm" @click="cancelEdit">取消</button>
                </div>
              </div>
            </td>
          </tr>
          </template>
        </tbody>
      </table>
    </div>
    <div id="ku">
      <p class="note" v-if="usage !== ''">{{ usage }}</p>
    </div>
    <p class="note">
      只显示前缀，完整凭证永不回显（FR-094）。分组与倍率来自当前渠道的采集结果；「上游限流」是上游施加的 Key 级上限，P1 只登记展示、不判闸
    </p>
  </template>
</template>
