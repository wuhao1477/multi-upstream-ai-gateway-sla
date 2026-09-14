<script setup lang="ts">
/**
 * 「哪些渠道有这个模型」那张表。**两处共用**：列表模式的行内展开，
 * 与卡片模式的抽屉。
 *
 * 共用而不是抄两遍：这张表上挂着三条口径（价格必须与计价口径同格、
 * 渠道停用 ≠ 目录下架、两者都要显式标出），抄一份就会有一份先烂掉，
 * 而烂掉的那份看起来完全正常 —— 它只在"那个渠道恰好停用了"时才错。
 *
 * data-model-channels 由调用方给：验收脚本按模型名取这张表。
 */
import UiPrice from '@/components/ui/UiPrice.vue'
import { effectivePrices, fmtAgo, fmtTime, statusLabel, unitLabel } from '@/utils/format'
import type { ModelChannel } from '@/api/types'

defineProps<{ modelName: string; channels: ModelChannel[] }>()

/**
 * 「实际价」那一格的悬停全文：逐个分组列出折算后的价格。
 *
 * 单元格里只放区间与最便宜的那个分组名 —— 真站点一个渠道有 6~14 个分组，
 * 全铺进表格会让一行变成十几行，而人先要的只是"最低能到多少、用哪个分组"。
 */
function groupsTitle(c: ModelChannel): string {
  const u = unitLabel(c.billing_unit) ?? ''
  return effectivePrices(c)
    .map((e) => `${e.groupRef}（分组倍率 ×${e.groupRate}）→ ${e.input}${u}`)
    .join('\n')
}
</script>

<template>
  <div class="tw">
    <table :data-model-channels="modelName">
      <thead>
        <tr>
          <th>渠道</th>
          <th title="模型自己的倍率/单价，**还没乘分组倍率**，不是你会被计的价">模型价</th>
          <th>输出价</th>
          <th
            title="模型价 × 分组倍率 = 这把 Key 实际会被计的价。分组由 Key 所在的分组决定，实测真站点的分组倍率跨度超过十倍。悬停看逐个分组。"
          >
            实际价（含分组倍率）
          </th>
          <th>渠道状态</th>
          <th>目录状态</th>
          <th>最近出现</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="c in channels" :key="c.channel_id">
          <td>
            <router-link
              class="linkish"
              :data-model-ch="c.channel_id"
              :to="{ name: 'channel-detail', params: { id: String(c.channel_id) } }"
              >{{ c.channel_name }}</router-link
            >
          </td>
          <td><UiPrice :value="c.input_price" :unit="c.billing_unit" /></td>
          <td><UiPrice :value="c.output_price" :unit="c.billing_unit" /></td>
          <!-- 一个分组倍率都没采到时显示「未采到分组倍率」而**不是**退回模型价：
               退回去等于悄悄把未知的分组倍率当成 1，而真站点上它可能是 0.12
               也可能是 3.5 —— 那个数字看起来完全正常，只是与账单无关 -->
          <td :data-model-eff="c.channel_id" :title="groupsTitle(c)">
            <template v-if="effectivePrices(c).length > 0">
              <UiPrice
                :value="effectivePrices(c)[0]!.input"
                :unit="c.billing_unit"
              />
              <span v-if="effectivePrices(c).length > 1" class="dim cell-sub"
                >最低 · 分组 {{ effectivePrices(c)[0]!.groupRef }}（共
                {{ effectivePrices(c).length }} 个分组，最高
                {{ effectivePrices(c)[effectivePrices(c).length - 1]!.input }}）</span
              >
              <span v-else class="dim cell-sub">分组 {{ effectivePrices(c)[0]!.groupRef }}</span>
            </template>
            <span v-else class="dim" title="该渠道还没采到分组倍率 —— 不可当成 ×1"
              >未采到分组倍率</span
            >
          </td>
          <!-- 渠道停用与目录下架是**两回事**，两列分开：停用是我方台账上的
               决定（不采集也不承接请求），下架是上游那边撤了这个模型。
               合成一个"可用"列就把两种处置动作混成了一个。 -->
          <td>
            <span class="badge" :class="c.channel_status === 'disabled' ? 'bad' : 'ok'">{{
              statusLabel(c.channel_status)
            }}</span>
          </td>
          <td>
            <span class="badge" :class="c.stale ? 'warn' : ''">{{
              c.stale ? '疑似下架' : '在架'
            }}</span>
          </td>
          <td class="dim" :title="fmtTime(c.last_seen_at)">{{ fmtAgo(c.last_seen_at) }}</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
