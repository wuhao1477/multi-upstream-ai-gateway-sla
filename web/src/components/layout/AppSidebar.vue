<script setup lang="ts">
import { useRoute, useRouter } from 'vue-router'
import NavIcon from './NavIcon.vue'
import ThemeSwitch from './ThemeSwitch.vue'
import { useChannelsStore } from '@/stores/channels'
import { useCredentialsStore } from '@/stores/credentials'
import type { PaneName } from '@/router/panes'
import { PANE_ORDER, PANES } from '@/router/panes'

const route = useRoute()
const router = useRouter()
const channels = useChannelsStore()
const creds = useCredentialsStore()

/** 角标：渠道数 / 当前渠道 / 凭证数。空值渲染成空串而不是 0。 */
function badge(p: PaneName): string {
  if (p === 'channels') return channels.count > 0 ? String(channels.count) : ''
  if (p === 'detail') return channels.currentID === null ? '' : `#${channels.currentID}`
  if (p === 'creds') return creds.count > 0 ? String(creds.count) : ''
  return ''
}

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

    <div class="nav-label">采集与台账</div>
    <button
      v-for="p in PANE_ORDER"
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

    <div class="side-foot">
      <div class="side-foot-l">外观</div>
      <ThemeSwitch />
    </div>
  </aside>
</template>
