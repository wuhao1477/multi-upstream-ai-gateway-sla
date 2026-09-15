<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import NavIcon from './NavIcon.vue'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import type { PaneName } from '@/router/panes'
import { PANE_ORDER, PANES } from '@/router/panes'

const route = useRoute()
const router = useRouter()
const channels = useChannelsStore()
const res = useResourcesStore()

/** 角标：各分栏的规模。空值渲染成空串而不是 0 —— "还没拉"和"确实是 0"不一样。 */
function badge(p: PaneName): string {
  if (p === 'channels') return channels.count > 0 ? String(channels.count) : ''
  if (p === 'accounts') return res.accounts.length > 0 ? String(res.accounts.length) : ''
  if (p === 'keys') return res.keys.length > 0 ? String(res.keys.length) : ''
  return ''
}

/**
 * 把分栏按 section 切段，分区标题只在段首出现一次。
 * 不是为了好看：「上游资源」三项与「运维」一项是两类任务，
 * 连成一排四个会让人以为它们是同一个流程的四步。
 */
const sections = computed(() => {
  const out: { title: string; panes: PaneName[] }[] = []
  for (const p of PANE_ORDER) {
    const s = PANES[p].section
    const last = out[out.length - 1]
    if (last !== undefined && last.title === s) last.panes.push(p)
    else out.push({ title: s, panes: [p] })
  }
  return out
})

function go(p: PaneName): void {
  void router.push({ name: p })
}

function isActive(p: PaneName): boolean {
  return route.name === p || route.meta.parent === p
}
</script>

<template>
  <!-- 控制台那一侧的分栏导航。品牌与主题开关在顶栏（AppHeader）——
       模型目录那一侧没有侧栏，放这儿等于换个页面就看不见版本号和主题开关了。 -->
  <aside class="sidebar">
    <template v-for="s in sections" :key="s.title">
      <div class="nav-label">{{ s.title }}</div>
      <button
        v-for="p in s.panes"
        :key="p"
        class="nav-item"
        :class="{ active: isActive(p) }"
        :data-pane="p"
        @click="go(p)"
      >
        <span class="nav-ic"><NavIcon :name="p" /></span>
        <span>{{ PANES[p].nav }}</span>
        <span class="nav-count">{{ badge(p) }}</span>
      </button>
    </template>
  </aside>
</template>
