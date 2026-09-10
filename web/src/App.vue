<script setup lang="ts">
import { onBeforeUnmount, onMounted } from 'vue'
import AppSidebar from '@/components/layout/AppSidebar.vue'
import AppTopbar from '@/components/layout/AppTopbar.vue'
import ToastHost from '@/components/layout/ToastHost.vue'
import { useAuthStore } from '@/stores/auth'
import { useChannelsStore } from '@/stores/channels'
import { useCredentialsStore } from '@/stores/credentials'
import { useResourcesStore } from '@/stores/resources'

const auth = useAuthStore()
const channels = useChannelsStore()
const creds = useCredentialsStore()
const res = useResourcesStore()

// 已经存过令牌就直接拉一轮：刷新后还要再点一次「刷新」是多余的一步。
// 没有令牌则什么都不做 —— 空发请求只会换回 401，把真正该看的提示挤掉。
//
// 账号与 Key 也在这里拉：渠道列表的「账号 / Key / 资金概览」三列读的就是它们，
// 懒加载的话首屏那三列全是「—」，看起来像后端没返回数据。三个请求换一屏
// 有意义的台账，值。
onMounted(() => {
  if (auth.hasToken) {
    void channels.load()
    void creds.load()
    void res.load()
  }
  document.addEventListener('click', closeMenus, true)
})
onBeforeUnmount(() => document.removeEventListener('click', closeMenus, true))

/**
 * 行内「更多」菜单（原生 <details>）的点击外部关闭。
 *
 * 原生 details 没有这个行为：点开一个、再点开下一个，屏幕上会挂着两个菜单，
 * 而它们长得一样、都盖在表格上。一个全局监听比给每个菜单挂 blur 少得多，
 * 也不需要引一个 popover 库。
 *
 * 用捕获阶段：菜单项自己的 @click 在冒泡阶段跑，这里先跑一步把**别的**菜单
 * 关掉，不影响被点中的那个继续处理。
 */
function closeMenus(e: MouseEvent): void {
  const inside = (e.target as HTMLElement | null)?.closest('details.more')
  for (const d of document.querySelectorAll<HTMLDetailsElement>('details.more[open]')) {
    if (d !== inside) d.open = false
  }
}
</script>

<template>
  <div class="shell">
    <AppSidebar />
    <div class="main">
      <AppTopbar />
      <div class="content">
        <RouterView />
      </div>
    </div>
  </div>
  <ToastHost />
</template>
