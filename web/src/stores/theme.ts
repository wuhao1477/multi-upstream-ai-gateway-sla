import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

/**
 * 存的是"选择"（含 system）而不是最终色，否则跟随系统就退化成一次性快照。
 *
 * ⚠️ `system` **没有按钮**，但它留着：没点过按钮的浏览器按系统偏好开，
 * 这是对的默认值。按钮只在 light / dark 之间切（一按一个明确的值）——
 * 三态按钮要求人先想清楚"跟随系统"和"浅色"此刻是不是一回事才敢点。
 */
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
  /** 此刻**实际**是深色吗。按钮显示的是它，不是 choice（system 时两者不同）。 */
  const dark = ref(false)

  function apply(): void {
    dark.value = choice.value === 'dark' || (choice.value === 'system' && mq.matches)
    document.documentElement.classList.toggle('dark', dark.value)
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

  /** 切到"当前不是的那个"。system 状态下按一次就落到一个明确值上。 */
  function toggle(): void {
    set(dark.value ? 'light' : 'dark')
  }

  return { choice, dark: computed(() => dark.value), set, toggle }
})
