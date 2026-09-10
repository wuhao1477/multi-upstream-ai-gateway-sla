<script setup lang="ts">
/**
 * Key 剩余配额单元格。
 *
 * 叫「Key 剩余配额」而不是「剩余额度」/「余额」：它不是钱，是这把 Key 在
 * 上游被允许再花多少。把多把 Key 的配额相加当成账号余额是本仓库真犯过的
 * 错（渠道详情的「额度合计」就是 SUM(remain_quota_usd)）。
 *
 * 四种形态各自渲染，判定顺序在 quotaView 里，**不限额度排在 0 之前** ——
 * 无限额的 NewAPI Key 上游回的就是 remain_quota=0。
 */
import { computed } from 'vue'
import { quotaView, usdFine } from '@/utils/money'
import type { Key } from '@/api/types'

const props = defineProps<{ item: Key; bar?: boolean }>()

const view = computed(() => quotaView(props.item))
const used = computed(() => props.item.used_quota_usd)
</script>

<template>
  <div class="cell-money" :data-quota="item.id">
    <span class="num" :class="view.tone" :title="view.title">{{ view.text }}</span>
    <span v-if="view.kind === 'unlimited'" class="badge" title="不限额度 ≠ 无限可用：仍受账号余额约束"
      >不限额</span
    >
    <span v-else-if="view.kind === 'exhausted'" class="badge bad">耗尽</span>
    <span v-else-if="view.kind === 'low'" class="badge warn">将耗尽</span>
    <!-- 已用额度只在采到时才显示。没采到时不写 "已用 $0" ——
         那是"没数据"，不是"一分没花" -->
    <span v-if="used !== undefined && view.kind !== 'unlimited'" class="dim cell-sub"
      >已用 {{ usdFine(used) }}</span
    >
    <!-- 进度条只在能算出总额（剩余+已用）时出现；算不出就不画，
         画一根凭空的条比不画更容易被当真 -->
    <span v-if="bar === true && view.ratio !== null" class="qbar" :title="view.title">
      <i :style="{ width: `${Math.min(100, Math.round(view.ratio * 100))}%` }" :class="view.tone" />
    </span>
  </div>
</template>
