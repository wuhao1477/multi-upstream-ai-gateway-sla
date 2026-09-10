<script setup lang="ts">
/**
 * 侧栏导航图标。内联 SVG 而非图标库：管理面要在内网/离线环境能开，
 * 且六个图标不值得引一个依赖。线条取自 lucide（与参考仓库同源）。
 */
defineProps<{ name: 'channels' | 'detail' | 'accounts' | 'keys' | 'creds' | 'import' }>()
</script>

<template>
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
    <template v-if="name === 'channels'">
      <rect x="2" y="3" width="20" height="6" rx="2" />
      <rect x="2" y="15" width="20" height="6" rx="2" />
      <path d="M6 6h.01M6 18h.01" />
    </template>
    <template v-else-if="name === 'detail'">
      <path d="M3 3v18h18" />
      <path d="m7 15 4-5 3 3 4-6" />
    </template>
    <!-- 账号=人形（钱的归属方），Key=钥匙。两个对象在界面上到处并列出现，
         图标不同才能让人在扫侧栏时不用读字 -->
    <template v-else-if="name === 'accounts'">
      <path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2" />
      <circle cx="12" cy="7" r="4" />
    </template>
    <template v-else-if="name === 'keys'">
      <path d="m15.5 7.5 3 3L22 7l-3-3" />
      <circle cx="7.5" cy="15.5" r="5.5" />
      <path d="m11.5 11.5 8-8" />
    </template>
    <template v-else-if="name === 'creds'">
      <rect x="3" y="11" width="18" height="11" rx="2" />
      <path d="M7 11V7a5 5 0 0 1 10 0v4" />
    </template>
    <template v-else>
      <path d="M12 3v12" />
      <path d="m7 10 5 5 5-5" />
      <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
    </template>
  </svg>
</template>
