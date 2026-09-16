<script setup lang="ts">
/**
 * 主题开关：**一个按钮**，在浅色与深色之间切。
 *
 * 原先是三段（浅色 / 深色 / 跟随系统）。三段的问题不是占地方，是要人先想清楚
 * "跟随系统"此刻等于哪一个才敢点 —— 而这个开关要回答的只是"现在太亮/太暗"。
 * 「跟随系统」作为**没点过时的默认**留着（见 stores/theme.ts），按一次就落到
 * 一个明确的值上。
 *
 * 图标画的是**当前**是什么（太阳 = 现在浅色），title 说的是按下去会变成什么 ——
 * 两者必须都在：只画当前的话，人不知道按了会怎样；只画目标的话，人会把它读成
 * 当前状态（这是这类按钮最常见的误读）。
 */
import { useThemeStore } from '@/stores/theme'

const theme = useThemeStore()
</script>

<template>
  <!-- data-theme 是**当前**主题，验收脚本据此判断切没切过去 -->
  <button
    id="theme-sw"
    type="button"
    class="theme-btn"
    :data-theme="theme.dark ? 'dark' : 'light'"
    :title="theme.dark ? '切换到浅色' : '切换到深色'"
    :aria-label="theme.dark ? '切换到浅色' : '切换到深色'"
    @click="theme.toggle()"
  >
    <svg
      v-if="!theme.dark"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
    >
      <circle cx="12" cy="12" r="4" />
      <path
        d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M6.3 17.7l-1.4 1.4M19.1 4.9l-1.4 1.4"
      />
    </svg>
    <svg v-else viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
      stroke-linecap="round" stroke-linejoin="round">
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
    </svg>
  </button>
</template>
