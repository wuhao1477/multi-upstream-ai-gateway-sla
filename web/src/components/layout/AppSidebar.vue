<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import NavIcon from './NavIcon.vue'
import ThemeSwitch from './ThemeSwitch.vue'
import { useChannelsStore } from '@/stores/channels'
import { useCredentialsStore } from '@/stores/credentials'
import { useResourcesStore } from '@/stores/resources'
import type { PaneName } from '@/router/panes'
import { PANE_ORDER, PANES } from '@/router/panes'

const route = useRoute()
const router = useRouter()
const channels = useChannelsStore()
const creds = useCredentialsStore()
const res = useResourcesStore()

/** 角标：各分栏的规模。空值渲染成空串而不是 0 —— "还没拉"和"确实是 0"不一样。 */
function badge(p: PaneName): string {
  if (p === 'channels') return channels.count > 0 ? String(channels.count) : ''
  if (p === 'detail') return channels.currentID === null ? '' : `#${channels.currentID}`
  if (p === 'accounts') return res.accounts.length > 0 ? String(res.accounts.length) : ''
  if (p === 'keys') return res.keys.length > 0 ? String(res.keys.length) : ''
  if (p === 'creds') return creds.count > 0 ? String(creds.count) : ''
  return ''
}

/**
 * 把分栏按 section 切段，分区标题只在段首出现一次。
 * 不是为了好看：「上游资源」四项与「运维」两项是两类任务，
 * 连成一排六个会让人以为它们是同一个流程的六步。
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
</script>

<template>
  <!-- 导航形态取自参考仓库的 settings-nav（图标 chip + 右侧激活竖条 + 底部虚线块），
       而不是 dashboard 的横向 pill nav：用户明确要求左右布局。 -->
  <aside class="sidebar">
    <div class="brand">
      <div class="brand-mark">SLA</div>
      <div>
        <div class="brand-t">上游采集与管理</div>
        <div class="brand-s">交付阶段 P1</div>
      </div>
    </div>

    <template v-for="s in sections" :key="s.title">
      <div class="nav-label">{{ s.title }}</div>
      <button
        v-for="p in s.panes"
        :key="p"
        class="nav-item"
        :class="{ active: route.name === p }"
        :data-pane="p"
        @click="go(p)"
      >
        <span class="nav-ic"><NavIcon :name="p" /></span>
        <span>{{ PANES[p].nav }}</span>
        <span class="nav-count">{{ badge(p) }}</span>
      </button>
    </template>

    <div class="side-foot">
      <div class="side-foot-l">外观</div>
      <ThemeSwitch />
    </div>
  </aside>
</template>
