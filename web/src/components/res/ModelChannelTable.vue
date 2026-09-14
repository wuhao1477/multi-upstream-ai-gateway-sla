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
import { fmtAgo, fmtTime, statusLabel } from '@/utils/format'
import type { ModelChannel } from '@/api/types'

defineProps<{ modelName: string; channels: ModelChannel[] }>()
</script>

<template>
  <div class="tw">
    <table :data-model-channels="modelName">
      <thead>
        <tr>
          <th>渠道</th>
          <th>输入价</th>
          <th>输出价</th>
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
