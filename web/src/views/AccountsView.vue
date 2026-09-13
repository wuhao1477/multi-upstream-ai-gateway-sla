<script setup lang="ts">
/**
 * 上游账号管理 —— **资金归属的主界面**。
 *
 * 余额挂在账号上（balance_signals ← 采集器），不在渠道上，也不在 Key 上。
 * 所以"还有多少钱"这个问题只在这一页有完整答案；渠道页给的是它下面这些
 * 账号的**去重合计**，Key 页给的是配额（使用约束，不是钱）。
 *
 * 页面上四个数字刻意分开摆：已知余额合计 / 钱包数 / 未采到余额的账号数 /
 * 状态异常数。合成一个"总余额"会把"未采到"当成 0 —— 那是个看起来精确的
 * 错数，比不给更糟（FR-020）。
 */
import { computed, onMounted, ref } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import UiStat from '@/components/ui/UiStat.vue'
import AccountTable from '@/components/res/AccountTable.vue'
import RegisterDrawers from '@/components/res/RegisterDrawers.vue'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { emptyAccountFilter, filterAccounts, isAccountFilterActive } from '@/utils/keyfilter'
import { totalBalance, usd } from '@/utils/money'

const channels = useChannelsStore()
const res = useResourcesStore()

const filter = ref(emptyAccountFilter())
const drawer = ref<'account' | 'key' | 'cred' | null>(null)
const drawerAccount = ref(0)

onMounted(() => {
  void res.load()
})

/**
 * 抽屉的上下文。属性名是复数的 `channel-ids` / `account-ids`（理由见
 * RegisterDrawers 顶部：写成单数会静默落进 attrs，表现是从账号行点「+ Key」
 * 打开的抽屉不预填账号、无报错，TS 也不管）。用 computed 而不是模板里内联
 * `[drawerAccount]`：内联字面量每次重渲染都是新数组，而那边的 watch 是
 * deep 的 —— 在筛选框里敲一个字就会把填了一半的 Key 明文清掉。
 */
const drawerChannelIDs = computed(() => [filter.value.channelID])
const drawerAccountIDs = computed(() => [drawerAccount.value])

const shown = computed(() => filterAccounts(res.accounts, filter.value))
const active = computed(() => isAccountFilterActive(filter.value))

/** 概览统计**跟着筛选走**：筛出"未采到余额"的那批之后，合计要是那批的。 */
const totals = computed(() => totalBalance(shown.value))
const riskCount = computed(
  () =>
    shown.value.filter((a) => {
      const s = a.balance_state ?? ''
      return s !== '' && s !== 'normal'
    }).length,
)
/**
 * 缺凭证的账号数。它和「余额未采集」不是一回事，所以单独摆一格：
 * 后者是"这次没采到"，前者是"**根本采不了**，而且不会自愈"。
 */
const noCredCount = computed(() => shown.value.filter((a) => a.cred_type === undefined).length)

function reset(): void {
  filter.value = emptyAccountFilter()
}

function openAddKey(accountID: number): void {
  drawerAccount.value = accountID
  drawer.value = 'key'
}

function openCred(accountID: number): void {
  drawerAccount.value = accountID
  drawer.value = 'cred'
}
</script>

<template>
  <div class="pane on" id="pane-accounts">
    <UiCard>
      <template #header>
        <div>
          <h2 class="card-t">上游账号</h2>
          <p class="card-d">
            别人家站点上的账号，用于采集与记账。余额归属在这里，不是平台登录用户。
          </p>
        </div>
        <div class="spacer"></div>
        <button class="btn outline sm" id="btn-account-reload" :disabled="res.loading" @click="res.reload()">
          {{ res.loading ? '刷新中…' : '刷新' }}
        </button>
        <button class="btn" id="btn-new-account" @click="drawer = 'account'">+ 登记账号</button>
      </template>

      <div class="stats">
        <UiStat label="账号数" :value="shown.length" />
        <UiStat label="已知余额合计" :value="usd(totals.total)" />
        <UiStat label="独立钱包" :value="totals.wallets" />
        <UiStat label="余额未采集" :value="totals.unknown" />
        <UiStat label="余额状态异常" :value="riskCount" />
        <UiStat label="缺采集凭证" :value="noCredCount" />
      </div>
      <p class="note">
        合计已按「余额组」去重（FR-022）：共用同一个上游钱包的账号只计一次，所以
        合计通常小于逐行相加。<b>未采集的 {{ totals.unknown }} 个账号不计入合计</b> ——
        把它们当 0 会得出一个偏低且看起来精确的总额。一期一律美元、汇率 1:1（FR-018）。
      </p>
    </UiCard>

    <UiCard>
      <template #header>
        <h2 class="card-t">账号列表</h2>
        <span class="badge" id="account-count">{{
          res.loaded ? `${shown.length} / ${res.accounts.length}` : '—'
        }}</span>
        <div class="spacer"></div>
      </template>

      <div class="toolbar">
        <div class="tb-grow">
          <label class="sr" for="acc-q">搜索账号</label>
          <input id="acc-q" v-model="filter.q" placeholder="搜索账号 ID / 上游用户 ID / 余额组…" />
        </div>
        <UiField label="渠道" for="acc-f-channel">
          <select id="acc-f-channel" v-model.number="filter.channelID">
            <option :value="0">全部渠道</option>
            <option v-for="c in channels.list" :key="c.id" :value="c.id">{{ c.name }}</option>
          </select>
        </UiField>
        <UiField label="状态" for="acc-f-status">
          <select id="acc-f-status" v-model="filter.status">
            <option value="">全部状态</option>
            <option value="active">启用</option>
            <option value="disabled">停用</option>
          </select>
        </UiField>
        <UiField label="余额" for="acc-f-balance">
          <select id="acc-f-balance" v-model="filter.balance">
            <option value="all">全部余额情况</option>
            <option value="known">已采到余额</option>
            <option value="unknown">未采集</option>
            <option value="risk">状态异常</option>
          </select>
        </UiField>
        <UiField label="采集凭证" for="acc-f-cred">
          <select id="acc-f-cred" v-model="filter.cred">
            <option value="all">全部凭证情况</option>
            <option value="missing">未登记（采不了）</option>
            <option value="has">已登记</option>
          </select>
        </UiField>
        <button class="btn outline sm" id="btn-acc-reset" :disabled="!active" @click="reset">
          重置
        </button>
      </div>

      <UiEmpty v-if="!res.loaded && res.error === ''">加载中…</UiEmpty>
      <UiEmpty v-else-if="res.error !== ''">加载失败：{{ res.error }}</UiEmpty>
      <UiEmpty v-else-if="res.accounts.length === 0">
        还没有任何上游账号。点右上角「登记账号」创建第一个。
      </UiEmpty>
      <UiEmpty v-else-if="shown.length === 0">没有符合当前筛选条件的账号</UiEmpty>
      <AccountTable
        v-else
        :items="shown"
        show-channel
        @add-key="openAddKey"
        @edit-cred="openCred"
      />
    </UiCard>

    <RegisterDrawers
      :mode="drawer"
      :channel-ids="drawerChannelIDs"
      :account-ids="drawerAccountIDs"
      @close="drawer = null"
      @created="drawerAccount = 0"
    />
  </div>
</template>
