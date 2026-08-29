<script setup lang="ts">
/**
 * 统计卡。结构固定为 <b>值</b><span>标签</span> ——
 * 验收脚本按 `.stat b` / `.stat span` 取值与标签，顺序与标签不能改。
 */
defineProps<{ label: string; value: string | number; small?: boolean }>()
</script>

<template>
  <div class="stat">
    <b :style="small === true ? { fontSize: '13px' } : undefined">{{ value }}</b>
    <span>{{ label }}</span>
  </div>
</template>
