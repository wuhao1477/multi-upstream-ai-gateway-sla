<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import NavIcon from './NavIcon.vue'
import ThemeSwitch from './ThemeSwitch.vue'
import { getVersion } from '@/api/admin'
import { useAuthStore } from '@/stores/auth'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import type { PaneName } from '@/router/panes'
import { PANE_ORDER, PANES } from '@/router/panes'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const channels = useChannelsStore()
const res = useResourcesStore()

/**
 * 后端版本。空串 = 还没拿到，此时**不渲染**这一段。
 *
 * 两个触发点，没有第三个：
 *  · onMounted —— 令牌已经存在 localStorage 里时首屏就有（同 App.vue 的做法）。
 *  · `res.loaded` 翻真 —— 刚填完令牌的那一次补上。
 *
 * ⚠️ **不要改成盯 `auth.token` 或 `auth.hasToken`**：令牌是一个字符一个字符
 * 敲进输入框的，每敲一下就打一次请求 = 一串半截令牌换回一串 401。那些 401
 * 是控制台里真实的失败请求，会把真正该看的错误淹掉（界面验收的「页面无
 * JavaScript 错误」那条就是这么红的）。`res.loaded` 只在**成功**拉过一轮
 * 之后才为真，拿它当"令牌能用了"的信号，按定义不会打在错令牌上。
 *
 * 失败一律吞掉退回空串：版本号显示不出来不该在界面上变成一条错误。
 */
const version = ref('')
function loadVersion(): void {
  if (!auth.hasToken) return
  getVersion().then(
    (v) => (version.value = v.version),
    () => (version.value = ''),
  )
}
onMounted(loadVersion)
watch(
  () => res.loaded,
  (ok) => {
    if (ok) loadVersion()
  },
)

/** 角标：各分栏的规模。空值渲染成空串而不是 0 —— "还没拉"和"确实是 0"不一样。 */
function badge(p: PaneName): string {
  if (p === 'channels') return channels.count > 0 ? String(channels.count) : ''
  if (p === 'accounts') return res.accounts.length > 0 ? String(res.accounts.length) : ''
  if (p === 'keys') return res.keys.length > 0 ? String(res.keys.length) : ''
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

function isActive(p: PaneName): boolean {
  return route.name === p || route.meta.parent === p
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
        <!-- 版本跟着交付阶段走：两者回答的是同一个问题（"这是哪一版"），
             分开放会让人只看到其中一个。窄屏下 .side-foot-l 会被隐藏，
             而 brand 一直在，所以版本放这儿而不是侧栏底部。 -->
        <div class="brand-s">
          交付阶段 P1<template v-if="version !== ''">
            ·
            <span data-app-version :title="`正在运行的 sla-core 版本：${version}`">{{
              version
            }}</span>
          </template>
        </div>
      </div>
    </div>

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

    <div class="side-foot">
      <div class="side-foot-l">外观</div>
      <ThemeSwitch />
    </div>
  </aside>
</template>
