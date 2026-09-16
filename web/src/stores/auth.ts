import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { getToken, setToken } from '@/api/client'

/**
 * 管理令牌。**只存 localStorage，不写服务端**（06 §1 的边界）。
 *
 * ⚠️ 落盘走 `set()` 而不是 `watch(token, …)`。watch 的回调是**异步**的
 * （Vue 默认 pre-flush），而登录页"存下令牌 → 立刻拿它打一个请求验一下"是
 * 紧挨着的两步 —— 那一刻 localStorage 里还是旧值，于是 api() 读到空令牌、
 * 判成"没登录"、把人弹回登录页，错误信息还写着"请先填入 ADMIN_TOKEN"，
 * 而人刚刚就是填了它。2026-09-16 被界面验收逮住的。
 */
export const useAuthStore = defineStore('auth', () => {
  const token = ref(getToken())
  const hasToken = computed(() => token.value.trim() !== '')

  /** 存令牌（空串 = 登出）。同步落盘，下一行就能拿它发请求。 */
  function set(v: string): void {
    const t = v.trim()
    token.value = t
    setToken(t)
  }

  return { token, hasToken, set }
})
