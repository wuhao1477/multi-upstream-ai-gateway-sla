import { defineStore } from 'pinia'
import { ref } from 'vue'

export type ToastKind = 'ok' | 'bad' | 'info'

/** 全局提示。同一时刻只有一条 —— 多条堆叠在这个面板里没有必要。 */
export const useToastStore = defineStore('toast', () => {
  const message = ref('')
  const kind = ref<ToastKind>('info')
  const visible = ref(false)
  let timer: ReturnType<typeof setTimeout> | undefined

  function show(msg: string, k: ToastKind = 'info'): void {
    message.value = msg
    kind.value = k
    visible.value = true
    clearTimeout(timer)
    timer = setTimeout(() => {
      visible.value = false
    }, 6000)
  }

  /** 失败提示的统一入口：把 Error 的 message 拼到前缀后面。 */
  function fail(prefix: string, e: unknown): void {
    const msg = e instanceof Error ? e.message : String(e)
    show(`${prefix}：${msg}`, 'bad')
  }

  return { message, kind, visible, show, fail }
})
