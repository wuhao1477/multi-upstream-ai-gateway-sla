<script setup lang="ts">
import { onMounted, ref } from 'vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import * as adminApi from '@/api/admin'
import type { ChannelGroup } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useToastStore } from '@/stores/toast'
import { fmtTime } from '@/utils/format'

const channels = useChannelsStore()
const toast = useToastStore()

const groups = ref<ChannelGroup[]>([])
const loaded = ref(false)
let modelRequest = 0
/** 展开的分组：名称用**上游分组名**，见下方注释。 */
const expanded = ref<{ name: string; count: number; models: string[] } | null>(null)

onMounted(async () => {
  const id = channels.currentID
  if (id === null) return
  try {
    const d = await adminApi.listGroups(id)
    groups.value = d.items
  } catch (e) {
    toast.fail('加载分组失败', e)
  } finally {
    loaded.value = true
  }
})

async function openModels(g: ChannelGroup): Promise<void> {
  const request = ++modelRequest
  try {
    const m = await adminApi.groupModels(g.id)
    if (modelRequest !== request || channels.currentID === null) return
    // 标题用**上游分组名**而非内部 id：id 逐次采集会变（实测两次运行里
    // 同一个 id 指向了不同分组），运维看"分组 2"无从对应到上游的 vip/default。
    expanded.value = { name: g.group_ref, count: m.count, models: m.models }
  } catch (e) {
    toast.fail('加载分组模型失败', e)
  }
}
</script>

<template>
  <UiEmpty v-if="loaded && groups.length === 0">还没有分组，先点「立即采集」</UiEmpty>
  <template v-else-if="groups.length > 0">
    <div class="sec-t">分组 <span class="badge">{{ groups.length }} 个</span></div>
    <div class="tw">
      <table>
        <thead>
          <tr>
            <th>分组</th>
            <th>倍率</th>
            <th>可用模型</th>
            <th>采集时间</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="g in groups" :key="g.id">
            <td>
              <code>{{ g.group_ref }}</code>
            </td>
            <td>
              <template v-if="g.rate_multiplier !== undefined">{{ g.rate_multiplier }}</template>
              <span v-else class="dim">未知</span>
            </td>
            <td>
              <button class="btn outline sm" :data-g="g.id" :data-name="g.group_ref" @click="openModels(g)">
                {{ g.model_count }} 个模型
              </button>
            </td>
            <td class="dim">{{ fmtTime(g.fetched_at) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <div id="gm">
      <p class="note" v-if="expanded !== null">
        分组 <code>{{ expanded.name }}</code> 可用模型（{{ expanded.count }}）：{{
          expanded.models.length > 0 ? expanded.models.join('、') : '无'
        }}
      </p>
    </div>
  </template>
</template>
