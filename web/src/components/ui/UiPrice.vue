<script setup lang="ts">
import { computed } from 'vue'
import { unitLabel } from '@/utils/format'

/**
 * 价格 + 计价口径，**同格显示**。
 *
 * 真实站点约 15% 的模型按次计价，其绝对美元价与倍率的数值区间重叠 ——
 * 光看数字分不出 "$3.5/次" 和 "倍率 3.5"，据此选型会把按次的视频模型
 * 当成"比 opus 便宜"。所以口径不是装饰，缺了就是误导。
 */
const props = defineProps<{ value?: number | undefined; unit?: string | null | undefined }>()

const label = computed(() => unitLabel(props.unit))
</script>

<template>
  <span v-if="value === undefined || value === null" class="dim">—</span>
  <template v-else>
    {{ value }}
    <span v-if="label !== null" class="dim">{{ label }}</span>
    <!-- 上游未声明口径：显式标出来，**不假定默认值**（02 §1.3bis） -->
    <span v-else class="warn" title="上游未声明口径，勿假定默认值">口径未知</span>
  </template>
</template>
