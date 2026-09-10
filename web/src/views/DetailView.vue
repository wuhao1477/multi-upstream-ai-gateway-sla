<script setup lang="ts">
import { ref, watch } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiStat from '@/components/ui/UiStat.vue'
import GroupsView from '@/views/detail/GroupsView.vue'
import CatalogView from '@/views/detail/CatalogView.vue'
import KeysView from '@/views/detail/KeysView.vue'
import { useChannelsStore } from '@/stores/channels'
import type { ItemStatus } from '@/api/types'
import { fmtTime } from '@/utils/format'

const channels = useChannelsStore()

type SubView = 'groups' | 'catalog' | 'keys'
const view = ref<SubView | null>(null)
/**
 * 重挂载计数。同一个分栏被再点一次时要重新拉数据（旧版就是重新 click 一遍），
 * 而子组件是在 onMounted 里拉的 —— 换 key 强制重挂载，
 * 比让每个子组件都暴露一个 reload 方法要少一层约定。
 */
const nonce = ref(0)

function show(v: SubView): void {
  if (view.value === v) nonce.value += 1
  view.value = v
}

// 换渠道就收起子视图：上一个渠道的分组/目录留在屏幕上会被当成新渠道的数据
watch(
  () => channels.currentID,
  () => {
    view.value = null
  },
)

/** 采集状态 → 徽标类。unsupported/skipped 不着色：它们不是故障。 */
function statusClass(s: ItemStatus): string {
  if (s === 'ok') return 'ok'
  if (s === 'unsupported' || s === 'skipped') return ''
  if (s === 'partial') return 'warn'
  return 'bad'
}
</script>

<template>
  <div class="pane on" id="pane-detail">
    <UiCard v-if="channels.currentID === null" id="detail-empty">
      <UiEmpty>先在「渠道商」里选一个渠道</UiEmpty>
    </UiCard>

    <template v-else>
      <UiCard id="detail-card">
        <template #header>
          <h2 class="card-t">资产总览</h2>
          <span class="badge acc" id="detail-name"
            >#{{ channels.currentID }} {{ channels.currentName }}</span
          >
          <div class="spacer"></div>
          <button class="btn" id="btn-sync" :disabled="channels.syncing" @click="channels.sync()">
            {{ channels.syncing ? '采集中…' : '立即采集' }}
          </button>
        </template>

        <div class="stats" id="inv-stats">
          <template v-if="channels.inventory !== null">
            <UiStat label="账号" :value="channels.inventory.accounts" />
            <UiStat label="Key" :value="channels.inventory.keys" />
            <UiStat label="分组" :value="channels.inventory.groups" />
            <UiStat label="目录模型" :value="channels.inventory.catalog_models" />
            <UiStat
              label="额度合计"
              :value="`$${channels.inventory.quota_total_usd.toFixed(2)}`"
            />
            <UiStat
              label="最近同步"
              :value="fmtTime(channels.inventory.last_synced_at)"
              small
            />
          </template>
        </div>

        <div id="inv-anomalies">
          <template v-if="channels.inventory !== null">
            <p
              v-if="channels.inventory.anomalies.length === 0"
              class="ok"
              style="font-size: 13px; margin: 14px 0 0"
            >
              ✓ 无异常项
            </p>
            <div v-else style="margin-top: 14px">
              <div class="anom" v-for="a in channels.inventory.anomalies" :key="a.kind">
                <span class="badge warn">{{
                  a.kind === 'delisted_model' ? '疑似下架模型' : a.kind
                }}</span>
                <span class="dim" style="font-size: 12px"> × {{ a.count }}</span>
                <div class="dim" style="font-size: 12px">{{ a.hint }}</div>
                <details v-if="a.items !== undefined && a.items.length > 0">
                  <summary>明细</summary>
                  <div class="dim" style="font-size: 12px">{{ a.items.join('、') }}</div>
                </details>
              </div>
            </div>
          </template>
        </div>

        <div id="sync-result">
          <p v-if="channels.syncing" class="dim" style="margin-top: 14px">
            正在依次采集账号/分组/Key/价格/目录…
          </p>
          <p
            v-else-if="channels.syncError !== ''"
            class="bad"
            style="font-size: 13px; margin-top: 14px"
          >
            采集失败：{{ channels.syncError }}
          </p>
          <template v-else-if="channels.syncResult !== null">
            <div class="sec-t">采集结果</div>
            <div class="tw">
              <table>
                <thead>
                  <tr>
                    <th>能力</th>
                    <th>支持级别</th>
                    <th>状态</th>
                    <th>行数</th>
                    <th>耗时</th>
                    <th>说明</th>
                  </tr>
                </thead>
                <tbody>
                  <!-- key 用下标：限流/前置失败的 items 没有 capability，
                       拿它当 key 会撞成同一个（Vue 会在控制台报重复 key）。 -->
                  <tr v-for="(i, idx) in channels.syncResult.items" :key="idx">
                    <td>
                      <code>{{ i.capability ?? '-' }}</code>
                    </td>
                    <td><code>{{ i.support ?? '-' }}</code></td>
                    <td>
                      <span class="badge" :class="statusClass(i.status)">{{ i.status }}</span>
                    </td>
                    <td>{{ i.rows ?? '' }}</td>
                    <td class="dim">{{ i.elapsed_ms === undefined ? '' : `${i.elapsed_ms} ms` }}</td>
                    <td class="dim" style="font-size: 12px">{{ i.note ?? i.error ?? '' }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <p class="note" v-if="channels.syncResult.elapsed_ms > 0">
              总耗时 {{ channels.syncResult.elapsed_ms }} ms
            </p>
          </template>
        </div>
      </UiCard>

      <UiCard id="detail-tabs-card">
        <template #header>
          <button
            class="btn sm"
            :class="view === 'groups' ? '' : 'outline'"
            id="btn-groups"
            @click="show('groups')"
          >
            分组
          </button>
          <button
            class="btn sm"
            :class="view === 'catalog' ? '' : 'outline'"
            id="btn-catalog"
            @click="show('catalog')"
          >
            模型目录
          </button>
          <button
            class="btn sm"
            :class="view === 'keys' ? '' : 'outline'"
            id="btn-keys"
            @click="show('keys')"
          >
            Key
          </button>
        </template>

        <!-- 一次只挂一个子视图：三张表并存在 DOM 里的话，
             "等表格出现"就可能读到上一个视图的数据（旧版靠等专有表头绕开）。 -->
        <div id="detail-body">
          <UiEmpty v-if="view === null">选一项查看</UiEmpty>
          <GroupsView v-else-if="view === 'groups'" :key="`g${nonce}`" />
          <CatalogView v-else-if="view === 'catalog'" :key="`c${nonce}`" />
          <KeysView v-else :key="`k${nonce}`" />
        </div>
      </UiCard>
    </template>
  </div>
</template>
