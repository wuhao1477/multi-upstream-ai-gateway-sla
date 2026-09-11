<script setup lang="ts">
/**
 * 账号余额单元格：**金额 + 状态 + 确认时刻，三件事永远一起出现**。
 *
 * 拆开过一次的后果是可以想见的：单看 "$0.42" 判断不了要不要充值 ——
 * 它可能是五分钟前确认的（真的快没了），也可能是三天前的（这期间花掉多少
 * 无人知道）。FR-020 要求记录"数据更新时间"，FR-026 要求按最近可信余额扣
 * 已知消耗形成下限，两条都指向同一件事：金额离开时刻就没有意义。
 */
import { computed } from 'vue'
import { balanceStateLabel, balanceView } from '@/utils/money'
import { fmtAgo, fmtTime } from '@/utils/format'
import type { Account } from '@/api/types'

const props = defineProps<{ account: Account }>()

const view = computed(() => balanceView(props.account))
const stateLabel = computed(() => balanceStateLabel(props.account.balance_state))
</script>

<template>
  <div class="cell-money" :data-balance="account.id">
    <span class="num" :class="view.tone" :title="view.title">{{ view.text }}</span>
    <!-- 状态用文字而不只是颜色：色盲用户看不出红黄绿的区别，
         而"余额耗尽"和"余额临界"的处置动作是不同的 -->
    <span v-if="stateLabel !== ''" class="badge" :class="view.tone">{{ stateLabel }}</span>
    <span
      v-if="view.kind !== 'never'"
      class="dim cell-sub"
      :title="fmtTime(account.balance_confirmed_at)"
      >{{ fmtAgo(account.balance_confirmed_at) }}确认</span
    >
  </div>
</template>
