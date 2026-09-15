<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import type { PaneName } from '@/router/panes'
import { PANES } from '@/router/panes'
const route = useRoute()

const meta = computed(() => {
  const name = route.name
  const base = typeof name === 'string' && name in PANES ? PANES[name as PaneName] : undefined
  const title = route.meta.title ?? base?.title
  const note = route.meta.note ?? base?.note
  return {
    title: typeof title === 'string' ? title : '',
    note: typeof note === 'string' ? note : '',
  }
})
</script>

<template>
  <!-- 控制台那一侧的分栏抬头。令牌搬去了顶栏（全站凭证，见 AppHeader）——
       模型目录那一页压根不渲染这个组件，它自己有一套抬头。 -->
  <header class="topbar">
    <h1 id="pane-title">{{ meta.title }}</h1>
    <span class="badge" id="pane-note">{{ meta.note }}</span>
  </header>
</template>
