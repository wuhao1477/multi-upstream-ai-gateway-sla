import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import { setUnauthorizedHandler } from './api/client'
import { useAuthStore } from './stores/auth'
import './styles/app.css'

const app = createApp(App).use(createPinia()).use(router)

/**
 * 令牌失效的统一出口：清掉它，回登录页，并记住人本来在哪一页。
 *
 * 清掉是必须的 —— 留着一把已经不灵的令牌，守卫会认为"有令牌"并放行，
 * 于是每进一页都是先加载、再 401、再弹回来，一个死循环。
 *
 * 已经在登录页就什么都不做：并发的几个请求会各自 401 一次，
 * 不拦的话是连着几次到同一个地址的 replace（vue-router 会打警告）。
 */
setUnauthorizedHandler(() => {
  useAuthStore().set('')
  const cur = router.currentRoute.value
  if (cur.name !== 'login') {
    void router.replace({ name: 'login', query: { next: cur.fullPath } })
  }
})

app.mount('#app')
