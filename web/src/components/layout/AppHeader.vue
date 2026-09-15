<script setup lang="ts">
/**
 * 顶层导航栏：左边品牌，右边「控制台 / 模型目录」。
 *
 * 形态取自一个真实 NewAPI 站点的站点顶栏 —— 那里也是品牌在左、
 * 页面切换在右、整条居中。这不是纯装饰：**顶层只有两项**，而那两项回答的是
 * 两个不同的问题（管上游资产 vs 查一个模型哪些渠道有）。原先把「模型目录」
 * 摆在侧栏里，它看起来像第四种"要管的对象"，而它谁也不管。
 *
 * 版本号跟着品牌走（原先在侧栏的 brand 里）：模型目录那一侧没有侧栏，
 * 版本号留在侧栏等于"换个页面就不知道跑的是哪一版"。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import ThemeSwitch from './ThemeSwitch.vue'
import { getVersion } from '@/api/admin'
import { useAuthStore } from '@/stores/auth'
import { useResourcesStore } from '@/stores/resources'
import type { PaneName, TopNav } from '@/router/panes'
import { PANES, TOP_NAV } from '@/router/panes'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const res = useResourcesStore()

/**
 * 后端版本。空串 = 还没拿到，此时**不渲染**这一段。
 *
 * 两个触发点：
 *  · onMounted —— 顶栏只在登录之后才挂载，所以这一次必然已经有令牌。
 *  · `res.loaded` 翻真 —— 兜底（令牌刚好在这一刻被换掉时补上）。
 *
 * ⚠️ 不要改成盯 `auth.token`：**曾经**因此炸过一次 —— 令牌那时绑在顶栏的输入框上，
 * 一个字符一个字符地敲，每敲一下就是一串半截令牌换回一个 401，把控制台里
 * 真正该看的错误全淹掉（界面验收的「页面无 JavaScript 错误」那条就是这么红的）。
 * 令牌现在只在登录页整个提交一次，那个坑不复存在，但这里也没有再盯它的理由。
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

/** 当前落在哪个顶层导航下。渠道详情是渠道管理的二级页，归控制台。 */
const active = computed<TopNav>(() => {
  const name = typeof route.name === 'string' ? route.name : ''
  const parent = typeof route.meta.parent === 'string' ? route.meta.parent : ''
  const key = (name in PANES ? name : parent) as PaneName
  return key in PANES ? PANES[key].top : 'console'
})

/**
 * 切顶层导航。回的是那一侧的**入口分栏**，不是记忆中的上一个位置 ——
 * 记忆听起来体贴，但"点控制台回到哪"取决于上次离开时在哪，而那是看不见的。
 */
function go(route_: PaneName): void {
  void router.push({ name: route_ })
}
</script>

<template>
  <header class="hdr">
    <div class="hdr-in">
      <div class="brand">
        <div class="brand-mark">SLA</div>
        <div>
          <div class="brand-t">上游采集与管理</div>
          <!-- 版本跟着交付阶段走：两者回答的是同一个问题（"这是哪一版"），
               分开放会让人只看到其中一个。 -->
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

      <nav class="topnav" aria-label="顶层导航">
        <button
          v-for="t in TOP_NAV"
          :key="t.key"
          class="tnav"
          :class="{ on: active === t.key }"
          :data-top-nav="t.key"
          :aria-current="active === t.key ? 'page' : undefined"
          :title="t.note"
          @click="go(t.route)"
        >
          {{ t.label }}
        </button>
      </nav>

      <!-- 令牌不在这儿了：它只在登录页填一次，之后存在浏览器里（见 LoginView）。
           摆在顶栏上的输入框有一个说不通的地方 —— 那是一把已经生效的凭证，
           却长得像一个还等着你填的表单项。 -->
      <ThemeSwitch />
    </div>
  </header>
</template>
