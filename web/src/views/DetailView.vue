<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiStat from '@/components/ui/UiStat.vue'
import UiSegmented from '@/components/ui/UiSegmented.vue'
import GroupsView from '@/views/detail/GroupsView.vue'
import CatalogView from '@/views/detail/CatalogView.vue'
import AccountTable from '@/components/res/AccountTable.vue'
import KeyTable from '@/components/res/KeyTable.vue'
import RegisterDrawers from '@/components/res/RegisterDrawers.vue'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import type { ItemStatus } from '@/api/types'
import { fmtAgo } from '@/utils/format'
import { totalBalance, totalQuota, usd, usdFine } from '@/utils/money'

const channels = useChannelsStore()
const res = useResourcesStore()

type SubView = 'accounts' | 'keys' | 'groups' | 'catalog'
const view = ref<SubView>('accounts')
const drawer = ref<'account' | 'key' | null>(null)
const drawerAccount = ref(0)
/**
 * 重挂载计数。同一个 Tab 被再点一次时要重新拉数据，而分组/目录子组件是在
 * onMounted 里拉的 —— 换 key 强制重挂载，比让每个子组件都暴露一个 reload
 * 方法要少一层约定。账号/Key 两个 Tab 走共享 store，不需要它。
 */
const nonce = ref(0)

function show(v: SubView): void {
  if (view.value === v) nonce.value += 1
  view.value = v
}

// 换渠道回到第一个 Tab：上一个渠道的目录留在屏幕上会被当成新渠道的数据
watch(
  () => channels.currentID,
  (id) => {
    view.value = 'accounts'
    if (id !== null) void res.load()
  },
)

const chID = computed(() => channels.currentID ?? 0)
const accounts = computed(() => res.channelAccounts(chID.value))
const keys = computed(() => res.channelKeys(chID.value))
/** 该渠道下账号余额的**去重合计**（FR-022）。它不是"渠道余额" —— 渠道没有钱包。 */
const balance = computed(() => totalBalance(accounts.value))
const quota = computed(() => totalQuota(keys.value))

/** 采集状态 → 徽标类。unsupported/skipped 不着色：它们不是故障。 */
function statusClass(s: ItemStatus): string {
  if (s === 'ok') return 'ok'
  if (s === 'unsupported' || s === 'skipped') return ''
  if (s === 'partial') return 'warn'
  return 'bad'
}

const ANOMALY_LABEL: Record<string, string> = {
  delisted_model: '疑似下架模型',
  stale_data: '数据陈旧',
  degraded_missing_fields: '价格待人工录入',
  unregistered_key: '上游有、库里没登记的 Key',
  credential_invalid: '采集凭证失效',
  credential_missing: '未登记采集凭证',
  site_family_unknown: '站型未识别',
  never_collected: '从未采集成功',
  key_unusable: 'Key 已停用或过期',
}

/**
 * 异常项按"要不要马上处理"分成两档。
 *
 * 六类异常原本一律用同一个黄徽标，于是「站型未识别」（这渠道**根本采不了**）
 * 和「疑似下架模型」（信息性提示）看起来一样重。前者不修，这个渠道在界面上
 * 永远是一串空数据。
 */
const BLOCKING = new Set([
  'credential_missing',
  'credential_invalid',
  'site_family_unknown',
  'never_collected',
])

function anomalyLabel(kind: string): string {
  return ANOMALY_LABEL[kind] ?? kind
}

function openAddKey(accountID: number): void {
  drawerAccount.value = accountID
  drawer.value = 'key'
}
</script>

<template>
  <div class="pane on" id="pane-detail">
    <UiCard v-if="channels.currentID === null" id="detail-empty">
      <UiEmpty>先在「渠道管理」里选一个渠道</UiEmpty>
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
            <!-- 这一格原来叫「额度合计」，而它取的是 SUM(key.remain_quota_usd)。
                 那是 Key 的使用约束，不是这个渠道的钱 —— 摆在账号旁边会被读成
                 渠道余额，正是 FR-022 禁止的那种混淆。名字必须写全。 -->
            <UiStat
              label="Key 剩余配额合计"
              :value="`$${channels.inventory.quota_total_usd.toFixed(2)}`"
            />
            <UiStat label="关联账号余额合计" :value="usd(balance.total)" />
            <UiStat label="分组" :value="channels.inventory.groups" />
            <UiStat label="目录模型" :value="channels.inventory.catalog_models" />
            <UiStat
              label="最近同步"
              :value="fmtAgo(channels.inventory.last_synced_at)"
              small
            />
          </template>
        </div>
        <p class="note" v-if="channels.inventory !== null">
          「Key 剩余配额合计」是这个渠道下各把 Key 还能花多少（使用约束）；
          「关联账号余额合计」是它下面各账号钱包里的钱，已按余额组去重（FR-022），
          <template v-if="balance.unknown > 0"
            >其中 {{ balance.unknown }} 个账号的余额从未采到，<b>不计入合计</b>；</template
          >
          两者<b>不可相加、也不互相蕴含</b>。渠道本身没有钱包，所以没有「渠道余额」这个数。
        </p>

        <div id="inv-anomalies">
          <template v-if="channels.inventory !== null">
            <p
              v-if="channels.inventory.anomalies.length === 0"
              class="ok"
              style="font-size: 13px; margin: 14px 0 0"
            >
              ✓ 无异常项
            </p>
            <div v-else class="anoms">
              <div
                class="anom"
                v-for="a in channels.inventory.anomalies"
                :key="a.kind"
                :data-anom="a.kind"
                :class="{ blocking: BLOCKING.has(a.kind) }"
              >
                <!-- 阻断性的用红、信息性的用黄。都用黄的话，"这渠道采不了"
                     和"有个模型可能下架了"在界面上一样重 -->
                <span class="badge" :class="BLOCKING.has(a.kind) ? 'bad' : 'warn'">{{
                  anomalyLabel(a.kind)
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
                    <th class="n">行数</th>
                    <th class="n">耗时</th>
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
                    <td class="n">{{ i.rows ?? '' }}</td>
                    <td class="n dim">{{ i.elapsed_ms === undefined ? '' : `${i.elapsed_ms} ms` }}</td>
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
          <UiSegmented
            :model-value="view"
            label="渠道详情视图"
            :options="[
              { value: 'accounts', label: '账号', count: accounts.length },
              { value: 'keys', label: 'Key', count: keys.length },
              { value: 'groups', label: '分组' },
              { value: 'catalog', label: '模型目录' },
            ]"
            @update:model-value="show($event as SubView)"
          />
          <div class="spacer"></div>
          <button
            v-if="view === 'accounts'"
            class="btn sm"
            id="btn-detail-new-account"
            @click="drawer = 'account'"
          >
            + 登记账号
          </button>
          <button v-if="view === 'keys'" class="btn sm" id="btn-detail-new-key" @click="drawer = 'key'">
            + 登记 Key
          </button>
        </template>

        <!-- 一次只挂一个子视图：几张表并存在 DOM 里的话，
             "等表格出现"就可能读到上一个视图的数据（旧版靠等专有表头绕开）。 -->
        <div id="detail-body">
          <template v-if="view === 'accounts'">
            <AccountTable
              :items="accounts"
              empty-text="该渠道还没有账号。账号是余额的归属方，也是 Key 的挂载点 —— 先建一个。"
              @add-key="openAddKey"
            />
            <p class="note" v-if="accounts.length > 0">
              账号余额来自采集（balance_signals），<b>「未采集」不是 0</b>：可能是站型不提供
              余额接口、凭证缺失或从未跑过采集。共享同一个余额组的账号在上游是同一个钱包，
              合计时只计一次（FR-022）。
            </p>
          </template>
          <template v-else-if="view === 'keys'">
            <KeyTable
              :items="keys"
              :cols="['ref', 'rate']"
              :show-account="true"
              empty-text="该渠道还没有 Key，先在「账号」页签给某个账号登记一把"
              @changed="res.reload()"
            />
            <p class="note" v-if="keys.length > 0">
              只显示前缀，完整凭证永不回显（FR-094）。分组与倍率来自当前渠道的采集结果；
              「上游限流」是上游施加的 Key 级上限，P1 只登记展示、不判闸（FR-127）。
              该渠道 Key 剩余配额合计 {{ usdFine(quota.remain) }}，最小
              {{ quota.min === null ? '—' : usdFine(quota.min) }}
              <template v-if="quota.unlimited > 0">，另有 {{ quota.unlimited }} 把不限额度</template>
              <template v-if="quota.unknown > 0">，{{ quota.unknown }} 把未采集</template>。
            </p>
          </template>
          <GroupsView v-else-if="view === 'groups'" :key="`g${nonce}`" />
          <CatalogView v-else :key="`c${nonce}`" />
        </div>
      </UiCard>

      <RegisterDrawers
        :mode="drawer"
        :channel-id="chID"
        :account-id="drawerAccount"
        @close="drawer = null"
        @created="drawerAccount = 0"
      />
    </template>
  </div>
</template>
