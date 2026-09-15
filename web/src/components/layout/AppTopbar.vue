<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import type { PaneName } from '@/router/panes'
import { PANES } from '@/router/panes'
const route = useRoute()
const auth = useAuthStore()

const meta = computed(() => {
  // 模型目录：标题与说明都交给页面自己（它有个居中的大标题），这里只留令牌。
  if (route.name === 'models') return { title: '', note: '' }
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
  <header class="topbar">
    <!-- 模型目录那一页自己有大标题与一句话（照上游定价页的形状），
         这里再印一遍就是同一句话出现两次。令牌输入框照旧全页都在 ——
         它是全站凭证，哪一页都可能要现填。 -->
    <h1 id="pane-title" v-if="meta.title !== ''">{{ meta.title }}</h1>
    <span class="badge" id="pane-note" v-if="meta.note !== ''">{{ meta.note }}</span>
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
