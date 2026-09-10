<script setup lang="ts">
import { computed } from 'vue'
import { useToastStore } from '@/stores/toast'

const toast = useToastStore()

const borderColor = computed(() =>
  toast.kind === 'bad'
    ? 'var(--destructive)'
    : toast.kind === 'ok'
      ? 'var(--success)'
      : 'var(--border)',
)
</script>

<template>
  <!-- v-show 而非 v-if：验收脚本用 $eval('#toast') 读文案，元素必须常驻 DOM。
       对应地 app.css 里的 #toast **没有** display:none —— v-show 只改行内
       display，压不住同元素上的 CSS 规则。 -->
  <div id="toast" v-show="toast.visible" :style="{ borderColor }">{{ toast.message }}</div>
</template>
