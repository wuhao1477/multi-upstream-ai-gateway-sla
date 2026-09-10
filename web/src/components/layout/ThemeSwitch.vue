<script setup lang="ts">
import { useThemeStore } from '@/stores/theme'
import type { ThemeChoice } from '@/stores/theme'

const theme = useThemeStore()

const OPTIONS: Array<{ value: ThemeChoice; title: string }> = [
  { value: 'light', title: '浅色' },
  { value: 'dark', title: '深色' },
  { value: 'system', title: '跟随系统' },
]
</script>

<template>
  <div class="theme-sw" id="theme-sw">
    <!-- aria-pressed 既是可访问性属性，也是验收脚本读按钮态的依据 -->
    <button
      v-for="o in OPTIONS"
      :key="o.value"
      :data-theme="o.value"
      :title="o.title"
      :aria-pressed="theme.choice === o.value"
      @click="theme.set(o.value)"
    >
      <svg
        v-if="o.value === 'light'"
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
      <svg
        v-else-if="o.value === 'dark'"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
      >
        <path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z" />
      </svg>
      <svg
        v-else
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
      >
        <rect x="2" y="3" width="20" height="14" rx="2" />
        <path d="M8 21h8M12 17v4" />
      </svg>
    </button>
  </div>
</template>
