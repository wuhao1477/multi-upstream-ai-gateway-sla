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
 * 两个触发点，没有第三个：
 *  · onMounted —— 令牌已经存在 localStorage 里时首屏就有。
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

      <ThemeSwitch />

      <!-- 令牌在顶栏而不是分栏顶栏：它是**全站**凭证，每一页都要用。
           原先摆在分栏顶栏里，于是模型目录那一页为了它单独挂一条几乎全空的
           横条 —— 那条横条上除了这个框什么都没有。 -->
      <label class="sr" for="token">管理令牌 ADMIN_TOKEN</label>
      <!-- type=password + v-model：明文只在 DOM 属性里（property，不是序列化 attribute），
           page.content() 抓不到，符合 FR-094 的"不回显"。 -->
      <input
        id="token"
        class="hdr-token"
        v-model="auth.token"
        type="password"
        placeholder="ADMIN_TOKEN"
        autocomplete="off"
        title="管理令牌 ADMIN_TOKEN，粘贴后自动记住"
      />
    </div>
  </header>
</template>
