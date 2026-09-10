<script setup lang="ts">
/** 卡片。title 之外的头部内容（角标、筛选框、按钮）走 header 插槽。 */
defineProps<{ title?: string; desc?: string }>()
</script>

<template>
  <div class="card">
    <div class="card-h" v-if="title !== undefined || $slots.header">
      <h2 class="card-t" v-if="title !== undefined">{{ title }}</h2>
      <slot name="header" />
    </div>
    <p class="card-d" v-if="desc !== undefined">{{ desc }}</p>
    <slot />
  </div>
</template>
