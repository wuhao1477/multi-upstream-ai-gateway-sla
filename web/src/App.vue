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
  // toggle 不冒泡，所以只能用捕获阶段 —— 捕获会从 document 一路下到 target，
  // 非冒泡事件照样经过。
  document.addEventListener('toggle', placeMenus, true)
  // 滚动同样用捕获：真正滚的是 .tw / .drawer-b 这些内层容器，
  // 它们的 scroll 事件不冒泡到 window。
  window.addEventListener('scroll', placeMenus, true)
  window.addEventListener('resize', placeMenus)
})
onBeforeUnmount(() => {
  document.removeEventListener('click', closeMenus, true)
  document.removeEventListener('toggle', placeMenus, true)
  window.removeEventListener('scroll', placeMenus, true)
  window.removeEventListener('resize', placeMenus)
})

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

/** 菜单与视口边缘之间留的余量，两个方向同一个值。 */
const MENU_GAP = 8

/**
 * 给已展开的「更多」菜单定位。
 *
 * 菜单原来是 `position:absolute`，而它每一层祖先都可能是裁剪上下文 ——
 * 表格容器 `.tw` 有 `overflow-x:auto`（宽表格要横向滚动，而 overflow-x:auto
 * 会让 overflow-y 也算成 auto），抽屉正文 `.drawer-b` 有 `overflow-y:auto`。
 * 菜单从 summary 往下展开，必然越过容器的下边缘，于是越靠底部的行露出的越少，
 * **最后一行点开等于什么都没有**。这就是「更多下拉被遮挡/隐藏」的根因，三处
 * 菜单（Key 行、渠道行、列设置）共用同一段 CSS，所以也只需在这里修一次。
 *
 * 改成 `position:fixed` 之后不再被任何 overflow 裁剪，代价是位置要自己算：
 * fixed 的包含块是视口，跟着页面滚动会漂，所以 toggle / scroll / resize
 * 三处都要重算。
 */
function placeMenus(): void {
  // 收起的菜单把落位痕迹清掉：位置是按"上次展开时按钮在哪"算的，留着的话
  // 下次展开会先按旧坐标闪一下（表格滚动过之后尤其明显）。顺带让
  // `style.left === ''` 成为"尚未落位"的可靠信号，验收脚本靠它等这一帧。
  for (const stale of document.querySelectorAll<HTMLElement>(
    'details.more:not([open]) .more-menu',
  )) {
    stale.style.cssText = ''
  }
  for (const d of document.querySelectorAll<HTMLDetailsElement>('details.more[open]')) {
    const menu = d.querySelector<HTMLElement>('.more-menu')
    const trigger = d.querySelector('summary')
    if (menu === null || trigger === null) continue
    // **先归零再量**。fixed 元素的宽度是"收缩到适合"，可用宽度 = 视口宽 − left
    // —— 也就是说量出来的宽度取决于我们把它放在哪。不归零就会量到一个偏窄的
    // 值（它此刻还贴在表格最右列，右边只剩几十像素），按那个值右对齐之后元素
    // 重新变宽、右边缘再次探出视口。这条踩过一次：菜单从"被表格裁掉"变成
    // "被视口裁掉"，看起来像没修好。归零后可用宽度是整个视口，量到的就是
    // max-content（由 CSS 的 max-width 封顶），之后落位不会再变。
    menu.style.left = '0px'
    menu.style.top = '0px'
    const width = menu.offsetWidth
    const height = menu.offsetHeight
    const at = trigger.getBoundingClientRect()
    // 右对齐触发按钮（菜单在最右一列，往左展开才不会出视口），
    // 再夹在视口两侧的余量之内 —— 窄屏下按钮本身就贴着右边缘。
    const maxLeft = window.innerWidth - width - MENU_GAP
    menu.style.left = `${Math.max(MENU_GAP, Math.min(at.right - width, maxLeft))}px`
    // 下方放不下就翻到按钮上方。表格最后几行必然放不下,而"翻上去"比
    // "贴着视口底部截断"好 —— 后者会把危险操作(删除)挤出可点范围。
    const below = at.bottom + 4
    const above = at.top - 4 - height
    const overflows = below + height > window.innerHeight - MENU_GAP
    menu.style.top = overflows && above >= MENU_GAP ? `${above}px` : `${below}px`
    // 落位完成才显形，见 CSS 里 .more-menu 的 visibility 注释。
    menu.style.visibility = 'visible'
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
