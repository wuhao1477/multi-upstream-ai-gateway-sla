import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'
import { getToken, setToken } from '@/api/client'

/**
 * 管理令牌。**只存 localStorage，不写服务端**（06 §1 的边界）。
 *
 * 每次输入都落盘（而不是等 change/blur）：运维粘贴完就去点按钮，
 * 中间不一定发生失焦；而落盘是幂等的，多写几次没有代价。
 */
export const useAuthStore = defineStore('auth', () => {
  const token = ref(getToken())
  const hasToken = computed(() => token.value.trim() !== '')

  watch(token, (v) => setToken(v.trim()))

  return { token, hasToken }
})
