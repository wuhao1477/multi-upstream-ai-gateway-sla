import { createRouter, createWebHistory } from 'vue-router'
import ChannelsView from '@/views/ChannelsView.vue'
import DetailView from '@/views/DetailView.vue'
import AccountsView from '@/views/AccountsView.vue'
import KeysView from '@/views/KeysView.vue'
import CredsView from '@/views/CredsView.vue'
import ImportView from '@/views/ImportView.vue'

/**
 * history 模式而非 hash：分栏可以直接分享链接（"打开 #12 的详情"）。
 *
 * 代价是后端要为 /admin/ui/* 全部回 index.html —— web.go 里有那条兜底路由。
 * base 用 '/admin/ui/'，vue-router 会把不带尾斜杠的 /admin/ui 也归到根路由，
 * 所以两个入口都能开。
 *
 * 六个视图都是静态 import（不做懒加载）：ESM 的 <script type="module"> 是
 * defer 语义，DOMContentLoaded 会等它执行完 —— 单块产物才能保证验收脚本
 * 的 waitUntil:'domcontentloaded' 返回时页面已经挂载好。懒加载会把这一点
 * 变成竞态，而这个界面总共几十 KB，分包省不下什么。
 */
const router = createRouter({
  history: createWebHistory('/admin/ui/'),
  routes: [
    { path: '/', name: 'channels', component: ChannelsView },
    { path: '/detail', name: 'detail', component: DetailView },
    { path: '/accounts', name: 'accounts', component: AccountsView },
    { path: '/keys', name: 'keys', component: KeysView },
    // 旧入口。「账号与 Key」拆成了两个分栏，收藏夹里的链接不该变成 404 ——
    // 账号是它原来的主要内容，所以落到账号页。
    { path: '/register', redirect: '/accounts' },
    { path: '/creds', name: 'creds', component: CredsView },
    { path: '/import', name: 'import', component: ImportView },
    // 未知路径回渠道列表，而不是留个空白页
    { path: '/:rest(.*)', redirect: '/' },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

export default router
