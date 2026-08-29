import { defineStore } from 'pinia'
import { ref } from 'vue'

/** 存的是"选择"（含 system）而不是最终色，否则跟随系统就退化成一次性快照。 */
export type ThemeChoice = 'light' | 'dark' | 'system'

const KEY = 'theme'
const mq = window.matchMedia('(prefers-color-scheme: dark)')

function read(): ThemeChoice {
  try {
    const v = localStorage.getItem(KEY)
    return v === 'light' || v === 'dark' || v === 'system' ? v : 'system'
  } catch {
    return 'system'
  }
}

export const useThemeStore = defineStore('theme', () => {
  const choice = ref<ThemeChoice>(read())

  function apply(): void {
    const dark = choice.value === 'dark' || (choice.value === 'system' && mq.matches)
    document.documentElement.classList.toggle('dark', dark)
  }

  function set(t: ThemeChoice): void {
    choice.value = t
    try {
      localStorage.setItem(KEY, t)
    } catch {
      /* 隐私模式：本次会话生效，下次开还是默认 */
    }
    apply()
  }

  // index.html 里的内联脚本已经在上色前打过一次类，这里再 apply 一次是为了
  // 让 store 与 DOM 同源（且内联脚本失败时兜底）。
  apply()
  mq.addEventListener('change', () => {
    if (choice.value === 'system') apply()
  })

  return { choice, set }
})
