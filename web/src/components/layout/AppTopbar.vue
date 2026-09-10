<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import type { PaneName } from '@/router/panes'
import { PANES } from '@/router/panes'

const route = useRoute()
const auth = useAuthStore()

const meta = computed(() => {
  const n = route.name
  if (typeof n === 'string' && n in PANES) return PANES[n as PaneName]
  return { nav: '', title: '', note: '' }
})
</script>

<template>
  <header class="topbar">
    <h1 id="pane-title">{{ meta.title }}</h1>
    <span class="badge" id="pane-note">{{ meta.note }}</span>
    <div class="spacer"></div>
    <div style="min-width: 250px">
      <label for="token" class="dim" style="font-size: 11px">管理令牌 ADMIN_TOKEN</label>
      <!-- type=password + v-model：明文只在 DOM 属性里（property，不是序列化 attribute），
           page.content() 抓不到，符合 FR-094 的"不回显"。 -->
      <input
        id="token"
        v-model="auth.token"
        type="password"
        placeholder="粘贴令牌后自动记住"
        autocomplete="off"
      />
    </div>
  </header>
</template>
